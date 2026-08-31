// Package normalize converts driver values into unambiguous canonical byte
// representations. It is the correctness foundation of mtdiff: two rows
// compare equal if and only if their canonical forms are equal.
//
// Encoding: each column is encoded as TLV:
//
//	NULL:  [0x00]                              (1 byte)
//	value: [typeTag(1B)][len(2B big-endian)][payload]
//
// NULL uses its own type tag rather than a payload sentinel, so BLOB values
// containing NUL bytes cannot be confused with NULL. A row is the
// concatenation of its column encodings.
package normalize

import (
	"database/sql/driver"
	"fmt"

	"mtdiff/internal/conn"
)

// type tags
const (
	tagNULL      = 0x00
	tagINT       = 0x01
	tagUINT      = 0x02
	tagDECIMAL   = 0x03
	tagFLOAT     = 0x04
	tagDOUBLE    = 0x05
	tagDATE      = 0x06
	tagTIME      = 0x07
	tagDATETIME  = 0x08
	tagTIMESTAMP = 0x09
	tagYEAR      = 0x0A
	tagENUM      = 0x0B
	tagSET       = 0x0C
	tagSTR       = 0x0D
	tagBYTES     = 0x0E
	tagJSON      = 0x0F
	tagBIT       = 0x10
)

// Options control value normalization.
type Options struct {
	Tolerance     float64 // >0: float/double quantized to this grid before comparison
	TrimTrailing  bool    // default true: trim trailing spaces (CHAR semantics)
	FoldCase      bool    // case-fold strings before comparison
	NormalizeJSON bool    // canonicalize JSON (sorted keys, normalized numbers)
	AllowTZSwap   bool    // encode DATETIME and TIMESTAMP identically
	IgnoreCols    map[string]bool
}

// DefaultOptions returns the built-in behavior.
func DefaultOptions() Options {
	return Options{TrimTrailing: true}
}

// Normalizer encodes rows for a fixed column set.
type Normalizer struct {
	cols []conn.Column
	opts Options
}

// NewNormalizer builds a normalizer. Column order defines row layout; both
// sides are SELECTed in source-side column order to stay immune to column
// reordering on the destination.
func NewNormalizer(cols []conn.Column, opts Options) *Normalizer {
	return &Normalizer{cols: cols, opts: opts}
}

// Normalize encodes one row (driver values, same order as the constructor's
// columns) into canonical bytes. The row is []any because database/sql
// cannot Scan NULLs into []driver.Value destinations; the elements are
// driver values either way, so the caller's scan buffer can be passed
// straight through without a per-row copy. buf may be reused across calls;
// the returned slice is valid until the next call on this normalizer.
func (n *Normalizer) Normalize(row []any, buf []byte) ([]byte, error) {
	if len(row) != len(n.cols) {
		return nil, fmt.Errorf("row has %d values, expected %d", len(row), len(n.cols))
	}
	for i, col := range n.cols {
		var err error
		buf, err = n.encodeColumn(buf, col, row[i])
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", col.Name, err)
		}
	}
	return buf, nil
}

func (n *Normalizer) encodeColumn(buf []byte, col conn.Column, v driver.Value) ([]byte, error) {
	if v == nil {
		return append(buf, tagNULL), nil
	}
	payload, err := n.encodeValue(col, v)
	if err != nil {
		return nil, err
	}
	return appendTLV(buf, n.tagFor(col), payload), nil
}

func (n *Normalizer) tagFor(c conn.Column) byte {
	switch c.Family {
	case conn.FamINT:
		return tagINT
	case conn.FamUINT:
		return tagUINT
	case conn.FamDECIMAL:
		return tagDECIMAL
	case conn.FamFLOAT:
		return tagFLOAT
	case conn.FamDOUBLE:
		return tagDOUBLE
	case conn.FamDATE:
		return tagDATE
	case conn.FamTIME:
		return tagTIME
	case conn.FamDATETIME:
		return tagDATETIME
	case conn.FamTIMESTAMP:
		if n.opts.AllowTZSwap {
			return tagDATETIME
		}
		return tagTIMESTAMP
	case conn.FamYEAR:
		return tagYEAR
	case conn.FamENUM:
		return tagENUM
	case conn.FamSET:
		return tagSET
	case conn.FamSTR:
		return tagSTR
	case conn.FamBYTES:
		return tagBYTES
	case conn.FamJSON:
		return tagJSON
	case conn.FamBIT:
		return tagBIT
	}
	return tagSTR
}

func appendTLV(buf []byte, tag byte, payload []byte) []byte {
	buf = append(buf, tag, byte(len(payload)>>8), byte(len(payload)))
	return append(buf, payload...)
}
