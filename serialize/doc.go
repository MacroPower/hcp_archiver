// Package serialize turns a go-tfe object into the bytes stored in the archive,
// and is the single pass that makes those bytes safe to keep at rest.
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
// # Redaction
//
// Because neither marshaler offers tag-driven omission, sensitive values are
// overwritten with a [REDACTED] marker on the struct before marshaling. The
// value is set to that marker rather than blanked, so a redacted secret stays
// distinguishable from a genuinely empty field. This covers sensitive variable,
// variable-set, and policy-set-parameter values, plus every write-only secret
// the API returns blank anyway: team, organization, agent, and user token
// secrets, an OAuth client secret, a run task's HMAC key, and a notification
// configuration's token. Each is recorded as redacted, so the archive keeps the
// object's existence and metadata without its secret. (Raw state blobs are a
// separate matter: they are written unredacted and embed sensitive values in
// cleartext, which is why the archive as a whole is sensitive at rest.)
//
// # Ephemeral URLs
//
// Every short-lived signed URL is stripped by field-name pattern rather than by
// enumeration: both the download URLs and the upload URLs (including the JSON
// and sanitized-state upload URLs) and the log-read URL on a hydrated plan or
// apply relation. They expire within minutes, can embed tokens, and merely
// duplicate blobs the archive already captures. A run therefore records a
// configuration version by id and keeps its ingress attributes, never the
// expiring upload URL a hydrated relation would otherwise nest. The provider
// shasum URLs need no handling because they are methods, not struct fields, and
// so are never serialized.
//
// # Relations
//
// A hydrated relation pointer marshals as a full nested object when included and
// as null when not, so the same field varies in shape and repeats data stored
// elsewhere. Relations are rendered as ids wherever practical, so the archived
// shape is stable and compact.
//
// Keeping redaction, URL stripping, and relation handling here means one safety
// pass covers every object type and no caller can persist an object that skipped
// it.
package serialize
