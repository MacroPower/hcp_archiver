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
// roll-ups), so the recorded sha256 is taken over the exact original bytes
// and `jq -r 'select(.content) | .content'` emits the file verbatim. A
// tombstone record carries only the observation time and "deleted": true.
// Appends ride
// [go.jacobcolvin.com/hcp_archiver/pkg/atomicfile.Append], so a record commits on
// a newline boundary and a torn tail from a crash is repaired by the next
// append and ignored by reads.
//
// A committed line that does not parse is skipped rather than failing the
// read, matching the resilience the archive's other NDJSON readers take
// against bit rot and partial restores. Failing instead would be far worse:
// the scan runs on the write path, so one damaged line would fail every
// future commit of the object and freeze the file it sits beside.
//
// Skipping is not free, and the cost is ordering rather than bytes. Every
// intact record stays on disk and every version stays recoverable, but the
// scans that place a new record read past the damage to an older one, so they
// can misjudge where the tail is: [Supersede] may re-record content the
// damaged line already carried, [Restore] may not see a damaged tombstone it
// should close, and damage to the record a [Bury] flushed can put the
// pre-deletion content back on top of an intact tombstone, where it reads as
// though it returned after the deletion. A sidecar with a damaged line is
// therefore a timeline to read skeptically, not one to trust for order.
//
// The damaged bytes stay on disk and can be recovered by hand, and a skip is
// announced rather than silent: a scan that walks past one warns through its
// logger (see [WithLogger]) with the sidecar's path, how many lines it
// skipped, and the file offset of the newest, which is enough to seek to the
// damage and read whatever is left of it. The warning is one aggregated
// record per scan, so a wholly rotted sidecar costs a line of log rather
// than thousands.
//
// It reports what a scan walked past, not what a sidecar holds. A scan stops
// at the record it was looking for and never reads the bytes behind it, so
// damage sitting under an intact answer goes unmentioned until some later
// scan reaches it. Auditing a sidecar means reading the whole file; this
// warning is narrower and more urgent than that, since it says a read the
// archive just performed was already reading past damage.
//
// The append path is not safe for concurrent writers of one sidecar:
// [go.jacobcolvin.com/hcp_archiver/pkg/atomicfile.Append] truncates and rewrites
// the file's tail, so two concurrent appends could interleave destructively.
// Three mechanisms above this package supply the required single writer. The
// archive's one-object-one-call discipline (each object is archived by exactly
// one worker per run) covers the objects that sit inside a single walk. The
// store serializes the commits that touch one sidecar against each other (see
// [go.jacobcolvin.com/hcp_archiver/pkg/store.WithHistory]), which is what holds the
// line for an object no single walk owns. And the collectors claim an object
// several of them address at once (a user, hydrated from teams, run creators,
// and event actors alike) for the run, so the crowd reaching for it collapses
// to one writer before it ever gets here.
//
// The store's serialization covers the sidecar alone. The ledger entry
// recording an object's signature is written outside it, so an object written
// from two places can still finish with the ledger describing one version and
// the file holding another: an intact timeline is not the same as a safely
// shared object.
//
// This is per-object content history, unrelated to the configuration's
// runHistory bound, which limits how many runs a workspace archives.
package history
