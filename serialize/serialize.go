package serialize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/jsonapi"
)

// Redacted is the marker written over a sensitive or write-only field by the
// safety pass. The value is set to this marker rather than blanked so a
// redacted secret stays distinguishable from a genuinely empty field.
const Redacted = "[REDACTED]"

const indent = "  "

// ErrMarshal wraps any lower-level marshaling problem so callers can classify
// a serialization fault with [errors.Is].
var ErrMarshal = fmt.Errorf("serialize")

// Marshal runs the safety pass over v and renders it as indented JSON suitable
// for storage in the archive.
//
// The safety pass redacts sensitive and write-only fields and strips ephemeral
// signed URLs; see the package documentation for the exact policy. A model
// carrying a jsonapi primary field is marshaled through HashiCorp's vendored
// jsonapi encoder, which yields kebab-case keys and renders relations as ids;
// every other type falls back to [encoding/json].
//
// Marshal mutates the struct that v points at (and each element of a slice) in
// place: the redaction and URL stripping overwrite fields on the value passed
// in. Callers pass freshly-listed objects, so this is safe by construction.
func Marshal(v any) ([]byte, error) {
	v = addressable(v)

	sanitize(reflect.ValueOf(v), map[uintptr]bool{})

	if isJSONAPIModel(v) {
		return marshalJSONAPI(v)
	}

	out, err := json.MarshalIndent(v, "", indent)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMarshal, err)
	}

	return out, nil
}

// addressable returns v unchanged unless it is a non-pointer struct value, in
// which case it returns a pointer to an addressable copy.
//
// The safety pass sets fields through reflection, which requires an addressable
// value; a struct passed by value yields unaddressable fields, so without this
// its secrets and ephemeral URLs would silently survive. Slice, array, and
// pointer values already expose addressable elements, so they pass through.
func addressable(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return v
	}

	p := reflect.New(rv.Type())
	p.Elem().Set(rv)

	return p.Interface()
}

// marshalJSONAPI renders v through the vendored jsonapi encoder without the
// sideloaded "included" array, so hydrated relations serialize as id refs, then
// re-indents the compact output.
func marshalJSONAPI(v any) ([]byte, error) {
	var buf bytes.Buffer

	err := jsonapi.MarshalPayloadWithoutIncluded(&buf, v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMarshal, err)
	}

	var out bytes.Buffer

	err = json.Indent(&out, bytes.TrimSpace(buf.Bytes()), "", indent)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMarshal, err)
	}

	return out.Bytes(), nil
}

// isJSONAPIModel reports whether v (following pointers, and into a slice
// element) is a struct type carrying a jsonapi primary field.
func isJSONAPIModel(v any) bool {
	t := reflect.TypeOf(v)
	if t == nil {
		return false
	}

	t = deref(t)

	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = deref(t.Elem())
	}

	if t.Kind() != reflect.Struct {
		return false
	}

	for f := range t.Fields() {
		tag := f.Tag.Get("jsonapi")
		if tag == "primary" || strings.HasPrefix(tag, "primary,") {
			return true
		}
	}

	return false
}

// deref returns the element type behind any chain of pointers.
func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t
}

// sanitize walks rv, following pointers and into slice, array, and interface
// elements, and applies the safety pass to every struct it reaches. The seen set
// records visited pointers so a cycle in a hydrated model graph terminates.
func sanitize(rv reflect.Value, seen map[uintptr]bool) {
	if !rv.IsValid() {
		return
	}

	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return
		}

		ptr := rv.Pointer()
		if seen[ptr] {
			return
		}

		seen[ptr] = true

		sanitize(rv.Elem(), seen)

	case reflect.Interface:
		if rv.IsNil() {
			return
		}

		sanitize(rv.Elem(), seen)

	case reflect.Slice, reflect.Array:
		for i := range rv.Len() {
			sanitize(rv.Index(i), seen)
		}

	case reflect.Map:
		if rv.IsNil() {
			return
		}

		ptr := rv.Pointer()
		if seen[ptr] {
			return
		}

		seen[ptr] = true

		for _, k := range rv.MapKeys() {
			mv := rv.MapIndex(k)

			// Map values are not addressable, so the safety pass cannot set
			// through them in place. Sanitize an addressable copy and store it
			// back, so a secret reachable only through a map is redacted rather
			// than written in cleartext.
			cp := reflect.New(mv.Type()).Elem()
			cp.Set(mv)
			sanitize(cp, seen)
			rv.SetMapIndex(k, cp)
		}

	case reflect.Struct:
		sanitizeStruct(rv, seen)

	default:
	}
}

// sanitizeStruct redacts sensitive and write-only fields and strips ephemeral
// URLs on a single struct value, recognizing the go-tfe field names rather than
// enumerating concrete types. Every other field is walked recursively so a
// secret carried in a nested struct, pointer, or slice is redacted too.
func sanitizeStruct(rv reflect.Value, seen map[uintptr]bool) {
	rt := rv.Type()
	sensitive := hasSensitiveValue(rv)

	for i := range rt.NumField() {
		fv := rv.Field(i)
		if !fv.CanSet() {
			continue
		}

		name := rt.Field(i).Name

		switch {
		case name == "Token", name == "Secret", name == "HMACKey":
			setString(fv, Redacted)
		case name == "Value" && sensitive:
			redactValue(fv)
		case isEphemeralURL(name):
			clearString(fv)
		default:
			sanitize(fv, seen)
		}
	}
}

// isEphemeralURL reports whether a field name matches a short-lived signed URL:
// any download or upload URL, or the log-read URL on a hydrated plan or apply.
func isEphemeralURL(name string) bool {
	return strings.HasSuffix(name, "DownloadURL") ||
		strings.HasSuffix(name, "UploadURL") ||
		name == "LogReadURL"
}

// hasSensitiveValue reports whether the struct carries a truthy Sensitive
// field, gating redaction of a Value field to sensitive variables, variable-set
// variables, and policy-set parameters.
func hasSensitiveValue(rv reflect.Value) bool {
	f := rv.FieldByName("Sensitive")
	if !f.IsValid() {
		return false
	}

	switch f.Kind() {
	case reflect.Bool:
		return f.Bool()
	case reflect.Pointer:
		return !f.IsNil() && f.Elem().Kind() == reflect.Bool && f.Elem().Bool()
	default:
		return false
	}
}

// redactValue overwrites a sensitive Value field of any kind, failing closed so
// no cleartext survives when the field is not a plain string.
//
// A string or *string takes the [Redacted] sentinel (a nil *string is allocated
// so its existence is still recorded); an interface takes the sentinel when a
// string satisfies it, else is zeroed; any other kind is zeroed. A plain string
// Value redacts identically to [setString], so string-Value output stays
// byte-for-byte unchanged. This is the safety-critical redactor, where a
// silently-unredacted non-string secret would be worse than a blanked field.
func redactValue(fv reflect.Value) {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(Redacted)

	case reflect.Pointer:
		if fv.Type().Elem().Kind() != reflect.String {
			fv.Set(reflect.Zero(fv.Type()))

			return
		}

		p := reflect.New(fv.Type().Elem())
		p.Elem().SetString(Redacted)
		fv.Set(p)

	case reflect.Interface:
		if reflect.TypeFor[string]().Implements(fv.Type()) {
			fv.Set(reflect.ValueOf(Redacted))

			return
		}

		fv.Set(reflect.Zero(fv.Type()))

	default:
		fv.Set(reflect.Zero(fv.Type()))
	}
}

// setString overwrites a string or *string field with s, allocating a pointer
// when needed so a nil secret still records its existence as redacted. Any other
// kind is zeroed rather than left as it was, so a Token, Secret, or HMACKey of
// an unexpected type fails closed instead of surviving in cleartext, matching
// [redactValue]. Its only caller guarantees the field is settable.
func setString(fv reflect.Value, s string) {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Pointer:
		if fv.Type().Elem().Kind() != reflect.String {
			fv.Set(reflect.Zero(fv.Type()))

			return
		}

		p := reflect.New(fv.Type().Elem())
		p.Elem().SetString(s)
		fv.Set(p)

	default:
		fv.Set(reflect.Zero(fv.Type()))
	}
}

// clearString zeroes a string or *string field so an omitempty tag drops the
// ephemeral URL it held.
func clearString(fv reflect.Value) {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString("")
	case reflect.Pointer:
		if fv.Type().Elem().Kind() == reflect.String {
			fv.Set(reflect.Zero(fv.Type()))
		}

	default:
	}
}
