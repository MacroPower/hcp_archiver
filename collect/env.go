package collect

import (
	"context"
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
	client         *tfeclient.Client
	store          *store.Store
	ledger         *manifest.Ledger
	blobRetries    int
	blobRetryDelay time.Duration
}

// Option configures an [Env] passed to [NewEnv].
//
// Options of this type:
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
		client:         client,
		store:          st,
		ledger:         ledger,
		blobRetries:    DefaultBlobRetries,
		blobRetryDelay: DefaultBlobRetryDelay,
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
type Collector interface {
	// Collect archives this collector's object family, returning only on a
	// context cancellation; a single missing or failed object is recorded and
	// does not abort the collector.
	Collect(ctx context.Context) error
	// Name identifies the collector for progress and logs.
	Name() string
}
