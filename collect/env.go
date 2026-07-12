package collect

import (
	"context"
	"slices"
	"sync"
	"time"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// Default in-run retry settings for transient blob failures.
const (
	// DefaultBlobRetries is the default number of additional attempts
	// [Env.Blob] makes after a fetch or mid-stream failure that classifies
	// transient.
	DefaultBlobRetries = 2

	// DefaultBlobRetryDelay is the default wait before [Env.Blob]'s first
	// retry; each subsequent retry doubles it.
	DefaultBlobRetryDelay = 2 * time.Second

	// DefaultAbsentConfirmDelay is the default wait before the confirming
	// re-probe of a fetch that answered 404, so a brief consistency blip is
	// not settled as an absence from one response.
	DefaultAbsentConfirmDelay = 2 * time.Second
)

// Env is the shared environment every domain collector composes to archive an
// object end to end.
//
// It binds the API client, the on-disk store, and the ledger into one place so
// a collector carries only the knowledge of its own endpoints: it supplies a
// fetch closure and a relative path, and the environment consults the ledger to
// decide whether to fetch, retrieves through the client, serializes and writes
// atomically, and records the resulting status, content signature, and
// high-water mark. Collectors reach the store, serializer, and ledger only
// through this type, which keeps the per-object archive policy in one place.
//
// An Env is safe for concurrent use: the client, store, and ledger it wraps are
// each concurrency-safe, so the orchestrator can share one Env across the
// workspace workers it runs in parallel. Create instances with [NewEnv].
type Env struct {
	client             *tfeclient.Client
	store              *store.Store
	ledger             *manifest.Ledger
	idOwners           map[string]map[string]string
	idMu               sync.Mutex
	blobRetries        int
	blobRetryDelay     time.Duration
	absentConfirmDelay time.Duration
}

// Option configures an [Env] passed to [NewEnv].
//
// Options of this type:
//   - [WithAbsentConfirm]
//   - [WithBlobRetry]
//   - [WithTarget]
type Option func(*Env)

// WithBlobRetry sets how [Env.Blob] retries a transient fetch or mid-stream
// failure within the run: retries is the number of additional attempts after
// the first, and delay is the wait before the first retry, doubling on each
// retry after that. A zero or negative retries disables in-run retrying,
// leaving the failure to the next run; a non-positive delay retries
// immediately. It returns an [Option].
func WithBlobRetry(retries int, delay time.Duration) Option {
	return func(e *Env) {
		e.blobRetries = retries
		e.blobRetryDelay = delay
	}
}

// WithAbsentConfirm sets the wait before the single confirming re-probe of a
// fetch whose error classifies terminal (a 404). A first 404 is never believed
// alone: every archive primitive re-fetches once after delay, and only a
// second 404 records the object absent (see [manifest.Ledger.RecordAbsent]).
// A non-positive delay re-probes immediately. It returns an [Option].
func WithAbsentConfirm(delay time.Duration) Option {
	return func(e *Env) {
		e.absentConfirmDelay = delay
	}
}

// WithTarget sets the initial progress target (the org, project, or workspace
// shown by progress reporting) before any walk updates it. It returns an
// [Option].
func WithTarget(target string) Option {
	return func(e *Env) {
		e.ledger.SetTarget(target)
	}
}

// NewEnv creates a new [Env] binding client, st, and ledger.
//
// The archiver builds one Env per organization from that org's client, store,
// and ledger, then hands it to every collector it runs against the org.
func NewEnv(client *tfeclient.Client, st *store.Store, ledger *manifest.Ledger, opts ...Option) *Env {
	e := &Env{
		client:             client,
		store:              st,
		ledger:             ledger,
		idOwners:           make(map[string]map[string]string),
		blobRetries:        DefaultBlobRetries,
		blobRetryDelay:     DefaultBlobRetryDelay,
		absentConfirmDelay: DefaultAbsentConfirmDelay,
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Client returns the shared API client so a collector can build fetch closures
// over its typed services. Route requests through the client so they share the
// run's single rate limiter.
func (e *Env) Client() *tfeclient.Client {
	return e.client
}

// Store returns the store so a collector can build the relative paths its
// objects live under. Commit bytes through the archive primitives ([Env.Object],
// [Env.Mutable], [Env.Blob], [Env.Bytes]) rather than the store's write methods,
// so every write also records a ledger entry.
func (e *Env) Store() *store.Store {
	return e.store
}

// SetTarget updates the current progress target (org, project, or workspace) so
// a long walk surfaces where it is working.
func (e *Env) SetTarget(target string) {
	e.ledger.SetTarget(target)
}

// Skip records the object at relPath as intentionally deferred, a settled state
// a re-run does not mistake for a gap.
func (e *Env) Skip(relPath string) {
	e.ledger.RecordSkipped(relPath)
}

// NotApplicable records the object at relPath as not applicable to this archive
// (a low-value or deferred surface), a settled state a re-run does not mistake
// for a gap.
func (e *Env) NotApplicable(relPath string) {
	e.ledger.RecordNotApplicable(relPath)
}

// MarkSurfaceDropped records that the enumeration of surface failed this run
// for a non-cancellation reason, marking the run incomplete.
//
// Every collector that tolerates a listing failure — logging it and moving on
// so one unreachable surface does not abort the rest of the organization —
// must record the drop through this at the point it swallows the error. A
// per-object failure is already visible as an errored ledger entry, but a
// listing that never completed records no entries for the objects it never
// named, so this is the only channel that keeps a dropped surface out of a
// clean exit. See [manifest.Ledger.MarkSurfaceDropped].
func (e *Env) MarkSurfaceDropped(surface string, cause error) {
	e.ledger.MarkSurfaceDropped(surface, cause)
}

// Reference maintains a run-scoped reference gate at gateKey that mirrors whether
// the cross-shard writes at sharedPaths have settled, so a write that lands
// outside the referencing run's own subtree (its created-by user, an event
// actor, a config-version tarball) is still retried by the run walk when it
// fails. The gate counts unsettled while any shared path is not yet settled,
// using the same predicate ([Env.ShouldFetch]) the walk uses to decide retries,
// and clears once every shared write is settled. See
// [manifest.Ledger.MirrorReference].
func (e *Env) Reference(gateKey string, sharedPaths ...string) {
	settled := !slices.ContainsFunc(sharedPaths, e.ledger.ShouldFetch)
	e.ledger.MirrorReference(gateKey, settled)
}

// ReferencePending reports whether the reference gate at gateKey exists and is
// still unsettled. A split-read that re-derives a cross-shard write's source
// consults it to force the read while the gate is open (the run-event actors,
// whose hydrated objects live only in the list include).
func (e *Env) ReferencePending(gateKey string) bool {
	return e.ledger.ReferencePending(gateKey)
}

// HighWaterMark returns the recorded watermark for key, or the zero time when
// none is set. The audit collector reads its forward Since cursor through this.
func (e *Env) HighWaterMark(key string) time.Time {
	return e.ledger.HighWaterMark(key)
}

// AdvanceHighWaterMark advances the watermark for key toward t, keeping the
// later of the two. The audit collector advances its Since cursor through this;
// [Walk] advances an append-mostly collection's mark itself.
func (e *Env) AdvanceHighWaterMark(key string, t time.Time) {
	e.ledger.AdvanceHighWaterMark(key, t)
}

// Entry returns the ledger entry recorded for relPath and whether one exists.
// The seal phase reads it to confirm an artifact is settled before it bundles the
// loose copy and removes it.
func (e *Env) Entry(relPath string) (manifest.Entry, bool) {
	return e.ledger.Entry(relPath)
}

// IsCollectionComplete reports whether the append-mostly collection under key was
// walked to its end, so the seal phase bundles a collection's cold artifacts only
// once its tail is fully archived.
func (e *Env) IsCollectionComplete(key string) bool {
	return e.ledger.IsCollectionComplete(key)
}

// ShouldFetch reports whether the current pass still needs to fetch the object
// at relPath: a settled object is skipped, while an absent or errored one is
// fetched. A collector that reads one API object and splits it into several
// derived files consults it to skip that read once every derived file is
// settled, so a re-run refetches only while a gap remains.
func (e *Env) ShouldFetch(relPath string) bool {
	return e.ledger.ShouldFetch(relPath)
}

// Collector archives one domain object family into a shared [Env].
//
// Each domain package implements it over the [Env] it is constructed with; the
// orchestrator runs the collectors and needs only this contract, not their
// per-family method shapes.
//
// See [go.jacobcolvin.com/hcp_archiver/collect/orgscope.Collector] for an
// implementation.
type Collector interface {
	// Collect archives this collector's object family, returning only on a
	// context cancellation; a single missing or failed object is recorded and
	// does not abort the collector.
	Collect(ctx context.Context) error
	// Name identifies the collector for progress and logs.
	Name() string
}
