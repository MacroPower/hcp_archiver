// Package serialize turns a go-tfe object into the bytes stored in the archive.
//
// The output is byte-faithful to what the API returned: nothing is redacted,
// stripped, or rewritten, so the archive is a full-fidelity record of the
// organization. The corollary is that the archive holds whatever secret
// material the API chooses to return (a notification configuration's token,
// signed artifact URLs, and the cleartext values raw state blobs already
// embed), and must be treated as sensitive at rest, which is why the store
// writes every file owner-only (0600).
//
// # Two marshalers
//
// Most go-tfe types are jsonapi response structs tagged for HashiCorp's
// vendored jsonapi encoder rather than for encoding/json. Types that carry a
// jsonapi primary field are marshaled through that vendored encoder, which
// yields stable kebab-case keys matching the public API documentation, renders
// relations as ids, and honors omitempty. The plain audit-trail and pagination
// types lack a primary field, so they fall back to encoding/json. Kebab-case is
// preferred over encoding/json's Go-field-name fallback because a field rename
// in a go-tfe upgrade would silently drift a Go-field-name schema, whereas the
// kebab names track the documented API and are the more stable choice for a
// long-lived reference archive.
//
// # Relations
//
// A hydrated relation pointer marshals as a full nested object when included and
// as null when not, so the same field varies in shape and repeats data stored
// elsewhere. Relations are rendered as ids wherever practical, so the archived
// shape is stable and compact.
package serialize
