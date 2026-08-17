package collect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/serialize"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
	"go.jacobcolvin.com/hcp_archiver/pkg/tfeclient"
)

// archiveConfig carries the per-object settings of one archive call.
type archiveConfig struct {
	updatedAt time.Time
}

// ArchiveOption configures a single archive of one object, accepted by
// [Env.Object], [Env.Mutable], [Env.Blob], and [Env.Bytes].
//
// The available options are:
//   - [WithUpdatedAt]
type ArchiveOption func(*archiveConfig)

// WithUpdatedAt records the server-side updated-at the listing reported for
// the object, carried onto its ledger entry when the archive records done
// (see [manifest.Entry.UpdatedAt]). It returns an [ArchiveOption].
func WithUpdatedAt(t time.Time) ArchiveOption {
	return func(cfg *archiveConfig) {
		cfg.updatedAt = t
	}
}

// resolveArchiveConfig folds opts into one [archiveConfig].
func resolveArchiveConfig(opts []ArchiveOption) archiveConfig {
	var cfg archiveConfig

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// retention says what a JSON archive write does with the content it replaces.
type retention int

const (
	// Keep only the current content: the file is the whole record.
	// [Env.Object] uses it, so even a retry that rewrites an object (an
	// errored entry re-fetched with different bytes) leaves no superseded
	// copy behind.
	retainCurrent retention = iota
	// Supersede replaced content into the object's history sidecar and
	// record disappearances there as tombstones. [Env.Mutable] uses it, so
	// a re-run never loses a version a prior run archived.
	retainHistory
)

// Object archives a single immutable object at relPath.
//
// It consults the ledger first: a settled object (done, skipped,
// not-applicable, or absent) returns without fetching, so an immutable
// artifact is never re-downloaded on a re-run. Otherwise it runs fetch,
// serializes and writes the result atomically, and records the object done
// with its content signature. A terminal fetch error (a 404) is confirmed by
// one in-run re-probe (see [WithAbsentConfirm]) and then records the object
// absent, a settled state re-probed only under retry-absent; any other error,
// a 410 among them, records the object as errored so a re-run retries it.
// Only a cancellation of ctx propagates; every other outcome is recorded and
// returns nil so one bad object never aborts the run.
func (e *Env) Object(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) (any, error),
	opts ...ArchiveOption,
) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	return e.archiveJSON(ctx, relPath, fetch, retainCurrent, resolveArchiveConfig(opts))
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
//
// A refresh never loses a version: when the payload changed, the outgoing
// content is appended to an append-only history sidecar beside the object
// (variables.json gains variables.history.ndjson) before the new bytes rename
// into place, so every superseded version stays recoverable in order. An
// object that disappears upstream (a confirmed 404 after a good archive)
// keeps its last-known file and records a tombstone in the sidecar, once per
// disappearance; one that comes back appends its returning content over the
// tombstone so the timeline stays ordered. An object that never changes never
// grows a sidecar.
//
// A re-read whose payload is byte-identical to what the ledger records done is
// not re-materialized when the loose file is absent: that is the fingerprint
// of an object sealed elsewhere (a terminal run.json coalesced into a
// roll-up), and rewriting it would resurrect the loose copy and duplicate the
// roll-up line on the next seal. The ledger, not file existence, is the
// record, consistent with [Env.Object], so a hand-deleted, unchanged mutable
// file also stays deleted. A changed payload always writes.
func (e *Env) Mutable(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) (any, error),
	opts ...ArchiveOption,
) error {
	return e.archiveJSON(ctx, relPath, fetch, retainHistory, resolveArchiveConfig(opts))
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
// recorded. That covers the mid-stream failures a long transfer actually meets
// (a connection reset, a body truncated short of its declared length, a stall
// past the idle bound), all of which [tfeclient.Classify] reports transient.
// The write is atomic, so a restarted stream never commits a partial file.
//
// An empty stream carries nothing to archive. Some endpoints answer an absent
// artifact with 204 No Content (a stack step that only planned, for one), which
// the client hands back as an empty reader with no error; writing it would leave
// a zero-byte file recorded done that a re-run never retries. Such a result is
// recorded as not applicable, a settled gap, and no file is written, matching
// [Env.Bytes].
func (e *Env) Blob(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) (io.Reader, error),
	opts ...ArchiveOption,
) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	cfg := resolveArchiveConfig(opts)
	confirmed := func(ctx context.Context) (io.Reader, error) {
		return fetchConfirmed(ctx, e, fetch)
	}

	var cause error

	for attempt := 0; ; attempt++ {
		settled, err := e.streamBlob(ctx, relPath, confirmed, cfg)
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
// when the attempt reached a final outcome (recorded done, a recorded
// not-applicable gap, a recorded local write failure, or a cancellation
// propagating as err), and false when the fetch or a mid-stream read failed,
// returning that cause for the caller to classify and perhaps retry.
func (e *Env) streamBlob(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) (io.Reader, error),
	cfg archiveConfig,
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
	// state descriptions, step artifacts); close it once streamed, and before a
	// retry opens a fresh one, so the connection and file descriptor are not
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

	e.recordDone(ctx, relPath, res, cfg)

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
func (e *Env) Bytes(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) ([]byte, error),
	opts ...ArchiveOption,
) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	data, err := fetchConfirmed(ctx, e, fetch)
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

	e.recordDone(ctx, relPath, res, resolveArchiveConfig(opts))

	return nil
}

// fetchConfirmed runs fetch, and when its error classifies terminal (a 404)
// re-probes once after a short delay (see [WithAbsentConfirm]) before
// believing it.
//
// Absence settles sticky, so a single 404 is never trusted: a read moments
// after the object was listed can answer 404 out of eventual consistency and
// succeed seconds later. The confirming probe turns that blip back onto the
// success path, while a genuinely gone object costs one extra request, once,
// before it settles absent. A cancellation ending the wait returns the
// original cause; the caller's cancellation check then propagates the
// wind-down instead of recording an outcome, leaving the object for the next
// run.
func fetchConfirmed[T any](ctx context.Context, e *Env, fetch func(context.Context) (T, error)) (T, error) {
	v, err := fetch(ctx)
	if err == nil || !tfeclient.IsTerminal(err) {
		return v, err
	}

	werr := sleep(ctx, e.absentConfirmDelay)
	if werr != nil {
		return v, err
	}

	e.ledger.AddRetry()

	return fetch(ctx)
}

// Confirmed runs fetch with the terminal-confirmation semantics of the archive
// primitives: an error that classifies terminal (a 404) is re-probed once after
// the absent-confirm delay (see [WithAbsentConfirm]) before it is believed. It
// suits a shared read performed outside the primitives, one whose value splits
// into several derived files, so the cause later recorded through
// [Env.RecordFailure] has already survived the in-run re-probe that guards a
// settled absence.
//
// It is a package-level function rather than an [*Env] method because methods
// cannot be generic.
func Confirmed[T any](ctx context.Context, e *Env, fetch func(context.Context) (T, error)) (T, error) {
	return fetchConfirmed(ctx, e, fetch)
}

// archiveJSON runs fetch, serializes and writes the value, and records the
// object, the shared body of [Env.Object] and [Env.Mutable]; keep decides
// whether replaced content supersedes into the object's history sidecar.
func (e *Env) archiveJSON(
	ctx context.Context,
	relPath string,
	fetch func(context.Context) (any, error),
	keep retention,
	cfg archiveConfig,
) error {
	v, err := fetchConfirmed(ctx, e, fetch)
	if err != nil {
		// A history-kept object that answered a confirmed 404 records its
		// disappearance before e.fail overwrites the entry's FetchedAt (see
		// [manifest.Ledger.RecordAbsent]); a cancellation records nothing, so
		// it buries nothing.
		if keep == retainHistory && ctx.Err() == nil && tfeclient.IsTerminal(err) {
			e.buryHistory(ctx, relPath)
		}

		return e.fail(ctx, relPath, err)
	}

	data, err := serialize.Marshal(v)
	if err != nil {
		return e.failWrite(ctx, relPath, fmt.Errorf("marshal %q: %w", relPath, err))
	}

	if e.sealedElsewhere(relPath, data) {
		return nil
	}

	res, err := e.store.WriteJSONBytes(relPath, data, e.historyOpts(relPath, keep)...)

	switch {
	case errors.Is(err, store.ErrHistoryNotClosed):
		// The bytes landed and only the sidecar record closing a trailing
		// tombstone did not, so the object is done: recording it errored
		// would leave the ledger describing content the archive no longer
		// holds, and would skip the eager mirror upload for bytes that are
		// already on disk. The next commit re-attempts the close.
		e.logger.LogAttrs(ctx, slog.LevelWarn, "history_restore_error",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)

	case err != nil:
		return e.failWrite(ctx, relPath, err)
	}

	e.recordDone(ctx, relPath, res, cfg)

	return nil
}

// historyOpts builds the write options carrying history retention for one
// JSON commit: nil for retainCurrent, and otherwise [store.WithHistory] over
// the entry's pre-write FetchedAt (still the prior run's fetch time, because
// the write happens before recordDone refreshes it) and the ledger's clock.
func (e *Env) historyOpts(relPath string, keep retention) []store.WriteOption {
	if keep != retainHistory {
		return nil
	}

	return []store.WriteOption{store.WithHistory(e.entryFetchedAt(relPath), e.ledger.Now())}
}

// entryFetchedAt returns the fetch time the ledger currently records for
// relPath, or the zero time when no entry exists. Read before an outcome is
// recorded, it is the prior run's fetch time: the stamp superseded and buried
// content carries.
func (e *Env) entryFetchedAt(relPath string) time.Time {
	entry, ok := e.ledger.Entry(relPath)
	if !ok {
		return time.Time{}
	}

	return entry.FetchedAt
}

// buryHistory records an observed disappearance in the object's history
// sidecar (see [store.Store.BuryHistory]). It must run before the failure is
// recorded, while the entry's FetchedAt still holds the last good fetch time;
// on the second and later 404 runs that field holds the prior RecordAbsent
// time instead, which is harmless because the bury no-ops once the sidecar's
// newest record is a tombstone. An append that does not land only warns: the
// disk is unchanged, so the next run's bury re-fires.
func (e *Env) buryHistory(ctx context.Context, relPath string) {
	_, err := e.store.BuryHistory(relPath, e.entryFetchedAt(relPath), e.ledger.Now())
	if err != nil {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "history_bury_error",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)
	}
}

// sealedElsewhere reports whether the freshly marshaled payload at relPath is
// already recorded done with exactly this content while the loose file is
// absent: the fingerprint of an object whose bytes were sealed into another
// form (a terminal run.json coalesced into a roll-up, verified before its
// loose source was removed). Writing it again would resurrect the loose file
// and make the next seal append a duplicate roll-up line, so [Env.archiveJSON]
// skips the write and leaves the entry untouched: re-recording it would only
// churn Attempts and dirty the shard for a no-op.
//
// The comparison requires a recorded hash and never falls back to size alone
// ([manifest.Signature.Equal] would), because a size-only match could bless a
// different payload. A stat error on the loose file fails open (the object is
// treated as present and the write proceeds), so only a clean "absent" report
// gates the skip.
//
// The skip fires only when the fresh payload is byte-identical to the
// recorded signature: nothing was superseded, so it returns before the write
// path's history retention and appends nothing to a sidecar.
func (e *Env) sealedElsewhere(relPath string, data []byte) bool {
	entry, ok := e.ledger.Entry(relPath)
	if !ok || entry.Status != manifest.StatusDone || entry.Signature == nil || entry.Signature.Hash == "" {
		return false
	}

	sig := manifest.SignatureOf(data)
	if entry.Signature.Hash != sig.Hash || entry.Signature.Size != sig.Size {
		return false
	}

	present, err := e.store.Exists(relPath)
	if err != nil {
		return false
	}

	return !present
}

// recordDone records a successful write and counts its bytes when the commit
// actually changed the on-disk content, then mirrors an org-scope change to
// the remote store as written (see [Env.eagerSync]). The config's server-side
// updated-at, when set, stamps the ledger entry.
func (e *Env) recordDone(ctx context.Context, relPath string, res store.WriteResult, cfg archiveConfig) {
	e.ledger.RecordDone(relPath, manifest.Signature{
		Hash: res.SHA256,
		Size: res.Size,
	}, manifest.WithUpdatedAt(cfg.updatedAt))

	if res.Changed {
		e.ledger.AddBytes(res.Size)
	}

	e.eagerSync(ctx, relPath, res)
}

// RecordFailure records cause as the outcome of the object at relPath without
// running a fetch, for a shared read performed outside the primitives whose
// value splits into several derived files. It self-gates like [Env.Object], so
// a settled path is left untouched (never regressed done->absent or
// done->errored), and classifies cause the same way: a terminal cause records
// the object absent, settled and sticky, so the caller must have run the
// read through [Confirmed] and let it re-probe the 404 in-run first; a denial
// records the object forbidden; anything else records it errored so a re-run
// retries. Only a cancellation of ctx propagates. No history tombstone is
// recorded here: the callers feed immutable derived files, which keep only
// their current content (see [Env.Mutable] for the history semantics).
func (e *Env) RecordFailure(ctx context.Context, relPath string, cause error) error {
	if !e.ledger.ShouldFetch(relPath) {
		return nil
	}

	return e.fail(ctx, relPath, cause)
}

// fail maps a fetch error onto a ledger status, the one place the client's
// transient-versus-terminal classification is turned into a recorded outcome.
//
// A cancellation of the passed context propagates so the run can wind down; a
// terminal error records the object absent, settled and sticky, trusting that
// the fetch already re-probed once in-run (see fetchConfirmed) so an
// eventual-consistency 404 on a just-listed object is not converted into a
// gap from one response; an access denial records a forbidden object
// (retryable, so a later run under a broader token captures it); anything else
// records an errored object, transient when the client classifies it so, so a
// re-run retries it and never mistakes a rate-limit blip for a gone object.
func (e *Env) fail(ctx context.Context, relPath string, cause error) error {
	canceled := e.canceled(ctx, relPath)
	if canceled != nil {
		return canceled
	}

	if tfeclient.IsTerminal(cause) {
		e.ledger.RecordAbsent(relPath, cause)

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
