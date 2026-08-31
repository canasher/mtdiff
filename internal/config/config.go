// Package config parses mtdiff configuration from CLI flags, YAML files
// and environment variables.
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Endpoint describes one MySQL endpoint (source or destination).
type Endpoint struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	User        string `yaml:"user"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	Database    string `yaml:"database"`
}

// Options are comparison-behavior flags, shared by CLI and YAML.
type Options struct {
	Tables           []string `yaml:"tables"`
	ExcludeTables    []string `yaml:"exclude_tables"`
	Key              []string `yaml:"key"`
	Parallel         int      `yaml:"parallel"`
	ChunkSize        int      `yaml:"chunk_size"`
	Tolerance        float64  `yaml:"tolerance"`
	Where            string   `yaml:"where"`
	IgnoreColumns    []string `yaml:"ignore_columns"`
	Snapshot         bool     `yaml:"snapshot"`
	Drill            bool     `yaml:"drill"`
	DrillLimit       int      `yaml:"drill_limit"`
	NoTrim           bool     `yaml:"no_trim"`
	FoldCase         bool     `yaml:"fold_case"`
	NormalizeJSON    bool     `yaml:"normalize_json"`
	AllowTZSwap      bool     `yaml:"allow_tz_swap"`
	StrictTypes      bool     `yaml:"strict_types"`
	MaxAllowedPacket int      `yaml:"max_allowed_packet"`
	Secure           bool     `yaml:"secure"`
}

// Config is the fully-resolved configuration.
type Config struct {
	Src  Endpoint `yaml:"src"`
	Dst  Endpoint `yaml:"dst"`
	Opts Options  `yaml:"options"`
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadFile loads a YAML config file. ${ENV} references in string values are
// replaced with the environment variable's value (empty if unset).
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(expandEnv(data), &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

func expandEnv(data []byte) []byte {
	return envRe.ReplaceAllFunc(data, func(m []byte) []byte {
		name := m[2 : len(m)-1]
		return []byte(os.Getenv(string(name)))
	})
}

// PromptFunc asks the user for a secret (e.g. a password) on a terminal.
type PromptFunc func(label string) (string, error)

// ResolvePassword fills Endpoint.Password from its configured source.
// Priority: PasswordEnv > pre-filled Password (YAML/DSN) > prompt.
// If no source yields a password the endpoint stays password-less
// (the server will reject if it requires one).
func (e *Endpoint) ResolvePassword(prompt PromptFunc) error {
	if e.PasswordEnv != "" {
		e.Password = os.Getenv(e.PasswordEnv)
		return nil
	}
	if e.Password != "" {
		return nil
	}
	if prompt != nil {
		p, err := prompt("password for " + e.User + "@" + e.Host)
		if err != nil {
			return err
		}
		e.Password = p
	}
	return nil
}

// Validate checks that the config is usable.
func (c *Config) Validate() error {
	if c.Src.Host == "" {
		return fmt.Errorf("source host is required (--src or --src-host)")
	}
	if c.Dst.Host == "" {
		return fmt.Errorf("destination host is required (--dst or --dst-host)")
	}
	// 0 means "unset, apply default"; only reject explicitly negative values.
	if c.Opts.Parallel < 0 {
		return fmt.Errorf("--parallel must be >= 0")
	}
	if c.Opts.ChunkSize < 0 {
		return fmt.Errorf("--chunk-size must be >= 0")
	}
	if c.Opts.Tolerance < 0 {
		return fmt.Errorf("--tolerance must be >= 0")
	}
	return nil
}

// ApplyDefaults fills unset positive options with their defaults.
func (c *Config) ApplyDefaults() {
	if c.Src.Port == 0 {
		c.Src.Port = 3306
	}
	if c.Dst.Port == 0 {
		c.Dst.Port = 3306
	}
	if c.Opts.Parallel <= 0 {
		c.Opts.Parallel = 4
	}
	if c.Opts.ChunkSize <= 0 {
		c.Opts.ChunkSize = 10000
	}
	if c.Opts.DrillLimit <= 0 {
		c.Opts.DrillLimit = 10
	}
}
