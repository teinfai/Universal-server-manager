package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type Backup struct {
	Name    string `json:"name"`
	EtcTar  string `json:"etc_tar"`
	Package string `json:"package"`
	Version string `json:"version"`
	Time    string `json:"time"`
}

type Run struct {
	Time   string `json:"time"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

type State struct {
	Stopped         bool   `json:"stopped"`
	StopReason      string `json:"stop_reason"`
	LastBackup      Backup `json:"last_backup"`
	LastRun         Run    `json:"last_run"`
	ActiveStep      string `json:"active_step"`
	LastInstallDate string `json:"last_install_date"`
}

var ErrLocked = errors.New("lock already held")

func Lock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "usm.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

func Load(dir string) (*State, error) {
	s := &State{}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, errors.New("state must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(s); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("unexpected data after state")
	}
	return s, nil
}

func Save(dir string, s *State) error {
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(s); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, "state.json")); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
