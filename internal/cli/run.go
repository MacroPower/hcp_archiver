package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	"go.jacobcolvin.com/x/cobras/profile"

	"go.jacobcolvin.com/hcp_archiver/pkg/archiver"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
)

// flagRetryAbsent names the run command's re-probe flag.
const flagRetryAbsent = "retry-absent"

// archiveFlags holds the raw flag values bound onto the run command. Its
// config method loads the YAML configuration and merges these per-run settings
// into a validated [config.Config].
type archiveFlags struct {
	configPath       *string
	progress         string
	progressInterval time.Duration
	retryAbsent      bool
}

// registerArchiveFlags binds the archive flags onto cmd and returns the
// [*archiveFlags] they write into.
func registerArchiveFlags(cmd *cobra.Command) *archiveFlags {
	af := &archiveFlags{configPath: registerConfigFlag(cmd)}
	fs := cmd.Flags()

	registerProgressFlags(fs, &af.progress, &af.progressInterval)
	fs.BoolVar(&af.retryAbsent, flagRetryAbsent, false,
		"re-probe objects previously recorded as absent")

	return af
}

// config loads the YAML configuration and merges the per-run flag values into a
// validated [config.Config]. The token comes from the environment, so a missing
// token surfaces here as [config.ErrMissingToken]; a malformed configuration
// file surfaces as a source-annotated error.
func (af *archiveFlags) config() (*config.Config, error) {
	mode, err := config.ParseProgressMode(af.progress)
	if err != nil {
		return nil, err
	}

	cfg, err := loadCmdConfig(*af.configPath)
	if err != nil {
		return nil, err
	}

	file := cfg.file

	opts := []config.Option{
		config.WithAddress(file.Address),
		config.WithRateLimit(file.RateLimit),
		config.WithOrganizations(file.Organizations),
		config.WithProjects(file.Projects),
		config.WithWorkspaces(file.Workspaces),
		config.WithRunHistoryCount(file.RunHistory.Fetch.Count),
		config.WithRunHistoryAge(time.Duration(file.RunHistory.Fetch.Age)),
		config.WithStacks(file.Include.Stacks),
		config.WithHYOK(file.Include.HYOK),
		config.WithRegistryDetail(file.Include.RegistryDetail),
		config.WithAuditTrail(file.Include.AuditTrail),
		config.WithArchiveDir(cfg.archiveDir),
		config.WithProgressMode(mode),
		config.WithProgressInterval(af.progressInterval),
		config.WithRetryAbsent(af.retryAbsent),
	}

	// An untouched remote section leaves offloading disabled rather than
	// enabling it over an empty bucket URL.
	if !file.Remote.IsZero() {
		opts = append(opts, config.WithRemote(file.Remote.RemoteConfig()))
	}

	return config.New(opts...)
}

// newRunCmd returns the command that archives the configured organizations.
// The sink resolves the shared log writer the root command builds in
// PersistentPreRunE, so log lines scroll above the progress panel while the
// terminal UI runs.
func newRunCmd(profiler *profile.Profiler, sink func() *progress.LogWriter) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Archive the configured organizations to disk",
		Long: `Archive HCP Terraform organizations to plain files on disk: state history,
run history, plan and apply logs, the configuration that produced each run,
and the surrounding org-level metadata. With no organization filters in the
configuration file, every organization the token can see is archived with the
default surfaces.

The archive directory is the unit of resume: re-running against the same
directory skips what is done or permanently gone, retries what errored,
appends new runs and state versions, and refreshes mutable metadata
(retaining every superseded version in a history sidecar) without
re-downloading immutable blobs. An interrupted run (ctrl-c or SIGTERM) exits
cleanly, and the next invocation continues from where it stopped; there is no
separate resume command.

A run ends with a per-status summary, the resume model made visible:

  done       fetched and written.
  absent     gone upstream (a 404, confirmed by an in-run re-probe);
             re-probed only with --retry-absent.
  forbidden  the token may not read it (a 403); retried on the next run,
             so a differently scoped token can still capture it.
  errored    a transient or unclassified failure, retried next run; a
             healthy archive ends with errored=0.
  skipped    intentionally deferred or not applicable to this archive;
  n/a        settled, and never mistaken for a gap.

A non-zero errored count is the one to investigate; the others are recorded
gaps, not failures. Coverage is bounded by the archiving identity, so no
single token necessarily sees everything: point several tokens at the same
archive directory in turn to accumulate the union of what each can read.`,
		Args: cobra.NoArgs,
	}

	af := registerArchiveFlags(cmd)
	registerProgressCompletion(cmd)

	cmd.RunE = func(cc *cobra.Command, _ []string) error {
		// Run the archive inside the profiler so the CPU profile is flushed and
		// the snapshot profiles are written even when the archive returns an
		// error; cobra skips PersistentPostRunE once RunE has failed.
		return profiler.Run(func() error {
			return runArchive(cc, af, sink())
		})
	}

	return cmd
}

// runArchive resolves the flags into a configuration and archives under a
// signal-aware context. A graceful interrupt exits cleanly, and a second forces
// an immediate quit. It passes logWriter as the log sink so, while the terminal
// UI runs, the collectors' log output routes through the one renderer that owns
// the screen.
func runArchive(cmd *cobra.Command, af *archiveFlags, logWriter *progress.LogWriter) error {
	cfg, err := af.config()
	if err != nil {
		return err
	}

	ctx, stop := signalContext(cmd.Context())
	defer stop()

	a := archiver.New(
		cfg,
		archiver.WithWriter(cmd.ErrOrStderr()),
		archiver.WithLogger(slog.Default()),
		archiver.WithLogSink(logWriter),
	)

	err = a.Run(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}

		return err
	}

	return nil
}
