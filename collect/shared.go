package collect

import (
	"context"
	"fmt"
)

// sharedOnce is one shared object's in-flight or finished archive: done closes
// when the claimant returns, publishing err to everyone waiting on it.
type sharedOnce struct {
	done chan struct{}
	err  error
}

// ArchiveShared archives the object at relPath through archive, letting one
// caller write it however many collections address it, and hands the callers
// waiting on that write the same outcome.
//
// An object addressed from more than one collection has no single owning walk,
// so the one-object-one-call discipline the rest of the archive rests on does
// not reach it: a user hydrated as a team member, as a run's created-by, and as
// a run event's actor resolves to one users/<id>.json that the org-scoped and
// workspace collectors both write, concurrently and repeatedly. This supplies
// the missing discipline. The first caller to name relPath archives while the
// others wait, and the waiters return the claimant's error.
//
// The claim is held once the archive leaves the ledger entry for relPath
// settled, so the repeats that follow (the same creator on a thousand runs)
// cost nothing. Its lifetime is read from that entry and no other, so archive
// must be what settles relPath rather than something recording its outcome
// elsewhere.
//
// An archive that leaves the entry unsettled releases the claim instead, and
// the next caller to name relPath re-archives it. That retry is the only one
// the run has: the reference gate a caller mirrors the write into
// ([Env.Reference]) keeps the failure visible to a later run's walk, but
// nothing re-drives the current one, so a claim outliving the failure would
// hand every remaining reference the same error and leave the gate open with no
// further attempt behind it. Only a caller arriving after the claimant returned
// ever re-archives, so the writers still never overlap.
//
// Callers wait rather than skip because the outcome is part of the contract. A
// caller that mirrors the write into a reference gate reads the ledger
// immediately afterwards; skipping ahead of the claimant's write would open the
// gate over an object that is about to settle, leaving the referencing walk
// unsettled over work that in fact completed.
//
// The memoized outcome is the claimant's, recorded under the claimant's
// context, so a claimant torn down mid-write publishes its cancellation to
// every waiter. Every caller of one object descends from the same
// organization's run, so a cancellation reaching one reaches all. A waiter
// whose own context ends first stops waiting and reports that instead, leaving
// the claim standing for the claimant to finish or abandon with the run.
//
// The archive closure must not itself call ArchiveShared for relPath, which
// would wait on its own claim until the context ends.
func (e *Env) ArchiveShared(ctx context.Context, relPath string, archive func(context.Context) error) error {
	shared, claimed := e.claimShared(relPath)

	if claimed {
		shared.err = archive(ctx)
		close(shared.done)
		e.releaseShared(relPath)

		return shared.err
	}

	select {
	case <-shared.done:
		return shared.err
	case <-ctx.Done():
		return fmt.Errorf("archive %q: %w", relPath, ctx.Err())
	}
}

// claimShared returns the memo for relPath, creating one when this is the
// first caller to name it, and reports whether this caller made the claim and
// so owns the archive.
func (e *Env) claimShared(relPath string) (*sharedOnce, bool) {
	e.sharedMu.Lock()
	defer e.sharedMu.Unlock()

	shared, ok := e.shared[relPath]
	if ok {
		return shared, false
	}

	shared = &sharedOnce{done: make(chan struct{})}
	e.shared[relPath] = shared

	return shared, true
}

// releaseShared drops the claim on relPath when the archive it covered left the
// object unsettled, so the next caller retries it rather than inheriting a
// failure the run is still trying to recover from. A settled object keeps its
// claim, which is what makes the repeats free.
//
// Only the claimant releases, and only once the archive has returned, so the
// entry it deletes is always its own: a caller reaching the map in the window
// between the two finds the finished claim and waits on it rather than
// installing a second one.
func (e *Env) releaseShared(relPath string) {
	if !e.ledger.ShouldFetch(relPath) {
		return
	}

	e.sharedMu.Lock()
	defer e.sharedMu.Unlock()

	delete(e.shared, relPath)
}
