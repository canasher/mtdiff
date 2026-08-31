package mtdiff

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"mtdiff/internal/config"
)

// connFlags carries the raw connection flags shared by all subcommands.
type connFlags struct {
	configPath string
	src, dst   string
	srcHost    string
	dstHost    string
	srcPort    int
	dstPort    int
	srcUser    string
	dstUser    string
	srcPassEnv string
	dstPassEnv string
	srcDB      string
	dstDB      string
}

func (f *connFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.configPath, "config", "", "YAML config file; CLI flags override its values")
	cmd.Flags().StringVar(&f.src, "src", "", `source connection "user[:pass]@host[:port]/db"`)
	cmd.Flags().StringVar(&f.dst, "dst", "", `destination connection "user[:pass]@host[:port]/db"`)
	cmd.Flags().StringVar(&f.srcHost, "src-host", "", "source host")
	cmd.Flags().StringVar(&f.dstHost, "dst-host", "", "destination host")
	cmd.Flags().IntVar(&f.srcPort, "src-port", 0, "source port (default 3306)")
	cmd.Flags().IntVar(&f.dstPort, "dst-port", 0, "destination port (default 3306)")
	cmd.Flags().StringVar(&f.srcUser, "src-user", "", "source user")
	cmd.Flags().StringVar(&f.dstUser, "dst-user", "", "destination user")
	cmd.Flags().StringVar(&f.srcPassEnv, "src-password-env", "", "env var holding the source password")
	cmd.Flags().StringVar(&f.dstPassEnv, "dst-password-env", "", "env var holding the destination password")
	cmd.Flags().StringVar(&f.srcDB, "src-db", "", "source database")
	cmd.Flags().StringVar(&f.dstDB, "dst-db", "", "destination database")
}

// build resolves the full configuration: YAML base, granular flags, then the
// shorthand forms (which take precedence over the granular ones).
func (f *connFlags) build(prompt config.PromptFunc) (*config.Config, error) {
	cfg := &config.Config{}
	if f.configPath != "" {
		base, err := config.LoadFile(f.configPath)
		if err != nil {
			return nil, failf(ExitArgErr, "%v", err)
		}
		cfg = base
	}
	apply := func(dst *config.Endpoint, shorthand, host, user, db, passEnv string, port int, label string) error {
		if shorthand != "" {
			ep, err := config.ParseShorthand(shorthand)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			*dst = ep
			return nil
		}
		if host != "" {
			dst.Host = host
		}
		if port != 0 {
			dst.Port = port
		}
		if user != "" {
			dst.User = user
		}
		if db != "" {
			dst.Database = db
		}
		if passEnv != "" {
			dst.PasswordEnv = passEnv
		}
		return nil
	}
	if err := apply(&cfg.Src, f.src, f.srcHost, f.srcUser, f.srcDB, f.srcPassEnv, f.srcPort, "src"); err != nil {
		return nil, failf(ExitArgErr, "%v", err)
	}
	if err := apply(&cfg.Dst, f.dst, f.dstHost, f.dstUser, f.dstDB, f.dstPassEnv, f.dstPort, "dst"); err != nil {
		return nil, failf(ExitArgErr, "%v", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, failf(ExitArgErr, "%v", err)
	}
	if err := cfg.Src.ResolvePassword(prompt); err != nil {
		return nil, failf(ExitArgErr, "%v", err)
	}
	if err := cfg.Dst.ResolvePassword(prompt); err != nil {
		return nil, failf(ExitArgErr, "%v", err)
	}
	return cfg, nil
}

// makePrompt returns an interactive password prompt, or nil when stdin is not
// a terminal (CI: never hang; a missing password surfaces as a server error).
func makePrompt() config.PromptFunc {
	fi, _ := os.Stdin.Stat()
	if fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	return func(label string) (string, error) {
		fmt.Fprintf(os.Stderr, "%s: ", label)
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(pw), nil
	}
}
