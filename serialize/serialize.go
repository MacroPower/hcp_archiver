package serialize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/jsonapi"
)

const indent = "  "

// ErrMarshal wraps any lower-level marshaling problem so callers can classify
// a serialization fault with [errors.Is].
var ErrMarshal = fmt.Errorf("serialize")

// Marshal renders v as indented JSON suitable for storage in the archive.
//
// The output is byte-faithful to the value passed in: nothing is redacted or
// stripped, so the archive records exactly what the API returned (see the
// package documentation for the at-rest implications). A model carrying a
// jsonapi primary field is marshaled through HashiCorp's vendored jsonapi
// encoder, which yields kebab-case keys and renders relations as ids; every
// other type falls back to [encoding/json].
func Marshal(v any) ([]byte, error) {
	v = addressable(v)

	if isJSONAPIModel(v) {
		return marshalJSONAPI(v)
	}

	// Encode with HTML escaping off so &, <, and > in a stored value survive byte
	// for byte rather than turning into \u escapes -- the byte-faithful contract
	// this package documents. The default json.MarshalIndent escapes them; a
	// json.Encoder with SetEscapeHTML(false) does not. Encode appends a trailing
	// newline that MarshalIndent omits, so trim it to keep the output byte-stable.
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)

	err := enc.Encode(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMarshal, err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// addressable returns v unchanged unless it is a non-pointer struct value, in
// which case it returns a pointer to a copy: the jsonapi encoder requires a
// pointer to the model it marshals.
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
//
// It builds the payload with [jsonapi.Marshal] and drops the included relations
// itself rather than calling [jsonapi.MarshalPayloadWithoutIncluded], so the
// final encode can run with HTML escaping off: the node attributes are native Go
// values encoded here, so &, <, and > in a stored value survive byte for byte
// instead of being rewritten to &, <, >.
func marshalJSONAPI(v any) ([]byte, error) {
	payload, err := jsonapi.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMarshal, err)
	}

	switch p := payload.(type) {
	case *jsonapi.OnePayload:
		p.Included = nil
	case *jsonapi.ManyPayload:
		p.Included = nil
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	err = enc.Encode(payload)
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

	// The jsonapi encoder marshals a struct pointer or a slice of them, never a
	// Go array, so only a slice element is followed here; an array falls through
	// to the struct check below and, failing it, to the encoding/json path.
	if t.Kind() == reflect.Slice {
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
