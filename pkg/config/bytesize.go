package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"go.jacobcolvin.com/x/jsonschema"
)

var (
	// ErrInvalidByteSize indicates a byte-size value that is not a plain
	// non-negative integer or a suffixed size string [ParseByteSize] accepts.
	ErrInvalidByteSize = errors.New("invalid byte size")

	// The anchored grammar of the suffixed size string: an unsigned decimal
	// number with an optional decimal or binary unit. It is the same pattern
	// the configuration schema enforces on the string form.
	byteSizePattern = regexp.MustCompile(`^\d+(\.\d+)?(B|[KMGT]B|[KMGT]iB)?$`)

	// Each size suffix mapped to its multiplier: decimal units are powers of
	// 1000, binary units powers of 1024, and "B" or no suffix is bytes.
	byteSizeUnits = map[string]int64{
		"":    1,
		"B":   1,
		"KB":  1000,
		"MB":  1000 * 1000,
		"GB":  1000 * 1000 * 1000,
		"TB":  1000 * 1000 * 1000 * 1000,
		"KiB": 1024,
		"MiB": 1024 * 1024,
		"GiB": 1024 * 1024 * 1024,
		"TiB": 1024 * 1024 * 1024 * 1024,
	}

	// The binary suffixes largest first, the order [ByteSize.String] probes
	// for an exact rendering.
	binaryUnits = []struct {
		suffix string
		size   int64
	}{
		{"TiB", 1024 * 1024 * 1024 * 1024},
		{"GiB", 1024 * 1024 * 1024},
		{"MiB", 1024 * 1024},
		{"KiB", 1024},
	}
)

// ByteSize is a byte count decoded from a plain non-negative integer or a
// suffixed string such as "64MiB". Decimal suffixes (KB, MB, GB, TB) are
// powers of 1000, binary suffixes (KiB, MiB, GiB, TiB) are powers of 1024,
// and "B" or no suffix means bytes. Suffixes are case-sensitive. A fractional
// number is accepted when the result is a whole number of bytes ("1.5GiB",
// but not "1.5B"). Convert to a plain count with int64(b).
type ByteSize int64

// ParseByteSize parses s in the byte-size syntax described on [ByteSize]. It
// returns [ErrInvalidByteSize] wrapped with the offending value when s does
// not conform, names a fractional byte count, or overflows.
func ParseByteSize(s string) (ByteSize, error) {
	if !byteSizePattern.MatchString(s) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidByteSize, s)
	}

	num := strings.TrimRight(s, "BKMGTi")
	unit := s[len(num):]

	// The pattern guarantees the suffix is a known unit and the number a
	// plain unsigned decimal; big.Rat keeps fractional values exact, so
	// "1.1KB" is precisely 1100 and integrality and overflow are decided
	// without float rounding.
	r, ok := new(big.Rat).SetString(num)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrInvalidByteSize, s)
	}

	r.Mul(r, new(big.Rat).SetInt64(byteSizeUnits[unit]))

	if !r.IsInt() {
		return 0, fmt.Errorf("%w: %q: not a whole number of bytes", ErrInvalidByteSize, s)
	}

	if !r.Num().IsInt64() {
		return 0, fmt.Errorf("%w: %q: value overflows", ErrInvalidByteSize, s)
	}

	return ByteSize(r.Num().Int64()), nil
}

// String renders the value with the largest binary unit that divides it
// exactly ("64MiB") and falls back to plain digits, so [ParseByteSize]
// round-trips the result.
func (b ByteSize) String() string {
	if b > 0 {
		for _, unit := range binaryUnits {
			if int64(b)%unit.size == 0 {
				return strconv.FormatInt(int64(b)/unit.size, 10) + unit.suffix
			}
		}
	}

	return strconv.FormatInt(int64(b), 10)
}

// UnmarshalYAML decodes the YAML scalar as either an integer byte count or a
// suffixed size string. A bad value is returned as a token-carrying YAML
// error, so the loader reports it against the offending source line.
func (b *ByteSize) UnmarshalYAML(node ast.Node) error {
	var v any

	err := yaml.NodeToValue(node, &v)
	if err != nil {
		//nolint:wrapcheck // The YAML error carries the useful context.
		return err
	}

	fail := func(err error) error {
		return &yaml.SyntaxError{Message: err.Error(), Token: node.GetToken()}
	}

	switch t := v.(type) {
	case string:
		parsed, err := ParseByteSize(t)
		if err != nil {
			return fail(err)
		}

		*b = parsed

	case int64:
		if t < 0 {
			return fail(fmt.Errorf("%w: %d: must not be negative", ErrInvalidByteSize, t))
		}

		*b = ByteSize(t)

	case uint64:
		if t > math.MaxInt64 {
			return fail(fmt.Errorf("%w: %d: value overflows", ErrInvalidByteSize, t))
		}

		*b = ByteSize(t)

	default:
		return fail(fmt.Errorf("%w: %v: must be an integer or a size string", ErrInvalidByteSize, v))
	}

	return nil
}

// JSONSchema describes both accepted scalar forms, replacing the schema that
// would be reflected from the underlying integer. It implements the schema
// generator's provider interface, so the generated configuration schema
// carries one shared definition for the type.
func (ByteSize) JSONSchema(_ context.Context, _ jsonschema.TypeContext) (*jsonschema.Schema, error) {
	minimum := 0.0

	return &jsonschema.Schema{
		Title: "Byte Size",
		Description: "A byte count: a plain integer number of bytes, or a string with a " +
			"decimal (KB, MB, GB, TB) or binary (KiB, MiB, GiB, TiB) suffix, such as 64MiB.",
		OneOf: []*jsonschema.Schema{
			{Type: "integer", Minimum: &minimum},
			{Type: "string", Pattern: byteSizePattern.String()},
		},
	}, nil
}
