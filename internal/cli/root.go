package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.jacobcolvin.com/x/cobras/log"
	"go.jacobcolvin.com/x/cobras/profile"
	"go.jacobcolvin.com/x/version"

	"go.jacobcolvin.com/hcp_archiver/pkg/archiver"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

const appName = "hcp_archiver"

// Flag names bound onto the root command. What and how to archive lives in the
// YAML configuration file; only per-run and operational settings are flags.
const (
	flagConfig           = "config"
	flagArchivePath      = "archive-path"
	flagProgress         = "progress"
	flagProgressInterval = "progress-interval"
	flagRetryAbsent      = "retry-absent"
)

// ErrLogHandler indicates an error occurred while creating a log handler.
var ErrLogHandler = errors.New("create log handler")

// archiveFlags holds the raw flag values bound onto the root command. Its
// config method loads the YAML configuration and merges these per-run settings
// into a validated [config.Config].
type archiveFlags struct {
	configPath       string
	archivePath      string
	progress         string
	progressInterval time.Duration
	retryAbsent      bool
}

// registerArchiveFlags binds the archive flags onto cmd and returns the
// [*archiveFlags] they write into.
func registerArchiveFlags(cmd *cobra.Command) *archiveFlags {
	af := &archiveFlags{}
	fs := cmd.Flags()

	fs.StringVarP(&af.configPath, flagConfig, "c", "",
		fmt.Sprintf("path to the YAML configuration file (defaults to $%s)", config.EnvConfigPath))
	fs.StringVarP(&af.archivePath, flagArchivePath, "o", "",
		"archive root directory (defaults to the configuration file's archive.path)")
	fs.StringVar(&af.progress, flagProgress, config.DefaultProgressMode.String(),
		"progress output mode (auto|human|json|quiet)")
	fs.DurationVar(&af.progressInterval, flagProgressInterval, config.DefaultProgressInterval,
		"progress reporting cadence")
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

	file, cfgPath, err := af.loadFile()
	if err != nil {
		return nil, err
	}

	output := af.archivePath
	if output == "" {
		output = configDir(cfgPath, file.Archive.Path)
	}

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
		config.WithArchiveDir(output),
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

// loadFile resolves the configuration file path from the --config flag, then
// the environment, and loads it. When neither names a path the built-in
// defaults are used and no file is read. The returned path is the one the
// file was loaded from (empty when none was), the base a relative path inside
// it resolves against.
func (af *archiveFlags) loadFile() (*config.File, string, error) {
	path := af.configPath
	if path == "" {
		path = os.Getenv(config.EnvConfigPath)
	}

	if path == "" {
		return config.DefaultFile(), "", nil
	}

	file, err := config.LoadFile(path)
	if err != nil {
		return nil, "", err
	}

	return file, path, nil
}

// NewRootCmd builds the root [*cobra.Command] for the hcp_archiver CLI. Logging
// is configured from persistent flags in PersistentPreRunE so every subcommand
// shares it. The root command runs the archive inside the profiler, so its
// profiles are written even when the archive errors; the only subcommand
// reports version information.
func NewRootCmd() *cobra.Command {
	logCfg := log.NewConfig()
	profileCfg := profile.NewConfig()

	cmd := &cobra.Command{
		Use:   appName,
		Short: "Archive an HCP Terraform organization to disk.",
		Long: `Archive HCP Terraform (formerly Terraform Cloud) organizations to plain files
on disk for long-term reference: state history, run history, plan and apply
logs, the configuration that produced each run, and the surrounding org-level
metadata. Nothing is restored back into HCP Terraform.

The API token comes from the environment: HCP_TOKEN, falling back to
TFC_TOKEN and then TFE_TOKEN, first non-empty wins. What and how to archive
lives in the YAML configuration file --config names (or $HCP_ARCHIVER_CONFIG);
with neither, every organization the token can see is archived with the
default surfaces.

The output directory is the unit of resume: re-running against the same
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
output directory in turn to accumulate the union of what each can read.`,
		SilenceUsage: true,
		Version:      version.GetVersion(),
	}

	logCfg.RegisterFlags(cmd.PersistentFlags())
	logCfg.MustRegisterCompletions(cmd)
	profileCfg.RegisterFlags(cmd.PersistentFlags())
	profileCfg.MustRegisterCompletions(cmd)

	profiler := profileCfg.NewProfiler()

	af := registerArchiveFlags(cmd)
	registerProgressCompletion(cmd)

	// One shared writer sits between the log handler and stderr: while the
	// terminal UI runs it hands log lines to the program so they scroll above
	// the pinned panel, otherwise it writes through to stderr. It is built in
	// PersistentPreRunE from the executing command's stderr (the same seam the
	// archiver resolves its writer at), so a redirected stderr routes logs too.
	var logWriter *progress.LogWriter

	cmd.PersistentPreRunE = func(cc *cobra.Command, _ []string) error {
		logWriter = progress.NewLogWriter(cc.ErrOrStderr())

		h, err := logCfg.NewHandler(logWriter)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrLogHandler, err)
		}

		slog.SetDefault(slog.New(h))

		return nil
	}

	cmd.RunE = func(cc *cobra.Command, _ []string) error {
		// Run the archive inside the profiler so the CPU profile is flushed and
		// the snapshot profiles are written even when the archive returns an
		// error; cobra skips PersistentPostRunE once RunE has failed.
		return profiler.Run(func() error {
			return runArchive(cc, af, logWriter)
		})
	}

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newViewCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newShowCmd())
	cmd.AddCommand(newExtractCmd())
	cmd.AddCommand(newExportCmd())

	return cmd
}

// newViewCmd returns a command that browses an existing archive in an
// interactive terminal UI mirroring the HCP interface: organizations open into
// projects, workspaces, runs, and state versions. The directory may be the
// archive root or a single organization's directory; it defaults to the
// configuration file's archive.path or, with none set, the current directory.
func newViewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view [archive-path]",
		Short: "Browse an archive in an interactive terminal UI",
		Long: `Browse an archive in an interactive terminal UI mirroring the HCP interface:
organizations open into projects, workspaces, runs, and state versions. The
directory may be the archive root or a single organization's directory; it
defaults to the configuration file's archive.path or, with none set, the
current directory.

` + remoteLong,
		Args: cobra.MaximumNArgs(1),
	}

	rf := registerRemoteFlags(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		file, cfgPath, err := rf.loadFile()
		if err != nil {
			return err
		}

		rcfg, err := rf.remoteFromFile(file)
		if err != nil {
			return err
		}

		dir := defaultArchiveDir(file, cfgPath)
		if len(args) == 1 {
			dir = args[0]
		}

		var opts []view.ArchiveOption

		if rcfg != nil {
			opts = append(opts, view.WithRemote(*rcfg))
		}

		err = view.Browse(ctx, dir, cc.InOrStdin(), cc.OutOrStdout(), opts...)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}

			return err
		}

		return nil
	}

	return cmd
}

// signalContext returns a context canceled on the first SIGINT or SIGTERM and
// arms a force-quit on the second: the first signal begins graceful shutdown,
// and a second, arriving while that shutdown is still underway, terminates the
// process immediately with the conventional 128+signal code (130 for SIGINT,
// 143 for SIGTERM) so a hung shutdown can still be aborted by the operator. The
// returned stop releases the handler and, called on normal completion, disarms
// the force-quit; like any [context.CancelFunc] it is safe to call more than
// once and from multiple goroutines. It replaces a bare [signal.NotifyContext],
// whose second signal is merely buffered and ignored.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	// Buffer two so a rapid double interrupt is not dropped before the first is
	// consumed.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})

	go func() {
		defer signal.Stop(sig)

		select {
		case <-done:
			return
		case <-sig:
			cancel()
		}

		select {
		case <-done:
		case s := <-sig:
			// A second signal and normal completion can become ready together,
			// since stop() closes done as the graceful shutdown returns, and a
			// select picks a ready case at random. Re-check done and prefer the
			// clean exit: force-quit only when shutdown is still underway.
			select {
			case <-done:
				return
			default:
			}

			// 128+signal is the conventional exit code for a signal-terminated
			// process: 130 for SIGINT (ctrl+c), 143 for SIGTERM.
			if sysSig, ok := s.(syscall.Signal); ok {
				//nolint:mnd // 128+signal is the conventional signal-terminated exit code.
				os.Exit(128 + int(sysSig))
			}

			//nolint:mnd // Fall back to the conventional SIGINT code when unknown.
			os.Exit(130)
		}
	}()

	// The close is not idempotent, so guard it with a Once: the returned func is a
	// context.CancelFunc, whose contract makes a second or concurrent call a no-op
	// rather than a "close of closed channel" panic.
	var stopOnce sync.Once

	return ctx, func() {
		cancel()
		stopOnce.Do(func() { close(done) })
	}
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

// registerProgressCompletion wires shell completion for the --progress flag to
// its four recognized values.
func registerProgressCompletion(cmd *cobra.Command) {
	err := cmd.RegisterFlagCompletionFunc(
		flagProgress,
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			modes := []string{
				config.ProgressModeAuto.String(),
				config.ProgressModeHuman.String(),
				config.ProgressModeJSON.String(),
				config.ProgressModeQuiet.String(),
			}

			return modes, cobra.ShellCompDirectiveNoFileComp
		},
	)
	if err != nil {
		panic(fmt.Sprintf("register %s completion: %v", flagProgress, err))
	}
}

// newVersionCmd returns a command that prints full build information.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		RunE: func(cc *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cc.OutOrStdout(), version.Get().String())
			if err != nil {
				return fmt.Errorf("print version: %w", err)
			}

			return nil
		},
	}
}
