package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teinfai/Universal-server-manager/internal/config"
	"github.com/teinfai/Universal-server-manager/internal/gitlab"
	"github.com/teinfai/Universal-server-manager/internal/notify"
	"github.com/teinfai/Universal-server-manager/internal/remote"
	"github.com/teinfai/Universal-server-manager/internal/state"
)

type Job struct {
	config   *config.Config
	state    *state.State
	remote   *remote.Client
	api      gitlab.API
	telegram notify.Telegram
	log      *os.File
}

type localError struct {
	kind string
	err  error
}

func (e *localError) Error() string { return e.kind + ": " + e.err.Error() }

func New(c *config.Config) *Job {
	j := &Job{config: c, api: gitlab.API{URL: c.GitLab.URL, Token: c.GitLab.Token}, telegram: notify.Telegram{BotToken: c.Telegram.BotToken, ChatID: c.Telegram.ChatID}}
	j.remote = &remote.Client{Config: c.SSH, Logf: j.logf}
	return j
}

func (j *Job) now() time.Time { return time.Now().In(j.config.Location) }

func (j *Job) night(t time.Time) (time.Time, error) {
	start, err := time.Parse("15:04", j.config.Schedule.Start)
	if err != nil {
		return time.Time{}, &localError{"config", err}
	}
	night := time.Date(t.Year(), t.Month(), t.Day(), start.Hour(), start.Minute(), 0, 0, j.config.Location)
	if night.After(t) {
		night = night.AddDate(0, 0, -1)
	}
	return night, nil
}

func (j *Job) redact(text string) string {
	for _, secret := range []string{j.config.GitLab.Token, j.config.Telegram.BotToken} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

func (j *Job) logf(text string) {
	if j.log == nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSuffix(j.redact(text), "\n"), "\n") {
		if _, err := fmt.Fprintf(j.log, "%s %s\n", j.now().Format(time.RFC3339), line); err != nil {
			fmt.Fprintln(os.Stderr, "night log write:", err)
		}
	}
}

func (j *Job) say(text string) error {
	j.logf(text)
	err := j.telegram.Send(text)
	if err != nil {
		j.logf("Telegram send: " + err.Error())
	}
	return err
}

func (j *Job) openLog() error {
	file, err := os.OpenFile(filepath.Join(j.config.LogDir, j.now().Format("2006-01-02")+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return &localError{"log file", err}
	}
	j.log = file
	return nil
}

func (j *Job) Close() {
	j.remote.Close()
	if j.log != nil {
		if err := j.log.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "night log close:", err)
		}
	}
}

func (j *Job) local(err error) int {
	var failure *localError
	if !errors.As(err, &failure) {
		failure = &localError{"state file", err}
	}
	fmt.Fprintln(os.Stderr, j.redact(failure.Error()))
	j.logf(failure.Error())
	j.say("usm local error: " + failure.kind + ". Check the USM log.")
	return 1
}

func (j *Job) save() error {
	if err := state.Save(j.config.StateDir, j.state); err != nil {
		return &localError{"state file", err}
	}
	return nil
}

func (j *Job) saveActive(id string) error { j.state.ActiveStep = id; return j.save() }

func (j *Job) end(result, detail, message string) int {
	j.state.LastRun = state.Run{Time: j.now().Format(time.RFC3339), Result: result, Detail: j.redact(detail)}
	if err := j.save(); err != nil {
		return j.local(err)
	}
	if message != "" {
		j.say(message)
	}
	if result == "failed" {
		return 1
	}
	return 0
}

func (j *Job) skip(reason string, err error) int {
	if err != nil {
		j.logf(err.Error())
	}
	return j.end("skipped", reason, "Skipped: "+reason+". Try tomorrow.")
}

func (j *Job) connect(writeScript bool) error {
	if err := j.remote.Connect(context.Background()); err != nil {
		return err
	}
	if writeScript {
		return j.remote.WriteScript(context.Background())
	}
	return nil
}

func (j *Job) fail(action string, cause error) int {
	var local *localError
	if errors.As(cause, &local) {
		return j.local(cause)
	}
	j.logf(cause.Error())
	var message string
	switch action {
	case "Backup":
		message = "Failed at " + action + ". Nothing changed.\nWhen fixed: usm resume"
	case "Install", "Check":
		backupTime, err := time.Parse(time.RFC3339, j.state.LastBackup.Time)
		if err != nil {
			return j.local(&localError{"state file", err})
		}
		message = "Failed at " + action + ". GitLab may be down.\nBackup " + backupTime.In(j.config.Location).Format("15:04") + " is safe.\nTo go back: usm restore"
	case "Restore":
		message = "Restore failed. Check by hand."
	}
	j.state.Stopped = true
	j.state.StopReason = "Failed at " + action
	detail := message
	if action == "Restore" {
		detail = "restore: " + cause.Error()
	}
	return j.end("failed", detail, message)
}

func (j *Job) Run(now bool) int {
	if err := j.openLog(); err != nil {
		return j.local(err)
	}
	lock, err := state.Lock(j.config.StateDir)
	if errors.Is(err, state.ErrLocked) {
		return 0
	}
	if err != nil {
		return j.local(&localError{"lock", err})
	}
	defer lock.Close()
	j.state, err = state.Load(j.config.StateDir)
	if err != nil {
		return j.local(&localError{"state file", err})
	}
	if j.state.Stopped {
		j.say("Still stopped after a failure. Run usm resume when fixed.")
		return 0
	}
	if !now {
		current := j.now()
		start, err := j.night(current)
		if err != nil {
			return j.local(err)
		}
		if j.state.LastInstallDate == start.Format("2006-01-02") {
			return j.end("nothing", "", "")
		}
		if current.Sub(start) > 2*time.Hour+30*time.Minute {
			return j.skip("too late tonight", nil)
		}
	}
	if err := j.connect(true); err != nil {
		return j.skip("no SSH or sudo", err)
	}
	if j.state.ActiveStep != "" {
		pollCtx, pollCancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := j.remote.Poll(pollCtx, j.state.ActiveStep)
		pollCancel()
		if err != nil {
			return j.skip("cannot read the last step on GitLab", err)
		}
		if output == "running" {
			return j.skip("a step is still running on GitLab", nil)
		}
		if !remote.Terminal(output) {
			return j.skip("cannot read the last step on GitLab", errors.New("unexpected step status"))
		}
		if err := j.saveActive(""); err != nil {
			return j.local(err)
		}
	}
	if err := gitlab.Sudo(j.remote); err != nil {
		return j.skip("no SSH or sudo", err)
	}
	pkg, current, err := gitlab.Package(j.remote)
	if err != nil {
		return j.skip("GitLab package not found", err)
	}
	if err := gitlab.Services(j.remote); err != nil {
		return j.skip("GitLab not running", err)
	}
	if err := j.api.Version(current); err != nil {
		return j.skip("GitLab not answering", err)
	}
	if err := gitlab.Background(j.remote); err != nil {
		return j.skip("GitLab still busy", err)
	}
	versions, err := gitlab.Versions(j.remote, pkg, current)
	if err != nil {
		return j.skip("package list unreadable", err)
	}
	stops, err := gitlab.Stops()
	if err != nil {
		return j.skip("upgrade path list not reachable", err)
	}
	if err := gitlab.Disk(j.remote, j.config.GitLab.BackupDir); err != nil {
		var low *gitlab.LowDisk
		if errors.As(err, &low) {
			return j.skip("not enough disk space. Need "+low.Text(), nil)
		}
		return j.skip("cannot read disk space", err)
	}
	path, err := gitlab.Path(current, versions, stops)
	if err != nil {
		return j.skip(err.Error(), err)
	}
	if len(path) == 0 {
		return j.end("nothing", "", "")
	}
	next := path[0]
	start := j.config.ServerName + ": " + current.Base + " → " + next.Base
	if len(path) > 1 {
		start += fmt.Sprintf(" (then %d more)", len(path)-1)
	}
	if err := j.say(start); err != nil {
		return j.end("skipped", "telegram unreachable", "")
	}
	if err := j.backup(pkg, current); err != nil {
		return j.fail("Backup", err)
	}
	night, err := j.night(j.now())
	if err != nil {
		return j.local(err)
	}
	j.state.LastInstallDate = night.Format("2006-01-02")
	if err := j.save(); err != nil {
		return j.local(err)
	}
	j.say("Installing " + next.Base)
	if err := j.remote.Step("install", []string{pkg, next.Full}, j.config.Location, j.saveActive); err != nil {
		return j.fail("Install", err)
	}
	if err := gitlab.Installed(j.remote, pkg, next); err != nil {
		return j.fail("Install", err)
	}
	j.say("Installed")
	if err := j.health(next); err != nil {
		return j.fail("Check", err)
	}
	if _, err := j.remote.Exec(context.Background(), "sudo gitlab-rake gitlab:check SANITIZE=true"); err != nil {
		j.logf("gitlab:check: " + err.Error())
	}
	j.say("GitLab OK")
	j.state.LastBackup = state.Backup{}
	if err := j.save(); err != nil {
		return j.local(err)
	}
	if err := j.prune(); err != nil {
		j.logf("prune: " + err.Error())
		j.say("Backup not deleted. Check the USM log.")
	} else {
		j.say("Backup deleted")
	}
	return j.end("done", "", "Done. Now "+next.Base)
}

func (j *Job) health(version gitlab.Version) error {
	if err := gitlab.Services(j.remote); err != nil {
		return err
	}
	return j.api.Version(version)
}

func (j *Job) backup(pkg string, current gitlab.Version) error {
	j.say("Backup started")
	name := "usm-" + j.now().Format("20060102-150405") + "-" + current.Base
	dir := j.config.GitLab.BackupDir
	if err := config.Validate("backup name", name); err != nil {
		return err
	}
	if err := config.Validate("backup dir", dir); err != nil {
		return err
	}
	if err := j.remote.Step("backup", []string{name, dir}, j.config.Location, j.saveActive); err != nil {
		return err
	}
	if _, err := j.remote.Exec(context.Background(), "sudo test -s "+dir+"/"+name+"_gitlab_backup.tar"); err != nil {
		return err
	}
	output, err := j.remote.Exec(context.Background(), "sudo ls -1 "+dir+"/"+name+"-etc")
	if err != nil {
		return err
	}
	var files []string
	for _, line := range strings.Split(output, "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) != 1 {
		return errors.New("expected exactly one config backup file")
	}
	if err := config.Validate("etc file", files[0]); err != nil {
		return err
	}
	etcTar := dir + "/" + name + "-etc/" + files[0]
	if err := config.Validate("etc tar", etcTar); err != nil {
		return err
	}
	j.state.LastBackup = state.Backup{Name: name, EtcTar: etcTar, Package: pkg, Version: current.Full, Time: j.now().Format(time.RFC3339)}
	if err := j.save(); err != nil {
		return err
	}
	j.say("Backup done")
	return nil
}

func (j *Job) prune() error {
	dir := j.config.GitLab.BackupDir
	if err := config.Validate("backup dir", dir); err != nil {
		return err
	}
	output, err := j.remote.Exec(context.Background(), "sudo ls -1 "+dir)
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(output, "\n") {
		var name, command string
		switch {
		case strings.HasSuffix(entry, "_gitlab_backup.tar"):
			name = strings.TrimSuffix(entry, "_gitlab_backup.tar")
			command = "sudo rm -f "
		case strings.HasSuffix(entry, "-etc"):
			name = strings.TrimSuffix(entry, "-etc")
			command = "sudo rm -rf "
		default:
			continue
		}
		if config.Validate("backup name", name) != nil {
			continue
		}
		if _, err := j.remote.Exec(context.Background(), command+dir+"/"+entry); err != nil {
			return err
		}
	}
	return nil
}

func (j *Job) Restore(yes bool) int {
	if yes {
		if err := j.openLog(); err != nil {
			return j.local(err)
		}
		lock, err := state.Lock(j.config.StateDir)
		if errors.Is(err, state.ErrLocked) {
			fmt.Println("A run is in progress.")
			return 2
		}
		if err != nil {
			return j.local(&localError{"lock", err})
		}
		defer lock.Close()
	}
	var err error
	j.state, err = state.Load(j.config.StateDir)
	if err != nil {
		return j.local(&localError{"state file", err})
	}
	backup := j.state.LastBackup
	if backup.Name == "" {
		fmt.Println("No backup to restore. USM deletes the backup after a good update.")
		return 2
	}
	if !yes {
		fmt.Println("Backup:", backup.Name)
		fmt.Println("Version:", backup.Version)
		fmt.Println("Run again with --yes to restore.")
		return 2
	}
	failed := func(err error) int { return j.fail("Restore", err) }
	if err := j.connect(true); err != nil {
		return failed(err)
	}
	if j.state.ActiveStep != "" {
		id := j.state.ActiveStep
		pollCtx, pollCancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := j.remote.Poll(pollCtx, id)
		pollCancel()
		if err != nil || (!remote.Terminal(output) && output != "running") {
			if err != nil {
				j.logf(err.Error())
			}
			fmt.Printf("Cannot read the last step on GitLab: %s.\n", id)
			return 2
		}
		if output == "running" {
			fmt.Printf("A step is still running on GitLab: %s. Wait for it.\n", id)
			return 2
		}
		if err := j.saveActive(""); err != nil {
			return j.local(err)
		}
	}
	for _, value := range []struct{ kind, text string }{
		{"package", backup.Package}, {"version", backup.Version}, {"backup name", backup.Name},
		{"backup dir", j.config.GitLab.BackupDir}, {"etc tar", backup.EtcTar},
	} {
		if err := config.Validate(value.kind, value.text); err != nil {
			return failed(err)
		}
	}
	version, err := gitlab.ParseVersion(backup.Version)
	if err != nil {
		return failed(err)
	}
	j.say("Restore started")
	if err := j.remote.Step("restore", []string{backup.Package, backup.Version, backup.Name, j.config.GitLab.BackupDir, backup.EtcTar}, j.config.Location, j.saveActive); err != nil {
		return failed(err)
	}
	if err := gitlab.Installed(j.remote, backup.Package, version); err != nil {
		return failed(err)
	}
	if err := j.health(version); err != nil {
		return failed(err)
	}
	return j.end("done", "restore", "Restore done. Now "+version.Base)
}

func (j *Job) Status() int {
	localState, err := state.Load(j.config.StateDir)
	if err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	view := struct {
		Stopped    bool         `json:"stopped"`
		StopReason string       `json:"stop_reason"`
		LastBackup state.Backup `json:"last_backup"`
		LastRun    state.Run    `json:"last_run"`
	}{localState.Stopped, localState.StopReason, localState.LastBackup, localState.LastRun}
	encoded, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		fmt.Println(err)
		return 0
	}
	fmt.Println(string(encoded))
	if err := j.openLog(); err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	if err := j.connect(false); err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	if err := gitlab.Sudo(j.remote); err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	pkg, current, err := gitlab.Package(j.remote)
	if err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	versions, err := gitlab.Versions(j.remote, pkg, current)
	if err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	stops, err := gitlab.Stops()
	if err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	path, err := gitlab.Path(current, versions, stops)
	if err != nil {
		fmt.Println(j.redact(err.Error()))
		return 0
	}
	fmt.Println("Installed:", current.Full)
	names := make([]string, 0, len(path))
	for _, version := range path {
		names = append(names, version.Full)
	}
	fmt.Println("Path:", strings.Join(names, " → "))
	return 0
}

func (j *Job) Resume() int {
	lock, err := state.Lock(j.config.StateDir)
	if errors.Is(err, state.ErrLocked) {
		fmt.Println("A run is in progress.")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lock.Close()
	j.state, err = state.Load(j.config.StateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	j.state.Stopped = false
	j.state.StopReason = ""
	if err := j.save(); err != nil {
		fmt.Fprintln(os.Stderr, j.redact(err.Error()))
		return 1
	}
	fmt.Println("Resumed.")
	return 0
}

func (j *Job) Test() int {
	if err := j.openLog(); err != nil {
		fmt.Println("FAIL log file:", j.redact(err.Error()))
		return j.local(err)
	}
	failed := false
	report := func(item string, err error) {
		if err != nil {
			failed = true
			fmt.Printf("FAIL %s: %s\n", item, j.redact(err.Error()))
			j.logf(item + ": " + err.Error())
		} else {
			fmt.Println("OK", item)
		}
	}
	err := j.connect(false)
	if err == nil {
		err = gitlab.Sudo(j.remote)
	}
	report("SSH and sudo", err)
	_, current, packageErr := gitlab.Package(j.remote)
	report("Package", packageErr)
	report("Services", gitlab.Services(j.remote))
	report("Disk space", gitlab.Disk(j.remote, j.config.GitLab.BackupDir))
	if packageErr != nil {
		report("API and version", errors.New("installed version is unavailable"))
	} else {
		report("API and version", j.api.Version(current))
	}
	_, err = gitlab.Stops()
	report("Stops file", err)
	report("Telegram", j.say("usm test message"))
	if failed {
		return 1
	}
	return 0
}
