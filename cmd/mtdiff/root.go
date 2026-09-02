// Package mtdiff wires the mtdiff CLI commands.
package mtdiff

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Exit codes: 0 = all tables identical, 1 = differences found,
// 2 = runtime error (connect/schema/introspection), 3 = argument error.
const (
	ExitOK         = 0
	ExitDifferent  = 1
	ExitRuntimeErr = 2
	ExitArgErr     = 3
)

// Version is overridden at build time via -ldflags.
var Version = "0.1.0-dev"

var rootCmd = &cobra.Command{
	Use:   "mtdiff",
	Short: "Compare data consistency between two MySQL tables",
	Long: `mtdiff compares whether the data of two MySQL tables (or a set of tables)
are identical. It chunks by key, streams rows, normalizes values and compares
fingerprints. Hashing happens in the application (not via MySQL MD5()), so it
works against MySQL-compatible layers such as TiDB and PolarDB-X.

Exit codes:
  0  all tables identical
  1  differences found
  2  runtime error (connect / schema / introspection)
  3  argument error`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command and returns the process exit code.
func Execute() int {
	err := rootCmd.Execute()
	if err == nil {
		return ExitOK
	}
	if ee, ok := err.(*ExitError); ok {
		if ee.Msg != "" {
			fmt.Fprintln(os.Stderr, "error:", ee.Msg)
		}
		return ee.Code
	}
	// Cobra argument / validation errors.
	fmt.Fprintln(os.Stderr, "error:", err)
	return ExitArgErr
}

// ExitError is an error carrying an explicit process exit code.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// failf returns an ExitError with the given exit code.
func failf(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// progressLog is the Progress callback wired into the comparer and the
// sync runner: long-running phases (chunk scans, resync chunks) land on
// stderr so a multi-hour run on a huge table is not a silent process. The
// report (text or JSON) goes to stdout and stays untouched, so JSON output
// stays parseable even with progress lines on the terminal.
func progressLog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run:   func(cmd *cobra.Command, _ []string) { fmt.Println("mtdiff", Version) },
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// The root command runs diff directly, so the bare form
	// `mtdiff --src ... --dst ...` works without a subcommand.
	rootCmd.RunE = diffRunE
	diffFlags.bind(rootCmd)
	bindCmpFlags(rootCmd, &diff)
}
