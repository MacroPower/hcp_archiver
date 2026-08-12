// Package history retains every superseded version of a mutable archive
// object in an append-only NDJSON sidecar beside it.
//
// A mutable object (workspace settings, variables, tag bindings, and their
// kin) is refreshed on every run, and a plain overwrite would discard the
// outgoing content. The sidecar closes that gap: when a changed payload is
// about to replace the file, the outgoing bytes are appended to the sidecar
// first ([Supersede]), so every byte sequence the archive ever held at the
// path stays recoverable: the newest from the file itself, every superseded
// one from the sidecar, in order. An object that disappears upstream keeps
// its last-known file and gains a tombstone record ([Bury]); one that comes
// back appends its returning content over the tombstone ([Restore]), keeping
// the timeline ordered. Nothing in a sidecar is ever rewritten or removed.
//
// Each record is one JSON line. A content record carries the superseded bytes
// verbatim as an escaped JSON string (the same convention as the seal
// roll-ups), so the recorded sha256 is taken over the exact original bytes and
// `jq -r '.content | fromjson'` reproduces the file. A tombstone record
// carries only the observation time and "deleted": true. Appends ride
// [go.jacobcolvin.com/hcp_archiver/atomicfile.Append], so a record commits on
// a newline boundary and a torn tail from a crash is repaired by the next
// append and ignored by reads.
//
// The append path is not safe for concurrent writers of one sidecar:
// [go.jacobcolvin.com/hcp_archiver/atomicfile.Append] truncates and rewrites
// the file's tail, so two concurrent appends could interleave destructively.
// The archive's one-object-one-call discipline (each object is archived by
// exactly one worker per run) is what provides the required single writer per
// sidecar.
//
// This is per-object content history, unrelated to the configuration's
// runHistory bound, which limits how many runs a workspace archives.
package history
