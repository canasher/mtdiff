package hash

import (
	"encoding/hex"
	"fmt"
)

// Hex renders a 16-byte table fingerprint as a 32-char hex string.
func Hex(fp [16]byte) string {
	return hex.EncodeToString(fp[:])
}

// HexDigest renders a chunk digest (count plus the path-relevant statistics)
// for human-readable diff reports.
func HexDigest(d ChunkDigest) string {
	if d.Ordered {
		return fmt.Sprintf("%016x:%016x", d.Count, d.Order)
	}
	return fmt.Sprintf("%016x:%016x:%016x:%016x", d.Count, d.Sum, d.Xor, d.SumSq)
}
