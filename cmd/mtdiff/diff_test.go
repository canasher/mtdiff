package mtdiff

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mtdiff/internal/config"
)

// newDiffCommand builds a command carrying the diff flags with all values
// reset to their defaults. bindDiffFlags registers into the package-global
// diff struct, so reset it first to avoid cross-test contamination.
func newDiffCommand(t *testing.T) *cobra.Command {
	t.Helper()
	diff = diffOpts{}
	cmd := &cobra.Command{Use: "diff"}
	bindDiffFlags(cmd)
	return cmd
}

// TestApplyOptionsZeroOverrides covers the P2-5 regression: tolerance,
// drill-limit and max-allowed-packet were detected by "value != 0", so an
// explicit --tolerance 0 could not reset a non-zero YAML value. They must
// now be detected via Flags().Changed.
func TestApplyOptionsZeroOverrides(t *testing.T) {
	cmd := newDiffCommand(t)
	cfg := &config.Config{}
	cfg.Opts.Tolerance = 0.5
	cfg.Opts.DrillLimit = 42
	cfg.Opts.MaxAllowedPacket = 123
	cfg.Opts.Parallel = 8
	cfg.Opts.ChunkSize = 1000

	// no flags given: YAML values survive
	if err := applyOptions(cmd, cfg); err != nil {
		t.Fatalf("no flags: %v", err)
	}
	if cfg.Opts.Tolerance != 0.5 || cfg.Opts.DrillLimit != 42 || cfg.Opts.MaxAllowedPacket != 123 {
		t.Errorf("unset flags must not touch YAML values: %+v", cfg.Opts)
	}
	if cfg.Opts.Parallel != 8 || cfg.Opts.ChunkSize != 1000 {
		t.Errorf("unset parallel/chunk-size must keep YAML: %+v", cfg.Opts)
	}

	// explicit zero must reset the YAML value (the regression)
	cmd.Flags().Set("tolerance", "0")
	cmd.Flags().Set("drill-limit", "0")
	cmd.Flags().Set("max-allowed-packet", "0")
	if err := applyOptions(cmd, cfg); err != nil {
		t.Fatalf("explicit zero: %v", err)
	}
	if cfg.Opts.Tolerance != 0 || cfg.Opts.DrillLimit != 0 || cfg.Opts.MaxAllowedPacket != 0 {
		t.Errorf("explicit --flag 0 must reset YAML values: %+v", cfg.Opts)
	}

	// non-zero explicit values still override
	cmd2 := newDiffCommand(t)
	cfg2 := &config.Config{}
	cfg2.Opts.Tolerance = 0.5
	cmd2.Flags().Set("tolerance", "2")
	if err := applyOptions(cmd2, cfg2); err != nil {
		t.Fatalf("non-zero: %v", err)
	}
	if cfg2.Opts.Tolerance != 2 {
		t.Errorf("--tolerance 2 must override, got %v", cfg2.Opts.Tolerance)
	}
}

// TestApplyOptionsIllegalValues guards the argument-error paths.
func TestApplyOptionsIllegalValues(t *testing.T) {
	cmd := newDiffCommand(t)
	cfg := &config.Config{}
	cmd.Flags().Set("parallel", "0")
	if err := applyOptions(cmd, cfg); err == nil || !strings.Contains(err.Error(), "--parallel") {
		t.Errorf("--parallel 0 must be an argument error, got %v", err)
	}
	cmd.Flags().Set("parallel", "2")
	cmd.Flags().Set("chunk-size", "-1")
	if err := applyOptions(cmd, cfg); err == nil || !strings.Contains(err.Error(), "--chunk-size") {
		t.Errorf("--chunk-size -1 must be an argument error, got %v", err)
	}
	if cfg.Opts.Parallel != 2 {
		t.Errorf("valid --parallel must apply before the erroring flag: %+v", cfg.Opts)
	}
}
