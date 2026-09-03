package mtdiff

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mtdiff/internal/config"
	msync "mtdiff/internal/sync"
)

func intPtr(v int) *int { return &v }

// newSyncCommand builds a command carrying the sync flags with all values
// reset to their defaults, mirroring newDiffCommand.
func newSyncCommand(t *testing.T) *cobra.Command {
	t.Helper()
	syncOpt = syncOpts{}
	cmd := &cobra.Command{Use: "sync"}
	bindCmpFlags(cmd, &syncOpt.cmp)
	f := cmd.Flags()
	f.BoolVar(&syncOpt.apply, "apply", false, "")
	f.BoolVar(&syncOpt.yes, "yes", false, "")
	f.IntVar(&syncOpt.batchSize, "batch-size", 0, "")
	f.IntVar(&syncOpt.sampleLimit, "sample-limit", 0, "")
	f.BoolVar(&syncOpt.noSyncSchema, "no-sync-schema", false, "")
	return cmd
}

// TestConfirmDecision covers the --apply gate: a dry run never asks, --yes
// never asks, a non-terminal stdin cannot confirm, only an interactive
// terminal without --yes prompts.
func TestConfirmDecision(t *testing.T) {
	cases := []struct {
		apply, yes, tty bool
		want            confirmResult
	}{
		{false, false, false, confirmProceed},
		{false, true, false, confirmProceed},
		{true, true, false, confirmProceed},
		{true, false, true, confirmPrompt},
		{true, false, false, confirmArgErr},
	}
	for _, c := range cases {
		if got := confirmDecision(c.apply, c.yes, c.tty); got != c.want {
			t.Errorf("confirmDecision(apply=%v, yes=%v, tty=%v) = %v, want %v",
				c.apply, c.yes, c.tty, got, c.want)
		}
	}
}

// TestSyncTableSets covers the whole-database table discovery: the sync
// set is the source's base tables minus the excluded ones, and the extras
// are the destination's tables the source lacks (which plan a DROP TABLE)
// — unless drops are disallowed (--where), or the table is excluded.
func TestSyncTableSets(t *testing.T) {
	src := []string{"a", "b", "c"}
	dst := []string{"a", "c", "x", "y"}

	tables, extra := syncTableSets(src, dst, nil, true)
	if len(tables) != 3 || tables[0] != "a" || tables[2] != "c" {
		t.Errorf("sync set = %v, want [a b c]", tables)
	}
	if len(extra) != 2 || extra[0] != "x" || extra[1] != "y" {
		t.Errorf("extras = %v, want [x y]", extra)
	}

	// exclusions remove from BOTH sets: an excluded dst-only table is
	// neither synced nor dropped
	tables, extra = syncTableSets(src, dst, []string{"b", "x"}, true)
	if len(tables) != 2 || tables[0] != "a" || tables[1] != "c" {
		t.Errorf("excluded: sync set = %v, want [a c]", tables)
	}
	if len(extra) != 1 || extra[0] != "y" {
		t.Errorf("excluded: extras = %v, want [y]", extra)
	}

	// a dst-only table is NOT extra when the source has it (same name)
	tables, extra = syncTableSets([]string{"a"}, []string{"a", "z"}, nil, true)
	if len(tables) != 1 || len(extra) != 1 || extra[0] != "z" {
		t.Errorf("same-name = %v extras = %v, want one table, extra z", tables, extra)
	}

	// drops disallowed (--where): no extras even with dst-only tables
	tables, extra = syncTableSets(src, dst, nil, false)
	if len(tables) != 3 || extra != nil {
		t.Errorf("no drops: tables = %v extras = %v, want 3 tables, no extras", tables, extra)
	}

	// both sides empty
	tables, extra = syncTableSets(nil, nil, nil, true)
	if tables != nil || extra != nil {
		t.Errorf("empty: tables = %v extras = %v, want none", tables, extra)
	}
}

// TestAllSkipWithNewModes pins that a CREATE, DROP, STATE, FULL or
// ROWLEVEL plan (not just a data op) means "something would be written":
// the apply gate must prompt and open the write connection.
func TestAllSkipWithNewModes(t *testing.T) {
	skip := func(modes ...string) bool {
		plans := make([]msync.TableSync, len(modes))
		for i, m := range modes {
			plans[i] = msync.TableSync{Name: fmt.Sprintf("t%d", i), Mode: m, Status: "PLANNED"}
		}
		return allSkip(plans)
	}
	if skip() {
		t.Error("an empty plan list is not all-skip (there is nothing to confirm)")
	}
	if !skip("SKIP", "SKIP") {
		t.Error("all SKIP must be all-skip")
	}
	for _, mode := range []string{"CREATE", "DROP", "STATE", "FULL", "ROWLEVEL", "ERROR"} {
		if skip("SKIP", mode) {
			t.Errorf("a %s plan must not be all-skip", mode)
		}
	}
}

// TestApplySyncOpts covers batch-size / sample-limit handling: an explicit
// value (Flags().Changed) must be legal and override YAML, while an unset
// flag leaves the YAML value for ApplyDefaults to fill.
func TestApplySyncOpts(t *testing.T) {
	cmd := newSyncCommand(t)
	cfg := &config.Config{}
	cfg.Opts.BatchSize = 500
	cfg.Opts.SampleLimit = intPtr(3)
	cfg.Opts.Tolerance = 0.5

	// no flags given: YAML values survive, no error
	if err := applySyncOpts(cmd, &syncOpt, cfg); err != nil {
		t.Fatalf("no flags: %v", err)
	}
	if cfg.Opts.BatchSize != 500 || cfg.SampleLimitOr(-1) != 3 || cfg.Opts.Tolerance != 0.5 {
		t.Errorf("unset flags must not touch YAML values: %+v", cfg.Opts)
	}

	// explicit --batch-size 0 is an argument error (0 means "unset")
	cmd2 := newSyncCommand(t)
	cfg2 := &config.Config{}
	cfg2.Opts.BatchSize = 500
	cmd2.Flags().Set("batch-size", "0")
	if err := applySyncOpts(cmd2, &syncOpt, cfg2); err == nil || !strings.Contains(err.Error(), "--batch-size") {
		t.Errorf("--batch-size 0 must be an argument error, got %v", err)
	}
	if cfg2.Opts.BatchSize != 500 {
		t.Errorf("failed validation must not overwrite YAML value: %+v", cfg2.Opts)
	}

	// explicit --sample-limit -1 is an argument error
	cmd3 := newSyncCommand(t)
	cfg3 := &config.Config{}
	cmd3.Flags().Set("sample-limit", "-1")
	if err := applySyncOpts(cmd3, &syncOpt, cfg3); err == nil || !strings.Contains(err.Error(), "--sample-limit") {
		t.Errorf("--sample-limit -1 must be an argument error, got %v", err)
	}

	// valid explicit values override; 0 is legal for sample-limit (means none)
	cmd4 := newSyncCommand(t)
	cfg4 := &config.Config{}
	cfg4.Opts.BatchSize = 500
	cfg4.Opts.SampleLimit = intPtr(3)
	cmd4.Flags().Set("batch-size", "250")
	cmd4.Flags().Set("sample-limit", "0")
	if err := applySyncOpts(cmd4, &syncOpt, cfg4); err != nil {
		t.Fatalf("valid overrides: %v", err)
	}
	if cfg4.Opts.BatchSize != 250 || cfg4.SampleLimitOr(-1) != 0 {
		t.Errorf("explicit values must override YAML: %+v", cfg4.Opts)
	}

	// the shared comparison flags flow through applySyncOpts too
	cmd5 := newSyncCommand(t)
	cfg5 := &config.Config{}
	cmd5.Flags().Set("tolerance", "2")
	if err := applySyncOpts(cmd5, &syncOpt, cfg5); err != nil {
		t.Fatalf("tolerance: %v", err)
	}
	if cfg5.Opts.Tolerance != 2 {
		t.Errorf("--tolerance must flow through applySyncOpts, got %+v", cfg5.Opts)
	}

	// --no-sync-schema: unset leaves the YAML value (structure sync on),
	// explicit true overrides it off
	cmd6 := newSyncCommand(t)
	cfg6 := &config.Config{}
	cfg6.Opts.NoSyncSchema = false
	if err := applySyncOpts(cmd6, &syncOpt, cfg6); err != nil {
		t.Fatalf("no-sync-schema unset: %v", err)
	}
	if cfg6.Opts.NoSyncSchema {
		t.Errorf("unset --no-sync-schema must not touch the YAML value: %+v", cfg6.Opts)
	}
	cmd7 := newSyncCommand(t)
	cfg7 := &config.Config{}
	cfg7.Opts.NoSyncSchema = false
	cmd7.Flags().Set("no-sync-schema", "true")
	if err := applySyncOpts(cmd7, &syncOpt, cfg7); err != nil {
		t.Fatalf("no-sync-schema explicit: %v", err)
	}
	if !cfg7.Opts.NoSyncSchema {
		t.Errorf("explicit --no-sync-schema must override the YAML value: %+v", cfg7.Opts)
	}
}
