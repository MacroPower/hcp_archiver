package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"
	"go.jacobcolvin.com/niceyaml/fangs"
	"go.jacobcolvin.com/x/cobras/log"
	"go.jacobcolvin.com/x/cobras/profile"
	"go.jacobcolvin.com/x/version"

	"go.jacobcolvin.com/hcp_archiver/archiver"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/progress"
	"go.jacobcolvin.com/hcp_archiver/view"
)

const appName = "hcp_archiver"

// Flag names bound onto the root command. What and how to archive lives in the
// YAML configuration file; only per-run and operational settings are flags.
const (
	flagConfig           = "config"
	flagOutput           = "output"
	flagProgress         = "progress"
	flagProgressInterval = "progress-interval"
	flagRecheckAbsent    = "recheck-absent"
)

// ErrLogHandler indicates an error occurred while creating a log handler.
var ErrLogHandler = errors.New("create log handler")

func main() {
	err := fang.Execute(
		context.Background(),
		newRootCmd(),
		fang.WithVersion(version.GetVersion()),
		// Preserve the multi-line, source-annotated formatting of a niceyaml
		// configuration error instead of collapsing it into a single styled block.
		fang.WithErrorHandler(fangs.ErrorHandler),
	)
	if err != nil {
		os.Exit(1)
	}
}

// archiveFlags holds the raw flag values bound onto the root command. Its
// config method loads the YAML configuration and merges these per-run settings
// into a validated [config.Config].
type archiveFlags struct {
	configPath       string
	output           string
	progress         string
	progressInterval time.Duration
	recheckAbsent    bool
}

// registerArchiveFlags binds the archive flags onto cmd and returns the
// [*archiveFlags] they write into.
func registerArchiveFlags(cmd *cobra.Command) *archiveFlags {
	af := &archiveFlags{}
	fs := cmd.Flags()

	fs.StringVarP(&af.configPath, flagConfig, "c", "",
		fmt.Sprintf("path to the YAML configuration file (defaults to $%s)", config.EnvConfigPath))
	fs.StringVarP(&af.output, flagOutput, "o", "",
		"archive root directory (required)")
	fs.StringVar(&af.progress, flagProgress, config.DefaultProgressMode.String(),
		"progress output mode (auto|human|json|quiet)")
	fs.DurationVar(&af.progressInterval, flagProgressInterval, config.DefaultProgressInterval,
		"progress reporting cadence")
	fs.BoolVar(&af.recheckAbsent, flagRecheckAbsent, false,
		"re-probe objects previously recorded as permanently gone")

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

	file, err := af.loadFile()
	if err != nil {
		return nil, err
	}

	return config.New(
		config.WithAddress(file.Address),
		config.WithOrganizations(file.Organizations),
		config.WithWorkspaceConcurrency(file.Concurrency),
		config.WithStacks(file.Scope.Stacks),
		config.WithHYOK(file.Scope.HYOK),
		config.WithRegistryDetail(file.Scope.RegistryDetail),
		config.WithAuditTrail(file.Scope.AuditTrail),
		config.WithOutputDir(af.output),
		config.WithProgressMode(mode),
		config.WithProgressInterval(af.progressInterval),
		config.WithRecheckAbsent(af.recheckAbsent),
	)
}

// loadFile resolves the configuration file path from the --config flag, then
// the environment, and loads it. When neither names a path the built-in
// defaults are used and no file is read.
func (af *archiveFlags) loadFile() (*config.File, error) {
	path := af.configPath
	if path == "" {
		path = os.Getenv(config.EnvConfigPath)
	}

	if path == "" {
		return config.DefaultFile(), nil
	}

	return config.LoadFile(path)
}

// newRootCmd builds the root [*cobra.Command] for the hcp_archiver CLI. Logging
// is configured from persistent flags in PersistentPreRunE so every subcommand
// shares it. The root command runs the archive inside the profiler, so its
// profiles are written even when the archive errors; the only subcommand
// reports version information.
func newRootCmd() *cobra.Command {
	logCfg := log.NewConfig()
	profileCfg := profile.NewConfig()

	cmd := &cobra.Command{
		Use:          appName,
		Short:        "Archive an HCP Terraform organization to disk.",
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

	return cmd
}

// newViewCmd returns a command that browses an existing archive in an
// interactive terminal UI mirroring the HCP interface: organizations open into
// projects, workspaces, runs, and state versions. The directory may be the
// archive root or a single organization's directory; it defaults to the
// current directory.
func newViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "view [archive-dir]",
		Short: "Browse an archive in an interactive terminal UI",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cc *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}

			ctx, stop := signal.NotifyContext(cc.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			err := view.Browse(ctx, dir, cc.InOrStdin(), cc.OutOrStdout())
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}

				return err
			}

			return nil
		},
	}
}

// runArchive resolves the flags into a configuration and archives under a
// signal-aware context. A graceful interrupt exits cleanly. It passes logWriter
// as the log sink so, while the terminal UI runs, the collectors' log output
// routes through the one renderer that owns the screen.
func runArchive(cmd *cobra.Command, af *archiveFlags, logWriter *progress.LogWriter) error {
	cfg, err := af.config()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
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
			cc.Println(version.Get().String())

			return nil
		},
	}
}
