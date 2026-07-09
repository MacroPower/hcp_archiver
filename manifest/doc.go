// Package manifest maintains the durable ledger that makes an archive run
// resumable, re-runnable, and observable. It is a real per-object record, not
// file-existence: a permanently-gone object and a not-yet-fetched one both leave
// no file, so existence alone cannot drive resume.
//
// # What it records
//
// For each object, keyed by its archive-relative path as an opaque string, the
// ledger stores a status (done, permanently absent, skipped, errored, forbidden,
// or not applicable) with counts, timestamps, the failing error, a fetch time,
// and a content signature (a size or hash). Failures record whether they were
// transient or terminal. Alongside the per-object entries it holds a high-water
// mark per append-mostly collection and a summary record per run (the last run
// time, a run count, and per-status totals). Keying on the relative path keeps
// the ledger independent of both the filesystem layout and the API client.
//
// # Resume
//
// From this state the ledger decides, per object, whether the current pass
// should fetch it: settled work (done, and sticky permanent absence) is skipped,
// while errored and forbidden objects and anything absent from the ledger are
// retried. Because permanent absence is sticky, a 404 or 410 is not re-requested
// every run; an absent-recheck toggle forces re-probing when an operator
// suspects a since-restored object. Distinguishing transient failures from
// terminal ones is what keeps a rate-limit blip from being mistaken for
// permanent absence. A first run
// starts from an empty ledger, so resume and a clean start are one code path,
// and an interrupted run and a failed one are indistinguishable to it: both
// leave objects errored or absent, and both are picked up next time.
//
// # Incremental re-run
//
// Re-running a completed archive fetches only what changed, along two lines.
// Append-mostly collections (state versions, runs and their children,
// configuration versions) key on the high-water mark: a re-run lists newest-first
// and stops once it reaches an already-archived object that has frozen into a
// terminal state, so new objects append while immutable artifacts are never
// re-fetched. A known but still-non-terminal run is revisited so its mutable
// tail is refreshed until it reaches a terminal state and is then frozen.
// Audit-trail pages are the exception: with no per-entry id to stop at, their
// high-water mark is a Since time cursor that the re-run walks forward from.
// Mutable objects with no natural watermark (settings, variables, team
// access, tag bindings, notification configurations, registry metadata) are
// re-read and atomically overwritten only when the content signature shows the
// payload differs; that overwrite records a fresh fetch time. Cheap metadata is
// always refreshed; heavy blobs never are.
//
// # Durability and the shared tally
//
// The ledger flushes itself durably at a bounded cadence and on shutdown, so a
// hard kill loses at most the last in-flight batch, never the ledger itself. It
// also guards the single mutex-protected tally that both the run summary and the
// live progress reporter read, so the reported counts and the ledger's own tally
// can never drift apart (the on-disk copy trails only by the last unflushed
// batch). Deferred objects are recorded as not-applicable so they
// are never mistaken for gaps.
package manifest
