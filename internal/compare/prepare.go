package compare

import (
	"context"
	"fmt"

	"mtdiff/internal/conn"
)

// PrepareSchemas introspects both sides and applies the key override,
// ignored columns and the compatibility check, in the same order compareTable
// has always done it. The sync engine calls the same function so its view of
// the schemas (key selection, ignored columns, column order) is identical to
// the comparison's.
func PrepareSchemas(ctx context.Context, src, dst *conn.Side, table string, key []string, ignore map[string]bool, compat conn.CompatOpts) (*conn.Schema, *conn.Schema, []string, error) {
	// one control session per side, released before the next acquisition
	// (the control pool is single-connection)
	var srcSchema, dstSchema *conn.Schema
	if err := src.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		srcSchema, err = conn.IntrospectTable(ctx, q, table)
		return err
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("src introspection: %w", err)
	}
	if err := dst.WithControl(ctx, func(q conn.Queryer) error {
		var err error
		dstSchema, err = conn.IntrospectTable(ctx, q, table)
		return err
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("dst introspection: %w", err)
	}
	keyWarns := applyKey(srcSchema, dstSchema, key, resolveKeyUniqueness(ctx, src, dst, table, key))
	srcSchema, dstSchema, err := filterIgnored(srcSchema, dstSchema, ignore)
	if err != nil {
		return nil, nil, nil, err
	}
	warns, err := conn.Compatible(srcSchema, dstSchema, compat)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("schema mismatch: %w", err)
	}
	return srcSchema, dstSchema, append(keyWarns, warns...), nil
}

// resolveKeyUniqueness returns the per-side resolver for an explicit
// --key: it asks each side's index catalog whether the key is a unique row
// address there (conn.ExplicitKeyIsUnique). Both sides are introspected
// under the same table name, so the resolver is keyed by side, not by
// table.
func resolveKeyUniqueness(ctx context.Context, src, dst *conn.Side, table string, key []string) func(string) (bool, error) {
	return func(side string) (bool, error) {
		s := src
		if side == "dst" {
			s = dst
		}
		var ok bool
		err := s.WithControl(ctx, func(q conn.Queryer) error {
			var err error
			ok, err = conn.ExplicitKeyIsUnique(ctx, q, table, key)
			return err
		})
		return ok, err
	}
}

// KeyFamilies returns the column families of the schema's key columns, in key
// order (for chunk.Planner).
func KeyFamilies(s *conn.Schema) []string {
	fams := make([]string, len(s.Key))
	for i, k := range s.Key {
		for _, col := range s.Cols {
			if col.Name == k {
				fams[i] = col.Family
				break
			}
		}
	}
	return fams
}
