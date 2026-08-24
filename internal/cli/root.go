package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.jacobcolvin.com/x/cobras/log"
	"go.jacobcolvin.com/x/cobras/profile"
	"go.jacobcolvin.com/x/version"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

const appName = "hcp_archiver"

// Flag names shared across commands. What and how to archive lives in the
// YAML configuration file; only per-run and operational settings are flags.
const (
	flagConfig           = "config"
	flagLogFile          = "log-file"
	flagProgress         = "progress"
	flagProgressInterval = "progress-interval"
)

var (
	// ErrLogHandler indicates an error occurred while creating a log handler.
	ErrLogHandler = errors.New("create log handler")

	// ErrLogFile indicates the --log-file destination could not be opened.
	ErrLogFile = errors.New("open log file")
)

// registerProgressFlags binds the progress output flags onto fs, writing into
// mode and interval, so every command carrying the pair shares one definition
// of its names, defaults, and help text.
func registerProgressFlags(fs *pflag.FlagSet, mode *string, interval *time.Duration) {
	fs.StringVar(mode, flagProgress, config.DefaultProgressMode.String(),
		"progress output mode (auto|human|json|quiet)")
	fs.DurationVar(interval, flagProgressInterval, config.DefaultProgressInterval,
		"progress reporting cadence")
}

// NewRootCmd builds the root [*cobra.Command] for the hcp_archiver CLI. Logging
// is configured from persistent flags in PersistentPreRunE so every subcommand
// shares it. The root command only dispatches: invoked without a subcommand it
// prints its help, and the run subcommand performs the archive.
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

The run subcommand performs the archive; its help explains the resume model
and the end-of-run status summary. The other subcommands browse, inspect, and
export an existing archive.

The API token comes from the environment: HCP_TOKEN, falling back to
TFC_TOKEN and then TFE_TOKEN, first non-empty wins. What and where to archive
lives in the YAML configuration file, which every command requires: --config
names it, then $HCP_ARCHIVER_CONFIG, then .hcp_archiver.yaml in the working
directory. Its one required key, archive.path, names the archive root.`,
		SilenceUsage: true,
		Version:      version.GetVersion(),
	}

	logCfg.RegisterFlags(cmd.PersistentFlags())
	logCfg.MustRegisterCompletions(cmd)

	var logFilePath string

	cmd.PersistentFlags().StringVar(&logFilePath, flagLogFile, "",
		"append the structured log stream to this file in addition to stderr")
	profileCfg.RegisterFlags(cmd.PersistentFlags())
	profileCfg.MustRegisterCompletions(cmd)

	profiler := profileCfg.NewProfiler()

	// One shared writer sits between the log handler and stderr: while the
	// terminal UI runs it hands log lines to the program so they scroll above
	// the pinned panel, otherwise it writes through to stderr. It is built in
	// PersistentPreRunE from the executing command's stderr (the same seam the
	// archiver resolves its writer at), so a redirected stderr routes logs too.
	var logWriter *progress.LogWriter

	cmd.PersistentPreRunE = func(cc *cobra.Command, _ []string) error {
		logWriter = progress.NewLogWriter(cc.ErrOrStderr())

		w := io.Writer(logWriter)

		// A file destination tees the handler's whole stream: the file gets
		// every line stderr does, so the events recording a past run's
		// destructive actions (each pruned remote key, discarded ledger
		// records, stranded-source warnings) survive the terminal session
		// instead of living only in the least durable place the process has.
		// The file stays open for the process lifetime exactly like the
		// stderr it mirrors, and its writes are unbuffered, so exit loses
		// nothing.
		if logFilePath != "" {
			//nolint:gosec // The log destination is operator-chosen.
			f, openErr := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if openErr != nil {
				return fmt.Errorf("%w: %w", ErrLogFile, openErr)
			}

			w = io.MultiWriter(logWriter, f)
		}

		h, err := logCfg.NewHandler(w)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrLogHandler, err)
		}

		logger := slog.New(h)
		slog.SetDefault(logger)

		// The logger also rides the invocation's context (see cmdLogger):
		// the process global serves the archiver's deep call tree, but a
		// command that logs from its own machinery must reach the logger
		// built for *its* stderr, not whichever invocation last replaced the
		// global.
		cc.SetContext(context.WithValue(cc.Context(), loggerKey{}, logger))

		if logFilePath != "" {
			// Announce the copy on the stream itself, so the file provably
			// receives the handler's output and the stderr reader learns a
			// durable copy exists. The announcement goes through this
			// invocation's logger, not the process default, which another
			// concurrent invocation (a parallel test) may have replaced.
			logger.LogAttrs(cc.Context(), slog.LevelDebug, "log_file_opened",
				slog.String("path", logFilePath))
		}

		return nil
	}

	// The run and export sinks close over logWriter by reference: the writer
	// only exists once PersistentPreRunE has run, so those commands resolve it
	// at run time rather than capturing the pre-run nil.
	cmd.AddCommand(newRunCmd(profiler, func() *progress.LogWriter { return logWriter }))
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newViewCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newShowCmd())
	cmd.AddCommand(newExtractCmd())
	cmd.AddCommand(newPullCmd())
	cmd.AddCommand(newExportCmd(func() progress.LogSink { return logWriter }))

	return cmd
}

// newViewCmd returns a command that browses the archive in an interactive
// terminal UI mirroring the HCP interface: organizations open into projects,
// workspaces, runs, and state versions. The directory comes from the
// configuration file's archive.path and may be the archive root or a single
// organization's directory.
func newViewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view",
		Short: "Browse the archive in an interactive terminal UI",
		Long: `Browse the archive in an interactive terminal UI mirroring the HCP interface:
organizations open into projects, workspaces, runs, and state versions. The
directory comes from the configuration file's archive.path and may be the
archive root or a single organization's directory.

` + remoteLong,
		Args: cobra.NoArgs,
	}

	cfgFlag := registerConfigFlag(cmd)

	cmd.RunE = func(cc *cobra.Command, _ []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		cfg, err := loadCmdConfig(*cfgFlag)
		if err != nil {
			return err
		}

		var opts []view.ArchiveOption

		if rcfg := remoteFromFile(cfg.file); rcfg != nil {
			opts = append(opts, view.WithRemote(*rcfg))
		}

		err = view.Browse(ctx, cfg.archiveDir, cc.InOrStdin(), cc.OutOrStdout(), opts...)
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

// loggerKey keys the invocation's logger in the command context, bound by
// the root command's PersistentPreRunE.
type loggerKey struct{}

// cmdLogger returns the logger built for this invocation's stderr, falling
// back to the process default when none was bound (a command run outside the
// root's pre-run, as in a direct test of a subcommand).
func cmdLogger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}

	return slog.Default()
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
