package sync

import (
	"database/sql/driver"
	"strings"

	"mtdiff/internal/chunk"
	"mtdiff/internal/conn"
)

// literalFor renders one raw driver value as a SQL literal for a column of
// the given family.
//
// chunk.Literal is type-agnostic: it renders every []byte as a hex blob.
// That is correct only for genuine binary columns (BLOB / VARBINARY /
// BINARY). The MySQL driver, however, also delivers DECIMAL, TIME, ENUM,
// SET, character strings (VARCHAR / CHAR / TEXT), JSON and BIT as []byte —
// the byte encoding of a *character* value. Rendering those as hex blobs is
// wrong: MySQL refuses to coerce a BINARY literal into DECIMAL (error 1264)
// or a JSON column, would misread the data as binary, and would store a
// garbage bit pattern in a BIT column. So the rendering is keyed on the
// column family (mirroring the normalizer, which is likewise family-aware):
// binary columns keep the hex-blob form, BIT becomes a bit literal, and
// every other family whose value arrives as []byte becomes a quoted
// character string.
func literalFor(fam string, v driver.Value) string {
	switch fam {
	case conn.FamBYTES:
		return chunk.Literal(v)
	case conn.FamBIT:
		return bitLiteral(v)
	}
	if b, ok := v.([]byte); ok {
		return chunk.Literal(string(b))
	}
	return chunk.Literal(v)
}

// keyLits builds one literal renderer per key column family, for the chunk
// strict comparators (RenderLessThan / RenderGreaterThan): the out-of-range
// bounds must be rendered with the same family awareness as the writes, or
// a character or decimal key compared as a hex blob would put rows on the
// wrong side of the boundary.
func keyLits(fams []string) []chunk.LiteralFunc {
	lits := make([]chunk.LiteralFunc, len(fams))
	for i := range fams {
		lits[i] = func(v driver.Value) string { return literalFor(fams[i], v) }
	}
	return lits
}

// bitLiteral renders a BIT value (the driver's big-endian byte pattern) as a
// b'...' bit literal. MySQL stores a bit literal losslessly into a BIT
// column of any width, whereas the byte pattern is meaningless as either a
// hex blob or a character string.
func bitLiteral(v driver.Value) string {
	b, ok := v.([]byte)
	if !ok {
		return chunk.Literal(v)
	}
	var sb strings.Builder
	sb.WriteString("b'")
	for _, x := range b {
		for shift := 7; shift >= 0; shift-- {
			if x>>uint(shift)&1 == 1 {
				sb.WriteByte('1')
			} else {
				sb.WriteByte('0')
			}
		}
	}
	sb.WriteByte('\'')
	return sb.String()
}
