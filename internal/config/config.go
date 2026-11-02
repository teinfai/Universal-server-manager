package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

type SSH struct {
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	User           string `yaml:"user"`
	KeyFile        string `yaml:"key_file"`
	KnownHostsFile string `yaml:"known_hosts_file"`
}

type Config struct {
	ServerName string `yaml:"server_name"`
	SSH        SSH    `yaml:"ssh"`
	GitLab     struct {
		URL       string `yaml:"url"`
		BackupDir string `yaml:"backup_dir"`
	} `yaml:"gitlab"`
	Telegram struct {
		BotToken string `yaml:"bot_token"`
		ChatID   string `yaml:"chat_id"`
	} `yaml:"telegram"`
	Schedule struct {
		Timezone string `yaml:"timezone"`
		Start    string `yaml:"start"`
	} `yaml:"schedule"`
	StateDir string         `yaml:"state_dir"`
	LogDir   string         `yaml:"log_dir"`
	Location *time.Location `yaml:"-"`
}

var patterns = map[string]*regexp.Regexp{
	"package":     regexp.MustCompile(`^gitlab-(ce|ee)$`),
	"version":     regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-(ce|ee)\.[0-9]+$`),
	"backup name": regexp.MustCompile(`^usm-[0-9]{8}-[0-9]{6}-[0-9]+\.[0-9]+\.[0-9]+$`),
	"backup dir":  regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`),
	"etc tar":     regexp.MustCompile(`^/[A-Za-z0-9._/-]+/gitlab_config_[0-9_]+\.tar$`),
	"etc file":    regexp.MustCompile(`^gitlab_config_[0-9_]+\.tar$`),
	"step id":     regexp.MustCompile(`^(backup|install|restore)-[0-9]{14}-[0-9a-f]{8}$`),
	"action":      regexp.MustCompile(`^(backup|install|restore)$`),
	"digits":      regexp.MustCompile(`^[0-9]+$`),
	"start":       regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`),
}

func Validate(kind, value string) error {
	p, ok := patterns[kind]
	if !ok || !p.MatchString(value) {
		return fmt.Errorf("invalid %s", kind)
	}
	return nil
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("config allows group or other access")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	for _, field := range []string{
		"server_name", "ssh.host", "ssh.port", "ssh.user", "ssh.key_file", "ssh.known_hosts_file",
		"gitlab.url", "gitlab.backup_dir", "telegram.bot_token", "telegram.chat_id",
		"schedule.timezone", "schedule.start", "state_dir", "log_dir",
	} {
		if !present(&document, strings.Split(field, ".")) {
			return nil, fmt.Errorf("missing field: %s", field)
		}
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("config must contain one YAML document")
	}
	if c.SSH.Port < 1 || c.SSH.Port > 65535 {
		return nil, errors.New("invalid ssh.port")
	}
	if err := Validate("backup dir", c.GitLab.BackupDir); err != nil {
		return nil, fmt.Errorf("gitlab.backup_dir: %w", err)
	}
	if err := Validate("start", c.Schedule.Start); err != nil {
		return nil, err
	}
	c.Location, err = time.LoadLocation(c.Schedule.Timezone)
	if err != nil {
		return nil, fmt.Errorf("schedule.timezone: %w", err)
	}
	u, err := url.Parse(c.GitLab.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid gitlab.url")
	}
	c.GitLab.URL = strings.TrimRight(c.GitLab.URL, "/")
	return &c, nil
}

func present(n *yaml.Node, fields []string) bool {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if len(fields) == 0 {
		return n.Kind == yaml.ScalarNode && n.Tag != "!!null" && strings.TrimSpace(n.Value) != ""
	}
	if n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == fields[0] {
			return present(n.Content[i+1], fields[1:])
		}
	}
	return false
}
