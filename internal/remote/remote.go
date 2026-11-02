package remote

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/teinfai/Universal-server-manager/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

//go:embed usm-step.sh
var script string

type Result struct {
	Stdout string
	Stderr string
	Exit   int
}

type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

type Client struct {
	Config config.SSH
	Logf   func(string)
	client *ssh.Client
	wire   net.Conn
}

func (c *Client) Connect(ctx context.Context) error {
	c.Close()
	key, err := os.ReadFile(c.Config.KeyFile)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("SSH private key: %w", err)
	}
	callback, err := knownhosts.New(c.Config.KnownHostsFile)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(c.Config.Host, strconv.Itoa(c.Config.Port))
	dialer := net.Dialer{Timeout: 30 * time.Second}
	wire, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := wire.SetDeadline(deadline); err != nil {
		wire.Close()
		return err
	}
	connection, channels, requests, err := ssh.NewClientConn(wire, address, &ssh.ClientConfig{
		User: c.Config.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback,
	})
	if err != nil {
		wire.Close()
		return err
	}
	if err := wire.SetDeadline(time.Time{}); err != nil {
		connection.Close()
		return err
	}
	c.wire = wire
	c.client = ssh.NewClient(connection, channels, requests)
	return nil
}

func (c *Client) Close() {
	if c.client != nil {
		c.client.Close()
		c.client = nil
		c.wire = nil
	}
}

func (c *Client) Run(ctx context.Context, command string, input io.Reader) (result Result, err error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Minute)
		defer cancel()
	}
	result.Exit = -1
	defer func() {
		c.Logf("SSH command: " + command)
		c.Logf(fmt.Sprintf("SSH exit: %d", result.Exit))
		c.Logf("SSH stdout: " + result.Stdout)
		c.Logf("SSH stderr: " + result.Stderr)
		if err != nil {
			c.Logf("SSH error: " + err.Error())
		}
	}()
	if c.client == nil {
		return result, &TransportError{errors.New("SSH is not connected")}
	}
	if err := ctx.Err(); err != nil {
		return result, &TransportError{err}
	}
	deadline, _ := ctx.Deadline()
	if err := c.wire.SetDeadline(deadline); err != nil {
		return result, &TransportError{err}
	}
	defer c.wire.SetDeadline(time.Time{})
	session, err := c.client.NewSession()
	if err != nil {
		return result, &TransportError{err}
	}
	defer session.Close()
	var stdout, stderr strings.Builder
	session.Stdout, session.Stderr, session.Stdin = &stdout, &stderr, input
	err = session.Run(command)
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	if err == nil {
		result.Exit = 0
		return result, nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		result.Exit = exit.ExitStatus()
		return result, err
	}
	return result, &TransportError{err}
}

func (c *Client) Exec(ctx context.Context, command string) (string, error) {
	r, err := c.Run(ctx, command, nil)
	return r.Stdout, err
}

func (c *Client) WriteScript(ctx context.Context) error {
	if _, err := c.Run(ctx, "sudo tee /usr/local/sbin/usm-step.sh >/dev/null", strings.NewReader(script)); err != nil {
		return err
	}
	_, err := c.Exec(ctx, "sudo chmod 0755 /usr/local/sbin/usm-step.sh")
	return err
}

func (c *Client) Poll(ctx context.Context, id string) (string, error) {
	if err := config.Validate("step id", id); err != nil {
		return "", err
	}
	output, err := c.Exec(ctx, fmt.Sprintf("sudo sh -c 'if [ -f /var/lib/usm/%s.status ]; then cat /var/lib/usm/%s.status; elif systemctl is-active --quiet usm-%s; then echo running; else echo gone; fi'", id, id, id))
	return strings.TrimSpace(output), err
}

func Terminal(output string) bool {
	return output == "gone" || config.Validate("digits", output) == nil
}

func (c *Client) Step(action string, args []string, location *time.Location, saveActive func(string) error) error {
	if err := config.Validate("action", action); err != nil {
		return err
	}
	kinds := map[string][]string{
		"backup":  {"backup name", "backup dir"},
		"install": {"package", "version"},
		"restore": {"package", "version", "backup name", "backup dir", "etc tar"},
	}[action]
	if len(args) != len(kinds) {
		return errors.New("invalid step argument count")
	}
	for i, value := range args {
		if err := config.Validate(kinds[i], value); err != nil {
			return err
		}
	}
	var id string
	for attempt := 0; attempt < 2; attempt++ {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id = fmt.Sprintf("%s-%s-%x", action, time.Now().In(location).Format("20060102150405"), random)
		if err := config.Validate("step id", id); err != nil {
			return err
		}
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 30*time.Second)
		r, err := c.Run(checkCtx, fmt.Sprintf("sudo sh -c 'test ! -e /var/lib/usm/%s.status && test ! -e /var/log/usm/%s.log'", id, id), nil)
		checkCancel()
		if err == nil {
			break
		}
		if r.Exit < 0 {
			return err
		}
		if attempt == 1 {
			return errors.New("step id collision on both attempts")
		}
	}
	if err := saveActive(id); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	finish := func(err error) error {
		logCtx, logCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer logCancel()
		if _, logErr := c.Exec(logCtx, "sudo cat /var/log/usm/"+id+".log"); logErr != nil {
			c.Logf("remote step log: " + logErr.Error())
		}
		return err
	}
	r, err := c.Run(ctx, "sudo systemd-run --unit=usm-"+id+" --collect /usr/local/sbin/usm-step.sh "+id+" "+action+" "+strings.Join(args, " "), nil)
	if err != nil {
		if r.Exit >= 0 {
			if saveErr := saveActive(""); saveErr != nil {
				return saveErr
			}
		}
		return finish(err)
	}
	for {
		if err := wait(ctx, 10*time.Second); err != nil {
			return finish(errors.New("step exceeded six-hour deadline"))
		}
		pollCtx, pollCancel := context.WithTimeout(ctx, 30*time.Second)
		output, err := c.Poll(pollCtx, id)
		pollCancel()
		var transport *TransportError
		if errors.As(err, &transport) {
			for {
				if err := wait(ctx, 30*time.Second); err != nil {
					return finish(errors.New("step exceeded six-hour deadline"))
				}
				if err := c.Connect(ctx); err == nil {
					break
				} else {
					c.Logf("SSH reconnect: " + err.Error())
				}
			}
			continue
		}
		if err != nil {
			return finish(err)
		}
		if Terminal(output) {
			if err := saveActive(""); err != nil {
				return err
			}
			if output == "gone" {
				return finish(errors.New("step ended without a result"))
			}
			if strings.TrimLeft(output, "0") == "" {
				return finish(nil)
			}
			return finish(fmt.Errorf("step exited %s", output))
		}
		if output != "running" {
			return finish(errors.New("unexpected step status"))
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
