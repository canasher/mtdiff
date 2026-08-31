package conn

import (
	"fmt"
	"strings"
)

// CompatOpts controls schema-compatibility strictness.
type CompatOpts struct {
	Strict      bool // require byte-identical type strings
	AllowTZSwap bool // allow DATETIME/TIMESTAMP swaps, compared as UTC instants
}

// Compatible checks that two schemas can be compared column-by-column.
// Columns must line up by name; tolerated differences come back as
// warnings, hard mismatches as errors.
func Compatible(src, dst *Schema, o CompatOpts) ([]string, error) {
	if len(src.Cols) != len(dst.Cols) {
		return nil, fmt.Errorf("column count differs: src=%d (%s) dst=%d (%s)",
			len(src.Cols), colNames(src.Cols), len(dst.Cols), colNames(dst.Cols))
	}
	var warns []string
	for i, c := range src.Cols {
		d := dst.Cols[i]
		if c.Name != d.Name {
			return nil, fmt.Errorf("column %d names differ: src=%q dst=%q", i, c.Name, d.Name)
		}
		if err := compatibleCol(c, d, o, &warns); err != nil {
			return nil, fmt.Errorf("column %s incompatible: src=%s dst=%s: %v", c.Name, c.RawType, d.RawType, err)
		}
	}
	return warns, nil
}

func compatibleCol(c, d Column, o CompatOpts, warns *[]string) error {
	if o.Strict {
		if strings.EqualFold(c.RawType, d.RawType) {
			return nil
		}
		return fmt.Errorf("types differ (strict): %s vs %s", c.RawType, d.RawType)
	}
	if c.Family == d.Family {
		switch c.Family {
		case FamDECIMAL:
			if c.Precision != d.Precision || c.Scale != d.Scale {
				*warns = append(*warns, fmt.Sprintf(
					"column %s: decimal width differs (%s vs %s), values are compared after normalization", c.Name, c.RawType, d.RawType))
			}
		case FamSTR:
			if c.Collation != "" && d.Collation != "" && c.Collation != d.Collation {
				*warns = append(*warns, fmt.Sprintf(
					"column %s: collation differs (%s vs %s); byte-exact comparison in effect, use --fold-case to ignore case", c.Name, c.Collation, d.Collation))
			}
		}
		return nil
	}
	switch {
	case numeric(c.Family) && numeric(d.Family):
		*warns = append(*warns, fmt.Sprintf("column %s: numeric types differ (%s vs %s), compared after normalization", c.Name, c.RawType, d.RawType))
		return nil
	case (c.Family == FamDATETIME && d.Family == FamTIMESTAMP) || (c.Family == FamTIMESTAMP && d.Family == FamDATETIME):
		if o.AllowTZSwap {
			*warns = append(*warns, fmt.Sprintf("column %s: DATETIME/TIMESTAMP swap (%s vs %s), compared as UTC instants", c.Name, c.RawType, d.RawType))
			return nil
		}
		return fmt.Errorf("DATETIME/TIMESTAMP mismatch; pass --allow-tz-swap to compare as UTC instants")
	}
	return fmt.Errorf("type families differ: %s vs %s (use --ignore-columns to exclude)", c.Family, d.Family)
}

func numeric(f string) bool {
	switch f {
	case FamINT, FamUINT, FamDECIMAL, FamFLOAT, FamDOUBLE:
		return true
	}
	return false
}

func colNames(cols []Column) string {
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}
