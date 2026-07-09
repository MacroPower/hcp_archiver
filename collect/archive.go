package collect

import (
	"context"
	"fmt"
	"io"

	"github.com/MacroPower/tfc_archiver/manifest"
	"github.com/MacroPower/tfc_archiver/store"
	"github.com/MacroPower/tfc_archiver/tfeclient"
)

// Object archives a single immutable object at relPath.
//
// It consults the ledger first: a settled object (done, skipped, not-applicable,
// or permanently absent) returns without fetching, so an immutable artifact is
// never re-downloaded on a re-run. Otherwise it runs fetch, serializes and
// writes the result atomically, and records the object done with its content
// signature. A terminal fetch error (a 404 or 410) records the object as
// permanently absent; any other error records it as errored so a re-run retries
// it. Only a cancellation of ctx propagates; every other outcome is recorded and
// returns nil so one bad object never aborts the run.
func (e *Env) Object(ctx context.Context, relPath string, fetch func(context.Context) (any, error)) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	return e.archiveJSON(ctx, relPath, fetch)
}

// Mutable archives a single mutable metadata object at relPath.
//
// Unlike [Env.Object] it ignores the settled state and always re-reads, so a
// re-run refreshes cheap metadata (org, project, and workspace settings,
// variables, team access, tag bindings, notification configs, registry
// metadata) and the still-changing tail of a non-terminal run. The underlying
// write skips the on-disk update when the payload is unchanged, so a re-read
// that finds no change costs only the fetch. Error handling matches
// [Env.Object].
func (e *Env) Mutable(ctx context.Context, relPath string, fetch func(context.Context) (any, error)) error {
	return e.archiveJSON(ctx, relPath, fetch)
}

// Blob archives a single immutable blob at relPath, streaming it from fetch's
// reader rather than serializing it.
//
// It suits large raw artifacts (state blobs, plan and apply logs) that should
// not be buffered or redacted. Like [Env.Object] it skips a settled object and
// records the outcome; error handling matches [Env.Object].
func (e *Env) Blob(ctx context.Context, relPath string, fetch func(context.Context) (io.Reader, error)) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	r, err := fetch(ctx)
	if err != nil {
		return e.fail(ctx, relPath, err)
	}

	res, err := e.store.WriteReader(relPath, r)
	if err != nil {
		return e.failWrite(ctx, relPath, err)
	}

	e.recordDone(relPath, res)

	return nil
}

// Bytes archives a single immutable blob already held in memory at relPath.
//
// It suits raw artifacts the client buffers whole (configuration tarballs,
// policy source) that need no serialization. Like [Env.Object] it skips a
// settled object and records the outcome; error handling matches [Env.Object].
func (e *Env) Bytes(ctx context.Context, relPath string, fetch func(context.Context) ([]byte, error)) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	data, err := fetch(ctx)
	if err != nil {
		return e.fail(ctx, relPath, err)
	}

	res, err := e.store.WriteBytes(relPath, data)
	if err != nil {
		return e.failWrite(ctx, relPath, err)
	}

	e.recordDone(relPath, res)

	return nil
}

// archiveJSON runs fetch, serializes and writes the value, and records the
// object, the shared body of [Env.Object] and [Env.Mutable].
func (e *Env) archiveJSON(ctx context.Context, relPath string, fetch func(context.Context) (any, error)) error {
	v, err := fetch(ctx)
	if err != nil {
		return e.fail(ctx, relPath, err)
	}

	res, err := e.store.WriteJSON(relPath, v)
	if err != nil {
		return e.failWrite(ctx, relPath, err)
	}

	e.recordDone(relPath, res)

	return nil
}

// recordDone records a successful write and counts its bytes when the commit
// actually changed the on-disk content.
func (e *Env) recordDone(relPath string, res store.WriteResult) {
	e.ledger.RecordDone(relPath, manifest.Signature{
		Hash: res.SHA256,
		Size: res.Size,
	})

	if res.Changed {
		e.ledger.AddBytes(res.Size)
	}
}

// fail maps a fetch error onto a ledger status, the one place the client's
// transient-versus-terminal classification is turned into a recorded outcome.
//
// A cancellation of the passed context propagates so the run can wind down; a
// terminal error records permanent absence (sticky); anything else records an
// errored object, transient when the client classifies it so, so a re-run
// retries it and never mistakes a rate-limit blip for a gone object.
func (e *Env) fail(ctx context.Context, relPath string, cause error) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("archive %q: %w", relPath, ctxErr)
	}

	if tfeclient.IsTerminal(cause) {
		e.ledger.RecordAbsent(relPath)

		return nil
	}

	e.ledger.RecordErrored(relPath, cause, tfeclient.IsTransient(cause))

	return nil
}

// failWrite records a local write failure, distinct from a fetch error: it is
// never a permanent absence, so it records an errored (non-transient) object a
// re-run retries. A cancellation of the passed context still propagates.
func (e *Env) failWrite(ctx context.Context, relPath string, cause error) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("archive %q: %w", relPath, ctxErr)
	}

	e.ledger.RecordErrored(relPath, cause, false)

	return nil
}
