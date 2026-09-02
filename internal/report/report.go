// Package report renders comparison results as text or JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"mtdiff/internal/compare"
	msync "mtdiff/internal/sync"
)

// Text writes a human-readable report.
func Text(w io.Writer, res []compare.TableResult) {
	fmt.Fprintf(w, "%-28s %10s %10s  %-10s  %s\n", "TABLE", "SRC_ROWS", "DST_ROWS", "STATUS", "DETAIL")
	for _, r := range res {
		detail := ""
		switch r.Status {
		case "DIFFERENT":
			if r.SrcRows != r.DstRows {
				detail = fmt.Sprintf("row count differs (%d vs %d); %d/%d chunks differ",
					r.SrcRows, r.DstRows, len(r.DiffChunks), r.Chunks)
			} else {
				detail = fmt.Sprintf("%d/%d chunks differ", len(r.DiffChunks), r.Chunks)
			}
		case "ERROR":
			detail = r.Error
		}
		fmt.Fprintf(w, "%-28s %10d %10d  %-10s  %s\n", r.Name, r.SrcRows, r.DstRows, r.Status, detail)
		for _, wn := range r.Warnings {
			fmt.Fprintf(w, "  warn %s: %s\n", r.Name, wn)
		}
		for _, d := range r.DiffChunks {
			fmt.Fprintf(w, "  chunk %3d  key [%s .. %s]  src=%s dst=%s\n", d.ID, d.Lo, d.Hi, d.Src, d.Dst)
		}
		for _, d := range r.Rows {
			fmt.Fprintf(w, "  row  %-14s key [%s]  src=%s dst=%s\n", d.Kind, d.Keys, d.SrcVals, d.DstVals)
		}
	}
	differ, err := 0, 0
	for _, r := range res {
		if r.Differing() {
			differ++
		}
		if r.Status == "ERROR" {
			err++
		}
	}
	if err > 0 {
		fmt.Fprintf(w, "RESULT: %d of %d tables errored\n", err, len(res))
	} else if differ > 0 {
		fmt.Fprintf(w, "RESULT: %d of %d tables differ\n", differ, len(res))
	} else {
		fmt.Fprintf(w, "RESULT: all %d tables identical\n", len(res))
	}
}

// JSON writes a machine-readable report; CI can check the "ok" field.
func JSON(w io.Writer, res []compare.TableResult) {
	ok := true
	tables := make([]map[string]any, 0, len(res))
	for _, r := range res {
		if r.Status != "OK" {
			ok = false
		}
		chunks := make([]map[string]any, 0, len(r.DiffChunks))
		for _, d := range r.DiffChunks {
			chunks = append(chunks, map[string]any{
				"id": d.ID, "lo": d.Lo, "hi": d.Hi, "src": d.Src, "dst": d.Dst,
			})
		}
		rows := make([]map[string]any, 0, len(r.Rows))
		for _, d := range r.Rows {
			rows = append(rows, map[string]any{
				"kind": string(d.Kind), "key": d.Keys, "src": d.SrcVals, "dst": d.DstVals,
			})
		}
		tables = append(tables, map[string]any{
			"name":        r.Name,
			"src_rows":    r.SrcRows,
			"dst_rows":    r.DstRows,
			"status":      r.Status,
			"error":       r.Error,
			"chunks":      r.Chunks,
			"diff_chunks": chunks,
			"src_fp":      r.SrcFP,
			"dst_fp":      r.DstFP,
			"rows":        rows,
			"warnings":    r.Warnings,
		})
	}
	out := map[string]any{
		"ok":     ok,
		"tables": tables,
		"summary": map[string]int{
			"total": len(res),
			"ok":    len(res) - countNonOK(res),
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(w, "{\"error\":%q}\n", err)
	}
}

func countNonOK(res []compare.TableResult) int {
	n := 0
	for _, r := range res {
		if r.Status != "OK" {
			n++
		}
	}
	return n
}

// OK reports whether every table compared identical.
func OK(res []compare.TableResult) bool {
	for _, r := range res {
		if r.Status != "OK" {
			return false
		}
	}
	return len(res) > 0
}

// SyncText renders the sync plan (applied=false: a dry-run preview) or the
// apply + verification report (applied=true).
func SyncText(w io.Writer, applied bool, res []msync.TableSync) {
	fmt.Fprintf(w, "%-24s %-9s %10s %10s %8s %8s %8s %-9s %-10s  %s\n",
		"TABLE", "MODE", "SRC_ROWS", "DST_ROWS", "INSERTS", "UPDATES", "DELETES", "STATUS", "VERIFY", "DETAIL")
	for _, r := range res {
		fmt.Fprintf(w, "%-24s %-9s %10d %10d %8d %8d %8d %-9s %-10s  %s\n",
			r.Name, r.Mode, r.SrcRows, r.DstRows, r.Inserts, r.Updates, r.Deletes, r.Status, r.Verified, syncDetail(r))
		for _, s := range r.SchemaSQL {
			fmt.Fprintf(w, "  DDL: %s\n", s)
		}
		for _, s := range r.SampleSQL {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	var need, failed, appliedN, verifiedOK int
	for _, r := range res {
		switch r.Status {
		case "PLANNED":
			need++
		case "FAILED":
			failed++
		case "APPLIED":
			appliedN++
			if r.Verified == "OK" {
				verifiedOK++
			}
		}
	}
	if applied {
		switch {
		case failed > 0:
			fmt.Fprintf(w, "RESULT: %d table(s) failed; %d of %d synced, %d verified identical\n",
				failed, appliedN, len(res), verifiedOK)
		case appliedN > 0 && verifiedOK < appliedN:
			fmt.Fprintf(w, "RESULT: %d table(s) synced but only %d verified identical (re-run mtdiff diff to inspect)\n",
				appliedN, verifiedOK)
		case appliedN > 0:
			fmt.Fprintf(w, "RESULT: all %d synced table(s) verified identical\n", appliedN)
		default:
			fmt.Fprintf(w, "RESULT: all %d tables identical, nothing to do\n", len(res))
		}
		return
	}
	switch {
	case failed > 0:
		fmt.Fprintf(w, "RESULT: %d of %d tables errored (dry-run; nothing was written)\n", failed, len(res))
	case need > 0:
		fmt.Fprintf(w, "RESULT: %d of %d tables need sync (dry-run; nothing was written)\n", need, len(res))
	default:
		fmt.Fprintf(w, "RESULT: all %d tables identical, nothing to do\n", len(res))
	}
}

func syncDetail(r msync.TableSync) string {
	if r.Error != "" {
		return r.Error
	}
	aligned := ""
	if r.SchemaChanged {
		aligned = "structure aligned (" + strconv.Itoa(len(r.SchemaSQL)) + " DDL) + "
	}
	switch r.Mode {
	case "FULL":
		return aligned + "truncate + full resync"
	case "ROWLEVEL":
		return fmt.Sprintf("row-level: %d insert, %d update, %d delete", r.Inserts, r.Updates, r.Deletes)
	case "SKIP":
		if r.SchemaChanged {
			return "structure aligned"
		}
		return "identical"
	}
	return ""
}

// SyncJSON writes a machine-readable sync report. "ok" is true when the run
// is clean: in a dry-run, no table needs sync (and none errored); after an
// apply, nothing failed and every synced table verified identical.
func SyncJSON(w io.Writer, applied bool, res []msync.TableSync) {
	ok := true
	tables := make([]map[string]any, 0, len(res))
	for _, r := range res {
		if !applied {
			if r.Status == "PLANNED" || r.Status == "FAILED" {
				ok = false
			}
		} else if r.Status == "FAILED" || (r.Verified != "" && r.Verified != "OK") {
			ok = false
		}
		sample := make([]string, 0, len(r.SampleSQL))
		sample = append(sample, r.SampleSQL...)
		schema := make([]string, 0, len(r.SchemaSQL))
		schema = append(schema, r.SchemaSQL...)
		tables = append(tables, map[string]any{
			"name":           r.Name,
			"mode":           r.Mode,
			"src_rows":       r.SrcRows,
			"dst_rows":       r.DstRows,
			"inserts":        r.Inserts,
			"updates":        r.Updates,
			"deletes":        r.Deletes,
			"chunks":         r.Chunks,
			"truncated":      r.Truncated,
			"status":         r.Status,
			"error":          r.Error,
			"verified":       r.Verified,
			"sample_sql":     sample,
			"schema_changed": r.SchemaChanged,
			"schema_sql":     schema,
		})
	}
	out := map[string]any{
		"ok":      ok,
		"applied": applied,
		"tables":  tables,
		"summary": map[string]int{"total": len(res)},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(w, "{\"error\":%q}\n", err)
	}
}
