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
	// passwordSet is true when a password was given explicitly, including an
	// empty one (the "user:@host" shorthand: the server has no password,
	// e.g. TiDB's default root). An explicit empty password suppresses the
	// interactive prompt; an absent one ("user@host") still prompts. The
	// field is unexported, so YAML config files can never set it.
	passwordSet bool
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
	// AllowUnenforcedReadOnly relaxes the read-only safety net for backends
	// that cannot enforce a session-level read-only (TiDB: read_only is
	// GLOBAL-only and SET SESSION TRANSACTION READ ONLY is a disabled no-op).
	// When set, opening a read side whose server rejects both enforcement
	// tiers proceeds with a per-connection warning; when clear (default),
	// such a connection is refused. mtdiff still only issues SELECTs on read
	// connections — the flag only accepts that the server could not stop
	// other statements.
	AllowUnenforcedReadOnly bool `yaml:"allow_unenforced_readonly"`
	// Sync options (used by the sync subcommand).
	BatchSize int `yaml:"batch_size"` // rows per multi-row INSERT / commit granularity
	// SampleLimit is a pointer so an explicit 0 ("show no samples") is
	// distinguishable from unset (nil, receives the default of 5): the
	// CLI's --sample-limit 0 and a YAML sample_limit: 0 must both survive
	// ApplyDefaults, while an absent value takes the default.
	SampleLimit  *int `yaml:"sample_limit"`
	NoSyncSchema bool `yaml:"no_sync_schema"` // skip the structure pre-step (default: align the destination structure first)
	// AllowStructureTruncate: when the in-place structure ALTER fails,
	// truncate the destination and re-apply the DDL (the pre-P1-3
	// behavior as a fallback). Default false: the failure stops the table
	// with its data preserved.
	AllowStructureTruncate bool `yaml:"allow_structure_truncate"`
	// AllowRowRewrite: permit the destructive row rewrite for a
	// unique-value conflict (swap/cycle/holder) — DELETE+INSERT of the
	// affected rows. It authorizes the row rewrite ONLY: a cross-chunk
	// swap becomes a full-resync plan (TRUNCATE + reload), executed only
	// when the confirmed plan showed that TRUNCATE (a confirmed
	// row-level plan never escalates to it in the same run). Default
	// false: the table is REFUSED instead, because the rewrite fires FK
	// ON DELETE CASCADE, triggers and audit logs for rows the user never
	// asked to change.
	AllowRowRewrite bool `yaml:"allow_row_rewrite"`
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
	if e.passwordSet {
		// An explicit empty password (the "user:@host" shorthand): the
		// server is password-less, do not prompt.
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
	if c.Opts.SampleLimit != nil && *c.Opts.SampleLimit < 0 {
		return fmt.Errorf("sample_limit must be >= 0, got %d", *c.Opts.SampleLimit)
	}
	return nil
}

// SampleLimitOr dereferences SampleLimit, filling unset (nil) values with
// the default. An explicit 0 is preserved (it means "show no samples").
func (c *Config) SampleLimitOr(def int) int {
	if c.Opts.SampleLimit == nil {
		return def
	}
	return *c.Opts.SampleLimit
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
	if c.Opts.SampleLimit == nil {
		v := 5
		c.Opts.SampleLimit = &v // explicit 0 (show none) is preserved
	}
}
