// Package normalize converts driver values into unambiguous canonical byte
// representations. It is the correctness foundation of mtdiff: two rows
// compare equal if and only if their canonical forms are equal.
//
// Encoding: each column is encoded as TLV:
//
//	NULL:  [0x00]                                        (1 byte)
//	value: [typeTag(1B)][len(8B big-endian uint64)][payload]
//
// NULL uses its own type tag rather than a payload sentinel, so BLOB values
// containing NUL bytes cannot be confused with NULL. A row is the
// concatenation of its column encodings.
//
// The payload length is 8 bytes, not 2: a 16-bit length truncates modulo
// 65536 once a payload (a TEXT/BLOB/JSON value — the tool explicitly
// supports values far beyond 64 KiB) reaches 64 KiB, and two DISTINCT rows
// then render IDENTICAL bytes (see TestTLCollisionRegression): the
// injective invariant of the canonical form is the correctness contract,
// and a silent collision is a silent false identical. An 8-byte length
// overflows only at 2^64 bytes — unreachable (MySQL's per-column limit is
// 1 GiB, a row is far below 2^64).
package normalize

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"

	"mtdiff/internal/conn"
)

// type tags.
//
// All numeric families (INT, UINT, DECIMAL, FLOAT, DOUBLE) share ONE tag
// (tagNUMERIC): the schema-compatibility layer already allows these
// families to be compared across each other ("compared after
// normalization"), so a cross-family equal value (INT 1 vs DECIMAL 1.00 vs
// DOUBLE 1.0) must render under the SAME tag — five distinct tags would
// make them compare unequal forever and the declared numeric compatibility
// would be fiction. The payload still carries the family's exact
// semantics (INT/UINT the exact decimal, DECIMAL the exact normalized
// decimal, FLOAT/DOUBLE the tolerance-aware decimal), so different VALUES
// across families (INT 2 vs DOUBLE 1.0) stay different; --strict-types
// rejects the cross-family schema itself before any normalization.
const (
	tagNULL      = 0x00
	tagNUMERIC   = 0x01 // INT / UINT / DECIMAL / FLOAT / DOUBLE (payload carries the family's exact semantics)
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
	case conn.FamINT, conn.FamUINT, conn.FamDECIMAL, conn.FamFLOAT, conn.FamDOUBLE:
		// one tag for every numeric family: cross-family numeric equality
		// (INT 1 == DECIMAL 1.00 == DOUBLE 1.0) is the declared schema
		// compatibility, so the tag must not distinguish them
		return tagNUMERIC
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

// appendTLV appends one column's value encoding: the type tag, the payload
// length as an 8-byte big-endian uint64, then the payload. The length is
// 8 bytes (see the package doc): the 2-byte length this used to carry
// truncates modulo 65536 at 64 KiB, so a 65536-byte payload encodes the
// SAME header as a 0-byte one and two distinct rows can collide (P0-1).
func appendTLV(buf []byte, tag byte, payload []byte) []byte {
	buf = append(buf, tag)
	var lenB [8]byte
	binary.BigEndian.PutUint64(lenB[:], uint64(len(payload)))
	buf = append(buf, lenB[:]...)
	return append(buf, payload...)
}
