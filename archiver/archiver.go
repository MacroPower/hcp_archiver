package archiver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// defaultFlushInterval is the cadence at which each organization's ledger is
// flushed durably to disk while its collectors run.
const defaultFlushInterval = 10 * time.Second

// Archiver drives a single archive run across one or more organizations.
//
// It is the composition root: from a validated [config.Config] it builds the
// shared client and, per organization, a fresh store, ledger, collection
// environment, and progress reporter, then schedules the domain collectors and
// owns the cross-cutting runtime (the worker pool, the flush and progress
// tickers, graceful shutdown, and the closing run record). It holds no
// per-object API knowledge; that lives in the collectors it runs.
//
// Create instances with [New].
type Archiver struct {
	cfg           *config.Config
	client        *tfeclient.Client
	logger        *slog.Logger
	w             io.Writer
	flushInterval time.Duration
}

// Option configures an [Archiver] passed to [New].
//
// The available options are:
//   - [WithWriter]
//   - [WithLogger]
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
		cfg:           cfg,
		w:             os.Stderr,
		flushInterval: defaultFlushInterval,
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
// It validates the configuration, builds the one shared client, resolves the
// organizations (the single named one, or every visible organization), and
// archives each sequentially. A non-cancellation failure of one organization is
// logged and does not abort the others; a cancellation of ctx aborts the run
// and is returned wrapped so the command can map it to a graceful exit. Run
// returns nil on clean completion.
func (a *Archiver) Run(ctx context.Context) error {
	err := a.cfg.Validate()
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	a.client, err = tfeclient.New(
		tfeclient.WithToken(a.cfg.Token),
		tfeclient.WithAddress(a.cfg.Address),
	)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	orgs, err := a.resolveOrgs(ctx)
	if err != nil {
		return fmt.Errorf("resolve organizations: %w", err)
	}

	for _, orgName := range orgs {
		if ctx.Err() != nil {
			return fmt.Errorf("archive run: %w", ctx.Err())
		}

		a.logger.LogAttrs(ctx, slog.LevelInfo, "org_archive_start",
			slog.String("org", orgName),
		)

		err = a.runOrg(ctx, orgName)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("archive run: %w", ctx.Err())
			}

			a.logger.LogAttrs(ctx, slog.LevelError, "org_archive_error",
				slog.String("org", orgName),
				slog.String("error", err.Error()),
			)

			continue
		}

		a.logger.LogAttrs(ctx, slog.LevelInfo, "org_archive_finish",
			slog.String("org", orgName),
		)
	}

	return nil
}
