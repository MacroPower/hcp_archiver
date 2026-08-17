package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"go.jacobcolvin.com/x/jsonschema"
)

var (
	// ErrInvalidDuration indicates a duration string that is not in the
	// extended duration syntax [ParseDuration] accepts.
	ErrInvalidDuration = errors.New("invalid duration")

	// The anchored grammar of the extended duration syntax: a bare "0", or one
	// or more unsigned decimal numbers, each with a unit suffix.
	// [Duration.JSONSchema] emits it into the configuration schema, so a value
	// that decodes is a value the schema admits.
	durationPattern = regexp.MustCompile(`^(0|(\d+(\.\d+)?(ns|us|µs|ms|s|m|h|d))+)$`)

	// One number-and-unit segment of a duration string validated by
	// durationPattern.
	durationSegment = regexp.MustCompile(`\d+(\.\d+)?(ns|us|µs|ms|s|m|h|d)`)
)

// Duration is a [time.Duration] decoded from Go duration syntax extended with
// a day unit: "d", one day being exactly 24 hours. The day unit composes like
// any other, so "90d", "1d12h", and "1.5d" are all valid. A plain conversion
// to [time.Duration] recovers the standard type.
//
// Values are unsigned, and the syntax is slightly stricter than
// [time.ParseDuration]: no sign, and microseconds spell as "us" or the micro
// sign "µs", not the Greek mu "μs". A bare "0", or the number 0 in YAML, is
// the zero duration.
type Duration time.Duration

// ParseDuration parses s in the extended duration syntax described on
// [Duration]. It returns [ErrInvalidDuration] wrapped with the offending
// value when s does not conform or the result overflows.
func ParseDuration(s string) (Duration, error) {
	if !durationPattern.MatchString(s) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDuration, s)
	}

	// The whole string is validated above, so it is exactly a run of
	// number-unit segments; parse each one alone and sum. A day segment is
	// re-read as hours and scaled by 24 in integer nanoseconds, keeping
	// fractional days exact rather than reformatting through a float.
	var total time.Duration

	for _, seg := range durationSegment.FindAllString(s, -1) {
		day := seg[len(seg)-1] == 'd'
		if day {
			seg = seg[:len(seg)-1] + "h"
		}

		v, err := time.ParseDuration(seg)
		if err != nil {
			return 0, fmt.Errorf("%w: %q: %w", ErrInvalidDuration, s, err)
		}

		if day {
			if v > math.MaxInt64/24 {
				return 0, fmt.Errorf("%w: %q: value overflows", ErrInvalidDuration, s)
			}

			v *= 24
		}

		total += v
		if total < 0 {
			return 0, fmt.Errorf("%w: %q: value overflows", ErrInvalidDuration, s)
		}
	}

	return Duration(total), nil
}

// String renders whole-day multiples as "<n>d" and every other value in Go
// duration syntax, so [ParseDuration] round-trips the result.
func (d Duration) String() string {
	v := time.Duration(d)
	if v != 0 && v%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", v/(24*time.Hour))
	}

	return v.String()
}

// JSONSchema describes the extended duration string, replacing the schema
// that would be reflected from the underlying integer. It implements the
// schema generator's provider interface, so the generated configuration
// schema carries one shared definition for the type.
func (Duration) JSONSchema(_ context.Context, _ jsonschema.TypeContext) (*jsonschema.Schema, error) {
	zero := any(0)

	return &jsonschema.Schema{
		Title: "Duration",
		Description: "A duration: a string in Go duration syntax extended with a day unit, " +
			"such as 90d, 36h, or 1d12h, or 0 for the zero duration.",
		OneOf: []*jsonschema.Schema{
			{Type: "string", Pattern: durationPattern.String()},
			{Const: &zero},
		},
	}, nil
}

// UnmarshalYAML decodes the YAML scalar as an extended duration string, or as
// the number 0 for the zero duration. A named duration type does not inherit
// the YAML decoder's built-in [time.Duration] handling, so the type carries
// its own. A bad value is returned as a token-carrying YAML error, so the
// loader reports it against the offending source line.
func (d *Duration) UnmarshalYAML(node ast.Node) error {
	var v any

	err := yaml.NodeToValue(node, &v)
	if err != nil {
		//nolint:wrapcheck // The YAML error carries the useful context.
		return err
	}

	fail := func() error {
		return &yaml.SyntaxError{
			Message: fmt.Sprintf("%v: %v: must be a duration string or 0", ErrInvalidDuration, v),
			Token:   node.GetToken(),
		}
	}

	switch t := v.(type) {
	case string:
		parsed, err := ParseDuration(t)
		if err != nil {
			return &yaml.SyntaxError{Message: err.Error(), Token: node.GetToken()}
		}

		*d = parsed

	case int64, uint64, float64:
		// The schema's zero constant matches a YAML 0 or 0.0 numerically, so
		// the decoder agrees: a numeric zero is the zero duration, and any
		// other number must carry a unit.
		if t != int64(0) && t != uint64(0) && t != float64(0) {
			return fail()
		}

		*d = 0

	default:
		return fail()
	}

	return nil
}
