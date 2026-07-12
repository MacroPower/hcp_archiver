package collect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// Object archives a single immutable object at relPath.
//
// It consults the ledger first: a settled object (done, skipped, not-applicable,
// or permanently absent) returns without fetching, so an immutable artifact is
// never re-downloaded on a re-run. Otherwise it runs fetch, serializes and
// writes the result atomically, and records the object done with its content
// signature. A terminal fetch error (a 404) records an absence observation,
// which settles to permanently absent only once a later run observes it again
// (see [manifest.Ledger.RecordAbsentObservation]); any other error, a 410 among
// them, records the object as errored so a re-run retries it. Only a
// cancellation of ctx propagates; every other outcome is recorded and returns
// nil so one bad object never aborts the run.
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
// not be buffered. Like [Env.Object] it skips a settled object and
// records the outcome; error handling matches [Env.Object].
//
// Unlike the buffered fetches, whose HTTP exchanges ride the API client's
// internal retry layer, a streamed log is read in chunks the go-tfe reader
// requests on the raw HTTP client, so a single network blip mid-stream fails
// the whole stream with no retry beneath this call. A fetch or mid-stream
// error that classifies transient is therefore retried here, re-running fetch
// after a doubling backoff (see [WithBlobRetry]), before the failure is
// recorded. The write is atomic, so a restarted stream never commits a
// partial file.
//
// An empty stream carries nothing to archive. Some endpoints answer an absent
// artifact with 204 No Content (a stack step that only planned, for one), which
// the client hands back as an empty reader with no error; writing it would leave
// a zero-byte file recorded done that a re-run never retries. Such a result is
// recorded as not applicable, a settled gap, and no file is written, matching
// [Env.Bytes].
func (e *Env) Blob(ctx context.Context, relPath string, fetch func(context.Context) (io.Reader, error)) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	var cause error

	for attempt := 0; ; attempt++ {
		settled, err := e.streamBlob(ctx, relPath, fetch)
		if settled {
			return err
		}

		cause = err

		// A cancellation classifies transient, but sleeping on a dead context
		// only delays the wind-down; propagate it before considering a retry.
		canceled := e.canceled(ctx, relPath)
		if canceled != nil {
			return canceled
		}

		if attempt >= e.blobRetries || !tfeclient.IsTransient(cause) {
			break
		}

		err = sleep(ctx, retryDelay(e.blobRetryDelay, attempt))
		if err != nil {
			return fmt.Errorf("archive %q: %w", relPath, err)
		}

		e.ledger.AddRetry()
	}

	return e.fail(ctx, relPath, cause)
}

// streamBlob runs one fetch-and-stream attempt for [Env.Blob]. Settled is true
// when the attempt reached a final outcome — recorded done, a recorded
// not-applicable gap, a recorded local write failure, or a cancellation
// propagating as err — and false when the fetch or a mid-stream read failed,
// returning that cause for the caller to classify and perhaps retry.
func (e *Env) streamBlob(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) (io.Reader, error),
) (bool, error) {
	r, err := fetch(ctx)
	if err != nil {
		return false, err
	}

	// A nil reader with no error carries nothing to archive: some readers answer
	// an absent artifact with a nil body (a 304 Not Modified on a conditional
	// fetch, a workspace with no readme). Treat it like an empty stream, recording
	// a settled gap and writing nothing, rather than dereferencing nil in the Peek
	// below.
	if r == nil {
		e.ledger.RecordNotApplicable(relPath)

		return true, nil
	}

	// Some clients hand back an [io.ReadCloser] over a live response body (stack
	// state descriptions, step artifacts); close it once streamed — and before a
	// retry opens a fresh one — so the connection and file descriptor are not
	// leaked.
	if rc, ok := r.(io.Closer); ok {
		defer rc.Close() //nolint:errcheck // Best-effort close of an already-consumed body.
	}

	// Peek one byte to spot an empty payload without buffering the blob. Only a
	// clean EOF settles it as not applicable; any other read error flows through
	// WriteReader below and is recorded like a mid-stream transfer failure.
	buffered := bufio.NewReader(r)

	_, peekErr := buffered.Peek(1)
	if errors.Is(peekErr, io.EOF) {
		e.ledger.RecordNotApplicable(relPath)

		return true, nil
	}

	// Distinguish a mid-stream read failure from a local write failure: the
	// recorder remembers a non-EOF read error so it routes through the fetch
	// classification (a stalled network read records errored+transient) rather
	// than being mislabeled a non-transient write failure.
	rec := &recordingReader{r: buffered}

	res, err := e.store.WriteReader(relPath, rec)
	if err != nil {
		if rec.err != nil {
			return false, rec.err
		}

		return true, e.failWrite(ctx, relPath, err)
	}

	e.recordDone(relPath, res)

	return true, nil
}

// maxBlobRetryDelay caps the doubling retry backoff so a generously configured
// retry count cannot stretch one object's in-run wait into minutes.
const maxBlobRetryDelay = 30 * time.Second

// retryDelay returns base doubled attempt times, capped at maxBlobRetryDelay.
// The cap binds from the first retry, not just once doubling overshoots it. A
// non-positive base stays non-positive (min leaves it unchanged), so a
// test-configured zero delay retries immediately.
func retryDelay(base time.Duration, attempt int) time.Duration {
	d := min(base, maxBlobRetryDelay)
	for range attempt {
		d *= 2
		if d >= maxBlobRetryDelay {
			return maxBlobRetryDelay
		}
	}

	return d
}

// sleep waits d or until ctx is done, whichever comes first, returning the
// context's error when it ended the wait. A non-positive d only checks the
// context.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err() //nolint:wrapcheck // The caller adds the object context.
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // The caller adds the object context.
	case <-timer.C:
		return nil
	}
}

// recordingReader remembers the first non-EOF error its reads return, so a
// streaming write that fails can tell a source read error apart from a store
// write error.
type recordingReader struct {
	r   io.Reader
	err error
}

// Read delegates and records the first non-EOF error.
func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}

	return n, err //nolint:wrapcheck // A transparent reader wrapper.
}

// Bytes archives a single immutable blob already held in memory at relPath.
//
// It suits raw artifacts the client buffers whole (configuration tarballs,
// policy source) that need no serialization. Like [Env.Object] it skips a
// settled object and records the outcome; error handling matches [Env.Object].
//
// An empty payload carries nothing to archive. Some endpoints answer an absent
// artifact with 204 No Content instead of a 404 (the structured plan JSON of a
// run that produced none, for one), which the client hands back as empty bytes
// with no error; writing it would leave a zero-byte, unparseable file. Such a
// result is recorded as not applicable, a settled gap, and no file is written.
func (e *Env) Bytes(ctx context.Context, relPath string, fetch func(context.Context) ([]byte, error)) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	data, err := fetch(ctx)
	if err != nil {
		return e.fail(ctx, relPath, err)
	}

	if len(data) == 0 {
		e.ledger.RecordNotApplicable(relPath)

		return nil
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
// terminal error records an absence observation, which settles to permanent
// (sticky) absence only once a later run observes it again, so an
// eventual-consistency 404 on a just-listed object is never converted into a
// permanent gap from one response; an access denial records a forbidden object
// (retryable, so a later run under a broader token captures it); anything else
// records an errored object, transient when the client classifies it so, so a
// re-run retries it and never mistakes a rate-limit blip for a gone object.
func (e *Env) fail(ctx context.Context, relPath string, cause error) error {
	canceled := e.canceled(ctx, relPath)
	if canceled != nil {
		return canceled
	}

	if tfeclient.IsTerminal(cause) {
		e.ledger.RecordAbsentObservation(relPath, cause)

		return nil
	}

	if tfeclient.IsForbidden(cause) {
		e.ledger.RecordForbidden(relPath, cause)

		return nil
	}

	e.ledger.RecordErrored(relPath, cause, tfeclient.IsTransient(cause))

	return nil
}

// failWrite records a local write failure, distinct from a fetch error: it is
// never a permanent absence, so it records an errored (non-transient) object a
// re-run retries. A cancellation of the passed context still propagates.
func (e *Env) failWrite(ctx context.Context, relPath string, cause error) error {
	canceled := e.canceled(ctx, relPath)
	if canceled != nil {
		return canceled
	}

	e.ledger.RecordErrored(relPath, cause, false)

	return nil
}

// canceled reports a cancellation of ctx as an archive error, or nil when ctx
// is still live. Both [Env.fail] and [Env.failWrite] short-circuit on it so a
// wind-down propagates rather than being recorded as an object's outcome.
func (e *Env) canceled(ctx context.Context, relPath string) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("archive %q: %w", relPath, ctxErr)
	}

	return nil
}
