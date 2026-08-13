package archiver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/progress"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// defaultFlushInterval is the cadence at which each organization's ledger is
// flushed durably to disk while its collectors run.
const defaultFlushInterval = 10 * time.Second

// defaultConcurrency is the fixed bound on outstanding API requests to the
// general rate bucket, aliasing [collect.DefaultConcurrency] so the gate and
// every fan-out cap share the collectors' number. Concurrency is a static
// resource bound here, not a tuning knob: request pacing is decided entirely by
// the client's adaptive rate governors, which react to the server's rate-limit
// feedback, so there is nothing for a concurrency setting to adapt to.
const defaultConcurrency = collect.DefaultConcurrency

var (
	// ErrRunIncomplete reports that the run finished without archiving every
	// organization or without mirroring every file: at least one organization
	// returned a non-cancellation error, recorded only failures with nothing
	// captured, dropped an enumeration surface, or closed with remote sync
	// failures leaving the mirror behind the local archive. The
	// per-organization and per-object details are logged as the run proceeds;
	// this sentinel lets the command exit non-zero so a scheduled run does not
	// report success over an empty, broken, or under-mirrored archive.
	ErrRunIncomplete = errors.New("archive run incomplete")

	// ErrNoOrganizations reports that auto-discovery resolved no organizations to
	// archive: the token sees none, whether the account is genuinely empty or the
	// token has been scoped down. Reporting success over a wholly empty archive
	// would defeat the same guarantee as [ErrRunIncomplete], so the run surfaces
	// this instead.
	ErrNoOrganizations = errors.New("no organizations to archive")
)

// Archiver drives a single archive run across one or more organizations.
//
// It is the composition root: from a validated [config.Config] it builds the
// shared client and, per organization, a fresh store, ledger, collection
// environment, and progress reporter, then schedules the domain collectors and
// owns the cross-cutting runtime (the request gate, the flush and progress
// tickers, graceful shutdown, and the closing run record). It holds no
// per-object API knowledge; that lives in the collectors it runs.
//
// The gate is the run's parallelism bound for generally-metered endpoints:
// every such API request takes a slot through the client's gate, so slots flow
// to whatever work is ready (several small workspaces, or many pieces of one
// large workspace) rather than being pinned one-per-workspace. The bound is
// fixed at [defaultConcurrency]; how fast those slots launch requests is decided
// by the client's adaptive rate governors, which are where the server's
// rate-limit feedback lands. The two runs list endpoints draw on a separate gate
// the client owns, because a slot spans the wait for its bucket's rate token and
// that bucket is paced sixty times slower.
//
// Create instances with [New].
type Archiver struct {
	cfg             *config.Config
	client          *tfeclient.Client
	remote          *remote.Client
	logger          *slog.Logger
	w               io.Writer
	logSink         progress.LogSink
	cancelRun       context.CancelFunc
	wireBytes       *atomic.Int64
	uploadWireBytes *atomic.Int64
	rateLimited     *atomic.Int64
	flushInterval   time.Duration
}

// Option configures an [Archiver] passed to [New].
//
// The available options are:
//   - [WithWriter]
//   - [WithLogger]
//   - [WithLogSink]
//   - [WithFlushInterval]
type Option func(*Archiver)

// WithWriter sets the writer progress and the default logger are rendered to,
// defaulting to [os.Stderr]. Tests capture output by passing a buffer. A nil
// writer keeps the default. It returns an [Option].
func WithWriter(w io.Writer) Option {
	return func(a *Archiver) {
		if w != nil {
			a.w = w
		}
	}
}

// WithLogger sets the structured logger used for run lifecycle and non-fatal
// per-organization errors, overriding the default built over the writer. A nil
// logger keeps the default. It returns an [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(a *Archiver) {
		if logger != nil {
			a.logger = logger
		}
	}
}

// WithLogSink sets the [progress.LogSink] the per-organization reporter routes
// its terminal UI's log output through, so log lines and the live panel share
// one renderer. A nil sink leaves the UI without a sink. It returns an
// [Option].
func WithLogSink(sink progress.LogSink) Option {
	return func(a *Archiver) {
		a.logSink = sink
	}
}

// WithFlushInterval sets the cadence at which the ledger is flushed durably
// while collectors run. A non-positive value keeps the default. It returns an
// [Option].
func WithFlushInterval(d time.Duration) Option {
	return func(a *Archiver) {
		if d > 0 {
			a.flushInterval = d
		}
	}
}

// New creates a new [Archiver] from cfg.
//
// It applies each [Option] in order and, when none supplied a logger, builds a
// text logger over the resolved writer. It performs no I/O and builds no client
// until [Archiver.Run] is called.
func New(cfg *config.Config, opts ...Option) *Archiver {
	a := &Archiver{
		cfg:             cfg,
		w:               os.Stderr,
		wireBytes:       new(atomic.Int64),
		uploadWireBytes: new(atomic.Int64),
		rateLimited:     new(atomic.Int64),
		flushInterval:   defaultFlushInterval,
	}

	for _, opt := range opts {
		opt(a)
	}

	if a.logger == nil {
		a.logger = slog.New(slog.NewTextHandler(a.w, nil))
	}

	return a
}

// Run archives every resolved organization in turn.
//
// It validates the configuration, builds the one shared client — proving a
// configured remote store manageable with a probe round-trip before any
// archive work — resolves the organizations (the single named one, or every
// visible organization), and archives each sequentially. The orgs run under a
// cancelable child of ctx whose cancel the reporter's terminal UI holds, so an
// in-UI ctrl+c aborts the whole run exactly as an external SIGINT does. A
// non-cancellation failure of one organization is logged and does not abort
// the others, but leaves the run incomplete; a cancellation aborts the run and
// is returned wrapped so the command can map it to a graceful exit. Run
// returns nil on clean completion and [ErrRunIncomplete] when any organization
// returned a non-cancellation error, captured nothing but failures, or closed
// with remote sync failures (the mirror is the long-term record, so a run
// that leaves it knowingly behind must not exit clean; the next run's sweep
// self-heals the gap).
func (a *Archiver) Run(ctx context.Context) error {
	err := a.cfg.Validate()
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	// One gate of request slots serves the whole run's general traffic: every
	// such request takes a slot, so parallelism follows the work rather than a
	// fixed per-workspace assignment. The count is fixed; the client's adaptive
	// rate governor decides how fast the slots launch, and orgs run sequentially
	// over one shared monotonic wire-byte counter (each reporter windows
	// deltas of it) with the rate-limit counter feeding the progress views the
	// same way.
	a.client, err = tfeclient.New(
		tfeclient.WithToken(a.cfg.Token),
		tfeclient.WithAddress(a.cfg.Address),
		tfeclient.WithRateLimit(a.cfg.RateLimit),
		tfeclient.WithWireBytes(a.wireBytes),
		tfeclient.WithRateLimitCounter(a.rateLimited),
		tfeclient.WithGate(tfeclient.NewSemaphore(defaultConcurrency)),
		tfeclient.WithLogger(a.logger),
	)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	// One remote client serves every organization: each organization's
	// mirror lands in the same bucket, keyed under its own subtree. The
	// preflight probe proves the store manageable now, before any archive
	// work, rather than letting a bad bucket or credential set surface as
	// per-object failures deep into the run.
	if a.cfg.Remote != nil {
		a.remote, err = remote.New(ctx, remoteConfig(a.cfg.Remote),
			remote.WithWireBytes(a.uploadWireBytes))
		if err != nil {
			return fmt.Errorf("build remote client: %w", err)
		}

		defer func() {
			closeErr := a.remote.Close()
			if closeErr != nil {
				a.logger.LogAttrs(ctx, slog.LevelWarn, "remote_close_error",
					slog.String("error", closeErr.Error()),
				)
			}
		}()

		report, preflightErr := a.remote.Preflight(ctx)
		if preflightErr != nil {
			return fmt.Errorf("verify remote store: %w", preflightErr)
		}

		// An untrusted backend digest attribute (an SSE-KMS bucket's ETag is
		// hex but no content MD5) downgrades nothing about correctness — the
		// client serves the metadata digests this tool records instead — but
		// size-matched files now pay one Head each, so the run notes it once.
		if report.AttrDigestsUntrusted {
			a.logger.LogAttrs(ctx, slog.LevelWarn, "remote_attr_digests_untrusted",
				slog.String("detail", "the store's digest attribute does not match written bytes"+
					" (SSE-KMS?); digest comparisons will use recorded object metadata only"),
			)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	a.cancelRun = cancel

	orgs, err := a.resolveOrgs(runCtx)
	if err != nil {
		return fmt.Errorf("resolve organizations: %w", err)
	}

	// Auto-discovery found nothing to archive. The loop below would fall straight
	// through and return a clean success over an empty archive, so surface the
	// empty result rather than let a scheduled run report success on it.
	if len(orgs) == 0 {
		return ErrNoOrganizations
	}

	incomplete := false

	for _, orgName := range orgs {
		if runCtx.Err() != nil {
			return fmt.Errorf("archive run: %w", runCtx.Err())
		}

		a.logger.LogAttrs(runCtx, slog.LevelInfo, "org_archive_start",
			slog.String("org", orgName),
		)

		tally, syncFailed, err := a.runOrg(runCtx, orgName)
		if err != nil {
			if runCtx.Err() != nil {
				return fmt.Errorf("archive run: %w", runCtx.Err())
			}

			a.logger.LogAttrs(runCtx, slog.LevelError, "org_archive_error",
				slog.String("org", orgName),
				slog.String("error", err.Error()),
			)

			incomplete = true

			continue
		}

		// A safety valve that fired at load is visible in the close lines
		// either way the run ends: a discard is an honored deletion or
		// evidence of loss, and an operator reads the summary, not the
		// load-time log.
		discarded := slog.Int("records_discarded", tally.RecordsDiscarded)

		// The mirror check sits beside orgIncomplete rather than inside it:
		// the tally describes what the collectors captured, while syncFailed
		// describes whether the close sweep got all of it mirrored — a
		// different failure with the same consequence for a scheduled run.
		if orgIncomplete(tally) || syncFailed > 0 {
			a.logger.LogAttrs(runCtx, slog.LevelError, "org_archive_incomplete",
				slog.String("org", orgName),
				slog.Int("errored", tally.Errored),
				slog.Int("forbidden", tally.Forbidden),
				slog.Int("surfaces_dropped", tally.SurfacesDropped),
				slog.Int("sync_failed", syncFailed),
				discarded,
			)

			incomplete = true

			continue
		}

		finishAttrs := []slog.Attr{slog.String("org", orgName)}
		if tally.RecordsDiscarded > 0 {
			finishAttrs = append(finishAttrs, discarded)
		}

		a.logger.LogAttrs(runCtx, slog.LevelInfo, "org_archive_finish", finishAttrs...)
	}

	// A cancellation during the final organization has no next iteration to
	// reach the top-of-loop guard, so the finished loop is classified once
	// more here.
	return runOutcome(runCtx, incomplete)
}

// runOutcome classifies the finished organization loop. A cancellation wins
// over an incomplete tally: failure counts accumulated before an interrupt
// landed describe an aborted run, not a finished-but-broken one, so the run
// keeps the graceful cancellation exit the command maps to a clean status.
// Otherwise every organization was tried, and a non-cancellation failure of
// any of them must not be reported as a clean run.
func runOutcome(ctx context.Context, incomplete bool) error {
	if ctx.Err() != nil {
		return fmt.Errorf("archive run: %w", ctx.Err())
	}

	if incomplete {
		return ErrRunIncomplete
	}

	return nil
}
