package view

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ErrDecode wraps any problem decoding an archived JSON document so callers can
// classify a malformed file with [errors.Is].
var ErrDecode = fmt.Errorf("decode archive document")

// Resource is one object from an archived JSON:API document: its identity and
// its attributes as the kebab-case keys the archive stores.
//
// The archive marshals go-tfe models through the jsonapi encoder, so every
// metadata file is a JSON:API document whose payload sits under "data". Parsing
// it generically keeps the browser independent of the go-tfe structs: a field
// the encoder renames still displays, just under its new key.
//
// Instances are produced by [DecodeResources].
type Resource struct {
	Attributes map[string]any `json:"attributes"`
	ID         string         `json:"id"`
	Type       string         `json:"type"`
}

// document is the envelope an archived JSON:API file carries: a payload that is
// one resource or a list of them.
type document struct {
	Data json.RawMessage `json:"data"`
}

// DecodeResources parses an archived JSON:API document into its resources,
// accepting both the single-object and list payload shapes.
func DecodeResources(data []byte) ([]Resource, error) {
	var doc document

	err := json.Unmarshal(data, &doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecode, err)
	}

	payload := bytes.TrimSpace(doc.Data)
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		return nil, nil
	}

	if payload[0] == '[' {
		var many []Resource

		err = json.Unmarshal(payload, &many)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDecode, err)
		}

		return many, nil
	}

	var one Resource

	err = json.Unmarshal(payload, &one)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecode, err)
	}

	return []Resource{one}, nil
}

// String returns the attribute named key as a string, or "" when it is absent
// or not a string.
func (r *Resource) String(key string) string {
	if s, ok := r.Attributes[key].(string); ok {
		return s
	}

	return ""
}

// Bool returns the attribute named key as a bool, or false when it is absent or
// not a bool.
func (r *Resource) Bool(key string) bool {
	if b, ok := r.Attributes[key].(bool); ok {
		return b
	}

	return false
}

// Int returns the attribute named key as an int64, or 0 when it is absent or
// not numeric. JSON numbers decode as float64, the only numeric shape to
// handle.
func (r *Resource) Int(key string) int64 {
	if f, ok := r.Attributes[key].(float64); ok {
		return int64(f)
	}

	return 0
}

// Time returns the attribute named key parsed as an RFC 3339 timestamp, or the
// zero time when it is absent or unparseable. The jsonapi encoder renders
// iso8601-tagged times in this layout.
func (r *Resource) Time(key string) time.Time {
	t, err := time.Parse(time.RFC3339, r.String(key))
	if err != nil {
		return time.Time{}
	}

	return t
}
