// Package report renders comparison results as text or JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"mtdiff/internal/compare"
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
