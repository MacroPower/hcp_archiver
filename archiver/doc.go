// Package archiver is the orchestrator and composition root of an archive run.
//
// From a validated configuration it constructs the shared client, store,
// ledger, progress reporter, collection environment, and every domain
// collector, then drives the walk. It enumerates the organizations the token can
// see (all of them when no organization is named) and, for each, archives the
// directly-owned org-level objects once, walks that organization's projects in
// order, fans its workspaces across a shared worker pool, always gathers the
// registry surface (deepened by the registry-detail toggle), and adds the
// optional stacks and audit surfaces as their toggles allow.
// Because each organization has its own archive tree and manifest, a fresh
// store and ledger are built per organization.
//
// The worker pool bounds in-flight API requests, not workspaces: every request
// takes a slot through the client's gate, so the same slots serve many small
// workspaces or many pieces of one large workspace, whichever is ready. A
// controller scales the pool between one worker and the configured ceiling from
// observed rate limiting, the way TCP finds a path's capacity: it doubles on
// each clean window until the first 429 (slow start), then halves on any window
// that saw 429s and adds one worker per clean window thereafter, so the run
// ramps up quickly, sheds load when the server pushes back, and recovers on its
// own.
//
// It owns the cross-cutting runtime and nothing else: the worker pool and its
// controller, the ledger-flush and progress tickers, graceful shutdown that
// flushes the ledger on a signal, and the closing run record. It holds no
// per-object API knowledge of its own; that lives in the collectors it
// schedules. Its responsibility is purely which work runs, in what order, and
// with how much concurrency.
//
// # Guarantees and scope
//
// A run is best-effort rather than fail-fast: a permanently-gone object is
// recorded and the walk continues, so one missing log never aborts the archive.
// It is not point-in-time consistent: a long run against a live organization
// sees new runs and state versions appear mid-walk, so it captures each
// collection's delta as of when it reaches that collection, not a single
// instant.
//
// Several surfaces are deliberately out of scope and recorded as not-applicable
// so they are not mistaken for gaps: the Explorer (a derived CSV view),
// ephemeral agents and query runs, test runs and their variables, and the admin
// endpoints (Terraform Enterprise only, which answer 401 or 404 on HCP).
// Billing, entitlements, and subscription tier are platform-managed with no
// export, and native soft-delete recovery is not attempted.
package archiver
