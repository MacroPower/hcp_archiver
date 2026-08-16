package collect

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/remote"
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

// DefaultStreamThreshold is the size at which the sync sweep stops riding a
// file whole in memory and streams it from disk instead. Roll-ups grow with
// run history and can reach gigabytes; buffering one whole would put the
// sweep's memory at the mercy of archive size.
const DefaultStreamThreshold int64 = 32 << 20

// DefaultConcurrency is the fixed bound on concurrent API work: the archiver
// sizes its general request gate from it, and each collection caps its
// fan-out at it ([Env.Concurrency]). One constant serves both so the fan-out
// can never outrun that gate.
const DefaultConcurrency = 16

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
// each concurrency-safe, and the state an Env keeps itself is guarded, so the
// orchestrator can share one across the workspace workers it runs in parallel.
// Chief among that state are the run's directory identities ([Env.ClaimDir])
// and its claim over the objects several collections address at once
// ([Env.ArchiveShared]). Create instances with [NewEnv].
type Env struct {
	client             *tfeclient.Client
	store              *store.Store
	ledger             *manifest.Ledger
	remote             *remote.Client
	logger             *slog.Logger
	idOwners           map[string]map[string]string
	shared             map[string]*sharedOnce
	eagerSem           *semaphore.Weighted
	remoteOrg          string
	remoteCfg          remote.Config
	idMu               sync.Mutex
	sharedMu           sync.Mutex
	eagerFailed        atomic.Int64
	remoteTally        remoteTally
	blobRetries        int
	blobRetryDelay     time.Duration
	absentConfirmDelay time.Duration
	streamThreshold    int64
}

// Option configures an [Env] passed to [NewEnv].
//
// Options of this type:
//   - [WithAbsentConfirm]
//   - [WithBlobRetry]
//   - [WithLogger]
//   - [WithRemote]
//   - [WithStreamThreshold]
//   - [WithTarget]
type Option func(*Env)

// WithStreamThreshold sets the size at which the sync sweep streams a file
// from disk instead of riding it whole in memory, overriding
// [DefaultStreamThreshold]. A non-positive threshold streams everything.
// It returns an [Option].
func WithStreamThreshold(n int64) Option {
	return func(e *Env) {
		e.streamThreshold = n
	}
}

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

// WithRemote enables mirroring to a remote object store: cold surfaces evict
// through [Env.OffloadFile] and everything else syncs through
// [Env.SyncArchive]. The client reaches the store, and cfg with orgName
// compose the object keys through [Env.RemoteKey]. A nil client keeps the
// archive local-only. It returns an [Option].
func WithRemote(client *remote.Client, cfg remote.Config, orgName string) Option {
	return func(e *Env) {
		e.remote = client
		e.remoteCfg = cfg
		e.remoteOrg = orgName
	}
}

// WithLogger sets the structured logger collectors report non-fatal,
// per-object conditions through (a bundle upload, an eviction warning),
// overriding [slog.Default]. A nil logger keeps the default. It returns an
// [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(e *Env) {
		if logger != nil {
			e.logger = logger
		}
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
		shared:             make(map[string]*sharedOnce),
		blobRetries:        DefaultBlobRetries,
		blobRetryDelay:     DefaultBlobRetryDelay,
		absentConfirmDelay: DefaultAbsentConfirmDelay,
		streamThreshold:    DefaultStreamThreshold,
	}

	for _, opt := range opts {
		opt(e)
	}

	if e.logger == nil {
		e.logger = slog.Default()
	}

	// The eager as-written uploads ride the archive fan-out, whose per-page
	// spread is unbounded; this semaphore is what caps their burst at the
	// same ceiling every other fan-out respects.
	if e.remote != nil {
		e.eagerSem = semaphore.NewWeighted(DefaultConcurrency)
	}

	return e
}

// Client returns the shared API client so a collector can build fetch closures
// over its typed services. Route requests through the client so they share the
// run's single rate governor.
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

// Remote returns the client for the object store the archive is mirrored
// to, or nil when no remote is configured; the nil return is the gate every
// remote code path checks first.
func (e *Env) Remote() *remote.Client {
	return e.remote
}

// EagerFailures returns how many eager remote uploads (as-written syncs and
// seal-boundary subtree files) failed so far. Eager failures warn and defer
// to the close sweep, which retries them; they never mark the run incomplete,
// so this count is visibility, not a gate.
func (e *Env) EagerFailures() int {
	return int(e.eagerFailed.Load())
}

// remoteTally is the run-wide accumulator behind [RemoteStats]: unlike the
// per-pass [SyncStats], it spans every remote motion of the organization's
// run: the eager as-written uploads, the seal-boundary subtree syncs, the
// eviction transfers, and the close sweep. The progress reporter can therefore
// snapshot one live figure for the whole run.
// The motions run concurrently, so every field is atomic and the zero value
// is ready to use.
type remoteTally struct {
	uploaded      atomic.Int64
	uploadedBytes atomic.Int64
	evicted       atomic.Int64
	failed        atomic.Int64
}

// upload counts one uploaded file and its size.
func (t *remoteTally) upload(bytes int64) {
	t.uploaded.Add(1)
	t.uploadedBytes.Add(bytes)
}

// evict counts one evicted surface and the bytes its transfer moved (zero
// when the probe found a finished prior upload).
func (t *remoteTally) evict(transferred int64) {
	t.evicted.Add(1)
	t.uploadedBytes.Add(transferred)
}

// fail counts one failed motion.
func (t *remoteTally) fail() {
	t.failed.Add(1)
}

// stats converts the accumulator into a [RemoteStats] snapshot.
func (t *remoteTally) stats() RemoteStats {
	return RemoteStats{
		UploadedBytes: t.uploadedBytes.Load(),
		Uploaded:      int(t.uploaded.Load()),
		Evicted:       int(t.evicted.Load()),
		Failed:        int(t.failed.Load()),
	}
}

// RemoteStats is a point-in-time snapshot of the run's remote-transfer tally,
// read through [Env.RemoteTally].
//
// Failed counts motions, not distinct files: a file that fails eagerly, again
// at its seal boundary, and again in the close sweep counts three times, and
// an [ErrRemoteCopyMismatch] re-reports every run by design, so the count can
// hold above zero steadily. Per-file and per-eviction outcomes are covered;
// pass-level failures (an inventory listing, a tree walk, a prune batch) stay
// out and surface through each pass's [SyncStats] instead.
type RemoteStats struct {
	// UploadedBytes is the total size of the files transferred: synced
	// uploads plus eviction uploads, unlike the per-pass
	// [SyncStats.UploadedBytes], which excludes eviction bytes.
	UploadedBytes int64
	// Uploaded counts files uploaded because the remote copy was absent or
	// differed.
	Uploaded int
	// Evicted counts cold surfaces moved remote and removed locally.
	Evicted int
	// Failed counts failed remote motions: a file's eager, seal-boundary, or
	// sweep upload, or an eviction refused at any of its gates.
	Failed int
}

// RemoteTally returns a snapshot of the run-wide remote-transfer tally across
// every motion so far; it reads zero without a remote configured.
func (e *Env) RemoteTally() RemoteStats {
	return e.remoteTally.stats()
}

// RemoteKey composes the object key for an archive-relative path, delegating
// to [remote.Config.Key] over the environment's organization so the key
// scheme has exactly one owner.
func (e *Env) RemoteKey(relPath string) string {
	return e.remoteCfg.Key(e.remoteOrg, relPath)
}

// Log returns the structured logger collectors report non-fatal conditions
// through, never nil.
func (e *Env) Log() *slog.Logger {
	return e.logger
}

// SetTarget updates the current progress target (org, project, or workspace) so
// a long walk surfaces where it is working.
func (e *Env) SetTarget(target string) {
	e.ledger.SetTarget(target)
}

// Concurrency returns the ceiling a collector caps one collection's fan-out
// at (its errgroup SetLimit), fixed at [DefaultConcurrency]. Real request
// parallelism is bounded by the client's gate either way; the cap keeps a
// huge collection from parking thousands of goroutines on the gate at once,
// where their synchronized retries would concentrate load on a single
// endpoint.
func (e *Env) Concurrency() int {
	return DefaultConcurrency
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

// Obligation returns the ledger marker at relPath for work whose outcome no
// object entry carries (see [manifest.Obligation]): a child enumeration
// beneath a walk-frozen element, or a nested walk whose settlement the
// enclosing walk's gate cannot otherwise see. The path must lie under the
// archive prefix of every collection that must stay open while the work is
// outstanding; open the marker before the work, so a crash leaves the pending
// record and a run-scoped dropped surface ([Env.MarkSurfaceDropped]) is never
// the only trace.
func (e *Env) Obligation(relPath string) *manifest.Obligation {
	return e.ledger.Obligation(relPath)
}

// FailObligation records ob failed with cause, classifying its transience
// through the shared client so a rate-limit blip retries while the marker
// still holds the enclosing walks open.
func (e *Env) FailObligation(ob *manifest.Obligation, cause error) {
	ob.Fail(cause, tfeclient.IsTransient(cause))
}

// MarkSurfaceDropped records that the enumeration of surface failed this run
// for a non-cancellation reason, marking the run incomplete.
//
// Every collector that tolerates a listing failure (logging it and moving on
// so one unreachable surface does not abort the rest of the organization)
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
// using the same predicate ([Env.ShouldFetch]) the walk uses to decide
// retries. Once every shared write settles, the gate clears, unless one
// settled as a confirmed absence, which marks the gate absent instead: the
// absence lives in a foreign shard the run walk never scans, and the mark is
// the trace a retry-absent run re-opens the walk through. See
// [manifest.Ledger.MirrorReference] and
// [manifest.Ledger.MirrorReferenceAbsent].
func (e *Env) Reference(gateKey string, sharedPaths ...string) {
	if slices.ContainsFunc(sharedPaths, e.ledger.ShouldFetch) {
		e.ledger.MirrorReference(gateKey, false)

		return
	}

	mirrorsAbsence := slices.ContainsFunc(sharedPaths, func(p string) bool {
		entry, ok := e.ledger.Entry(p)

		return ok && entry.Status == manifest.StatusAbsent
	})
	if mirrorsAbsence {
		e.ledger.MirrorReferenceAbsent(gateKey)

		return
	}

	e.ledger.MirrorReference(gateKey, true)
}

// SkipGatedListing reports whether a metered child listing can be skipped:
// its list file is settled, the reference gate over the children it derives
// is not pending, and no recorded absence beneath subtreePrefix awaits a
// retry-absent re-probe (see [manifest.Ledger.HasRetryableAbsentUnder]).
//
// The three clauses travel together as one named predicate because every
// gate that composed them by hand was a place to forget one: a forgotten
// retry-absent clause left the flag inert for exactly the absences only the
// listing could reach, while the walk above kept re-paging on their account.
func (e *Env) SkipGatedListing(listPath, gateKey, subtreePrefix string) bool {
	return !e.ledger.ShouldFetch(listPath) &&
		!e.ledger.ReferencePending(gateKey) &&
		!e.ledger.HasRetryableAbsentUnder(subtreePrefix)
}

// Entry returns the ledger entry recorded for relPath and whether one exists.
// The seal phase reads it to confirm an artifact is settled before it bundles the
// loose copy and removes it.
func (e *Env) Entry(relPath string) (manifest.Entry, bool) {
	return e.ledger.Entry(relPath)
}

// FlushLedger persists everything recorded in the ledger so far durably (see
// [manifest.Ledger.Flush]). Records otherwise reach disk only on the
// archiver's periodic flush, so a phase that destroys the loose evidence its
// in-memory records describe (the seal) flushes first: a hard kill then never
// leaves a durable ledger missing the entries and collection flags that
// authorized the destruction.
func (e *Env) FlushLedger() error {
	err := e.ledger.Flush()
	if err != nil {
		return fmt.Errorf("flush ledger: %w", err)
	}

	return nil
}

// Collection returns the ledger handle for the collection whose entries live
// under archivePrefix (see [manifest.Collection]): its completion and settled
// flags, high-water mark, and unsettled-child gate, all keyed on the prefix so
// they share the shard that owns the entries. [Walk] drives one; the seal
// phase reads completion through one before bundling a collection's cold
// artifacts; the audit collector keeps its Since cursor on one.
func (e *Env) Collection(archivePrefix string) *manifest.Collection {
	return e.ledger.Collection(archivePrefix)
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
