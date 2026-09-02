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
	// Sync options (used by the sync subcommand).
	BatchSize    int  `yaml:"batch_size"`     // rows per multi-row INSERT / commit granularity
	SampleLimit  int  `yaml:"sample_limit"`   // sample SQL statements shown per table in a dry-run
	NoSyncSchema bool `yaml:"no_sync_schema"` // skip the structure pre-step (default: align the destination structure first)
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
// (the server will reject if it requires one). A password_env that names an
// unset variable is a configuration error, not a silent fallback: an
// accidental no-password connection surfaces as an incomprehensible server
// auth failure instead of an actionable message.
func (e *Endpoint) ResolvePassword(prompt PromptFunc) error {
	if e.PasswordEnv != "" {
		p, ok := os.LookupEnv(e.PasswordEnv)
		if !ok {
			return fmt.Errorf("password_env %q is set but the environment variable is not", e.PasswordEnv)
		}
		e.Password = p
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

// MinChunkSize is the smallest chunk-size accepted as an explicit value
// (0 still means "unset, apply default"). A smaller positive value on a
// large table means one chunk per few rows: the chunk/channel bookkeeping
// grows without bound.
const MinChunkSize = 10

// Validate checks that the config is usable. It must run BEFORE
// ApplyDefaults: defaults rewrite unset (<= 0) values, so validating after
// them can never see an explicit negative value from a YAML file.
func (c *Config) Validate() error {
	if c.Src.Host == "" {
		return fmt.Errorf("source host is required (--src or --src-host)")
	}
	if c.Dst.Host == "" {
		return fmt.Errorf("destination host is required (--dst or --dst-host)")
	}
	// 0 means "unset, apply default"; only reject explicitly negative values.
	if c.Opts.Parallel < 0 {
		return fmt.Errorf("parallel must be >= 0, got %d", c.Opts.Parallel)
	}
	if c.Opts.ChunkSize < 0 {
		return fmt.Errorf("chunk_size must be >= 0, got %d", c.Opts.ChunkSize)
	}
	if c.Opts.ChunkSize > 0 && c.Opts.ChunkSize < MinChunkSize {
		return fmt.Errorf("chunk_size %d is below the minimum of %d", c.Opts.ChunkSize, MinChunkSize)
	}
	if c.Opts.DrillLimit < 0 {
		return fmt.Errorf("drill_limit must be >= 0, got %d", c.Opts.DrillLimit)
	}
	if c.Opts.Tolerance < 0 {
		return fmt.Errorf("tolerance must be >= 0, got %v", c.Opts.Tolerance)
	}
	if c.Opts.BatchSize < 0 {
		return fmt.Errorf("batch_size must be >= 0, got %d", c.Opts.BatchSize)
	}
	if c.Opts.SampleLimit < 0 {
		return fmt.Errorf("sample_limit must be >= 0, got %d", c.Opts.SampleLimit)
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
	if c.Opts.BatchSize <= 0 {
		c.Opts.BatchSize = 1000
	}
	if c.Opts.SampleLimit <= 0 {
		c.Opts.SampleLimit = 5
	}
}
