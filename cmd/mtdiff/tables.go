package mtdiff

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mtdiff/internal/conn"
)

var tablesFlags connFlags

var tablesCmd = &cobra.Command{
	Use:   "tables",
	Short: "List tables on both sides and their intersection",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := tablesFlags.build(makePrompt())
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		src, err := conn.OpenSide(ctx, "src", cfg.Src, 0, 1)
		if err != nil {
			return failf(ExitRuntimeErr, "%v", err)
		}
		defer src.Close()
		dst, err := conn.OpenSide(ctx, "dst", cfg.Dst, 0, 1)
		if err != nil {
			return failf(ExitRuntimeErr, "%v", err)
		}
		defer dst.Close()

		srcTables, err := conn.ListTables(ctx, src.Ctl())
		if err != nil {
			return failf(ExitRuntimeErr, "src: %v", err)
		}
		dstTables, err := conn.ListTables(ctx, dst.Ctl())
		if err != nil {
			return failf(ExitRuntimeErr, "dst: %v", err)
		}
		sort.Strings(srcTables)
		sort.Strings(dstTables)

		fmt.Printf("src: %s (MySQL %s)\n", src.Masked(), src.Version)
		fmt.Printf("dst: %s (MySQL %s)\n", dst.Masked(), dst.Version)
		fmt.Printf("src tables (%d):  %s\n", len(srcTables), strings.Join(srcTables, ", "))
		fmt.Printf("dst tables (%d):  %s\n", len(dstTables), strings.Join(dstTables, ", "))
		inter := intersection(srcTables, dstTables)
		fmt.Printf("common (%d):      %s\n", len(inter), strings.Join(inter, ", "))
		return nil
	},
}

func intersection(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

func init() {
	tablesFlags.bind(tablesCmd)
	rootCmd.AddCommand(tablesCmd)
}
