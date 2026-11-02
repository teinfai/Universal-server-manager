package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teinfai/Universal-server-manager/internal/config"
	"github.com/teinfai/Universal-server-manager/internal/remote"
	"gopkg.in/yaml.v3"
)

const stopsURL = "https://gitlab.com/gitlab-org/gitlab/-/raw/master/config/upgrade_path.yml"

type Version struct {
	Full    string
	Base    string
	Edition string
	Numbers [4]int64
}

type Stop struct{ Major, Minor int64 }

func ParseVersion(full string) (Version, error) {
	v := Version{Full: full}
	if err := config.Validate("version", full); err != nil {
		return v, err
	}
	parts := strings.Split(full, "-")
	v.Base = parts[0]
	revision := strings.Split(parts[1], ".")
	v.Edition = revision[0]
	numbers := append(strings.Split(v.Base, "."), revision[1])
	for i, text := range numbers {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return v, err
		}
		v.Numbers[i] = n
	}
	return v, nil
}

func compare(a, b Version) int {
	for i := range a.Numbers {
		if a.Numbers[i] < b.Numbers[i] {
			return -1
		}
		if a.Numbers[i] > b.Numbers[i] {
			return 1
		}
	}
	return 0
}

func comparePair(a, b Stop) int {
	if a.Major < b.Major || (a.Major == b.Major && a.Minor < b.Minor) {
		return -1
	}
	if a == b {
		return 0
	}
	return 1
}

func pair(v Version) Stop { return Stop{v.Numbers[0], v.Numbers[1]} }

func Path(current Version, versions []Version, stops []Stop) ([]Version, error) {
	var available []Version
	seen := make(map[string]bool)
	for _, version := range versions {
		if version.Edition != current.Edition {
			return nil, errors.New("package edition mismatch")
		}
		if !seen[version.Full] && compare(version, current) > 0 {
			available = append(available, version)
		}
		seen[version.Full] = true
	}
	if len(available) == 0 {
		return nil, nil
	}
	sort.SliceStable(available, func(i, j int) bool { return compare(available[i], available[j]) < 0 })
	target := available[len(available)-1]
	largest := func(stop Stop) (Version, bool) {
		for i := len(available) - 1; i >= 0; i-- {
			if pair(available[i]) == stop {
				return available[i], true
			}
		}
		return Version{}, false
	}
	ordered := append([]Stop(nil), stops...)
	sort.SliceStable(ordered, func(i, j int) bool { return comparePair(ordered[i], ordered[j]) < 0 })
	var path []Version
	for _, stop := range ordered {
		if stop == pair(current) {
			if version, ok := largest(stop); ok && version.Numbers[2] > current.Numbers[2] {
				path = append(path, version)
			}
			break
		}
	}
	for _, stop := range ordered {
		if comparePair(stop, pair(current)) <= 0 || comparePair(stop, pair(target)) >= 0 {
			continue
		}
		version, ok := largest(stop)
		if !ok {
			return nil, fmt.Errorf("no package for required version %d.%d, check by hand", stop.Major, stop.Minor)
		}
		path = append(path, version)
	}
	if len(path) == 0 || path[len(path)-1].Full != target.Full {
		path = append(path, target)
	}
	return path, nil
}

func Sudo(r *remote.Client) error {
	_, err := r.Exec(context.Background(), "sudo -n true")
	return err
}

func Package(r *remote.Client) (string, Version, error) {
	var pkg string
	var current Version
	installed := 0
	var versionErr error
	for _, candidate := range []string{"gitlab-ce", "gitlab-ee"} {
		if err := config.Validate("package", candidate); err != nil {
			return "", current, err
		}
		output, err := r.Exec(context.Background(), "dpkg-query -W -f='${Status} ${Version}' "+candidate)
		if err != nil || !strings.HasPrefix(output, "install ok installed ") {
			continue
		}
		installed++
		pkg = candidate
		current, err = ParseVersion(strings.TrimPrefix(output, "install ok installed "))
		if err != nil {
			versionErr = err
		} else if "gitlab-"+current.Edition != candidate {
			versionErr = errors.New("installed package edition mismatch")
		}
	}
	if installed != 1 {
		return "", current, fmt.Errorf("expected one GitLab package, found %d", installed)
	}
	if versionErr != nil {
		return "", current, versionErr
	}
	return pkg, current, nil
}

func Installed(r *remote.Client, pkg string, version Version) error {
	if err := config.Validate("package", pkg); err != nil {
		return err
	}
	if err := config.Validate("version", version.Full); err != nil {
		return err
	}
	output, err := r.Exec(context.Background(), "dpkg-query -W -f='${Version}' "+pkg)
	if err != nil {
		return err
	}
	if output != version.Full {
		return errors.New("installed version does not match requested version")
	}
	return nil
}

func Services(r *remote.Client) error {
	output, err := r.Exec(context.Background(), "sudo gitlab-ctl status")
	if err != nil {
		return err
	}
	running := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "down:") {
			return errors.New("a GitLab service is down")
		}
		if strings.HasPrefix(line, "run:") {
			running = true
		}
	}
	if !running {
		return errors.New("no running GitLab service found")
	}
	return nil
}

func Background(r *remote.Client) error {
	output, err := r.Exec(context.Background(), `sudo gitlab-psql -t -A -c "SELECT count(*) FROM batched_background_migrations WHERE status NOT IN (3, 6);"`)
	if err != nil {
		return err
	}
	output = strings.TrimSpace(output)
	if err := config.Validate("digits", output); err != nil {
		return err
	}
	if output != "0" {
		return errors.New("unfinished background migrations")
	}
	return nil
}

func Madison(output string, current Version) ([]Version, error) {
	var versions []Version
	found := false
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			return nil, errors.New("invalid madison line")
		}
		version, err := ParseVersion(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, err
		}
		if version.Edition != current.Edition {
			return nil, errors.New("madison package edition mismatch")
		}
		if version.Full == current.Full {
			found = true
		}
		if !seen[version.Full] {
			versions = append(versions, version)
			seen[version.Full] = true
		}
	}
	if !found {
		return nil, errors.New("installed package version is absent from apt")
	}
	return versions, nil
}

func Versions(r *remote.Client, pkg string, current Version) ([]Version, error) {
	if err := config.Validate("package", pkg); err != nil {
		return nil, err
	}
	if _, err := r.Exec(context.Background(), "sudo apt-get update -qq"); err != nil {
		return nil, err
	}
	output, err := r.Exec(context.Background(), "apt-cache madison "+pkg)
	if err != nil {
		return nil, err
	}
	return Madison(output, current)
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func Stops() ([]Stop, error) {
	response, err := httpClient().Get(stopsURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upgrade path HTTP %d", response.StatusCode)
	}
	d := yaml.NewDecoder(response.Body)
	var document yaml.Node
	if err := d.Decode(&document); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("upgrade path must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.SequenceNode || len(document.Content[0].Content) == 0 {
		return nil, errors.New("upgrade path must be a nonempty YAML list")
	}
	var stops []Stop
	for _, item := range document.Content[0].Content {
		if item.Kind != yaml.MappingNode {
			return nil, errors.New("upgrade stop must be a map")
		}
		var stop Stop
		seen := make(map[string]bool)
		for i := 0; i < len(item.Content); i += 2 {
			key, value := item.Content[i], item.Content[i+1]
			if key.Tag != "!!str" || seen[key.Value] {
				return nil, errors.New("invalid upgrade stop key")
			}
			seen[key.Value] = true
			switch key.Value {
			case "major", "minor":
				if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
					return nil, errors.New("upgrade stop version must use integers")
				}
				var number int64
				if err := value.Decode(&number); err != nil {
					return nil, err
				}
				if key.Value == "major" {
					stop.Major = number
				} else {
					stop.Minor = number
				}
			case "comments":
				if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					return nil, errors.New("upgrade stop comments must be a string")
				}
			default:
				return nil, errors.New("unexpected upgrade stop field")
			}
		}
		if !seen["major"] || !seen["minor"] {
			return nil, errors.New("upgrade stop needs major and minor")
		}
		stops = append(stops, stop)
	}
	return stops, nil
}

type LowDisk struct{ Need, Free *big.Int }

func (e *LowDisk) Error() string { return "need " + e.Text() }

func (e *LowDisk) Text() string { return gb(e.Need) + " GB free, have " + gb(e.Free) + " GB" }

func gb(bytes *big.Int) string {
	f := new(big.Float).Quo(new(big.Float).SetInt(bytes), big.NewFloat(1073741824))
	return f.Text('f', 1)
}

func byteCount(text string) (*big.Int, error) {
	if err := config.Validate("digits", text); err != nil {
		return nil, err
	}
	number, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, errors.New("invalid byte count")
	}
	return number, nil
}

func Disk(r *remote.Client, dir string) error {
	if err := config.Validate("backup dir", dir); err != nil {
		return err
	}
	output, err := r.Exec(context.Background(), "sudo df -B1 --output=avail "+dir)
	if err != nil {
		return err
	}
	last := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			last = strings.TrimSpace(line)
		}
	}
	free, err := byteCount(last)
	if err != nil {
		return err
	}
	size := func(path string) (*big.Int, error) {
		if err := config.Validate("backup dir", path); err != nil {
			return nil, err
		}
		output, err := r.Exec(context.Background(), "sudo du -sb "+path)
		if err != nil {
			return nil, err
		}
		return byteCount(strings.SplitN(strings.SplitN(output, "\n", 2)[0], "\t", 2)[0])
	}
	used, err := size("/var/opt/gitlab")
	if err != nil {
		return err
	}
	if strings.HasPrefix(dir, "/var/opt/gitlab/") {
		backups, err := size(dir)
		if err != nil {
			return err
		}
		used.Sub(used, backups)
	}
	if used.Sign() < 0 {
		return errors.New("backup size exceeds GitLab size")
	}
	if free.Cmp(used) <= 0 {
		return &LowDisk{Need: used, Free: free}
	}
	return nil
}

var readyHostPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

func Ready(r *remote.Client, gitlabURL string) error {
	u, err := url.Parse(gitlabURL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("invalid GitLab URL scheme")
	}
	host := u.Hostname()
	if !readyHostPattern.MatchString(host) {
		return errors.New("invalid GitLab URL host")
	}
	portText := u.Port()
	if portText == "" {
		portText = "80"
		if u.Scheme == "https" {
			portText = "443"
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid GitLab URL port")
	}
	hostPort := fmt.Sprintf("%s:%d", host, port)
	command := fmt.Sprintf("curl -sk --max-time 30 --resolve %s:127.0.0.1 %s://%s/-/readiness", hostPort, u.Scheme, hostPort)
	output, err := r.Exec(context.Background(), command)
	if err != nil {
		return err
	}
	var result struct{ Status string }
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return fmt.Errorf("GitLab readiness: %w", err)
	}
	if result.Status != "ok" {
		return fmt.Errorf("GitLab readiness: %s", result.Status)
	}
	return nil
}
