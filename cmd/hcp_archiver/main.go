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
	"github.com/spf13/pflag"
	"go.jacobcolvin.com/x/cobras/log"
	"go.jacobcolvin.com/x/cobras/profile"
	"go.jacobcolvin.com/x/version"

	"go.jacobcolvin.com/hcp_archiver/archiver"
	"go.jacobcolvin.com/hcp_archiver/config"
)

const appName = "hcp_archiver"

// Flag names bound onto the root command.
const (
	flagAddress          = "address"
	flagOrganization     = "organization"
	flagOrgAlias         = "org"
	flagOutput           = "output"
	flagConcurrency      = "concurrency"
	flagProgress         = "progress"
	flagProgressInterval = "progress-interval"
	flagRecheckAbsent    = "recheck-absent"
	flagStacks           = "stacks"
	flagHYOK             = "hyok"
	flagRegistryDetail   = "registry-detail"
	flagAuditTrail       = "audit-trail"
)

// ErrLogHandler indicates an error occurred while creating a log handler.
var ErrLogHandler = errors.New("create log handler")

func main() {
	err := fang.Execute(
		context.Background(),
		newRootCmd(),
		fang.WithVersion(version.GetVersion()),
	)
	if err != nil {
		os.Exit(1)
	}
}

// archiveFlags holds the raw flag values bound onto the root command. Its
// config method resolves them into a validated [config.Config].
type archiveFlags struct {
	address          string
	organization     string
	output           string
	progress         string
	progressInterval time.Duration
	concurrency      int
	recheckAbsent    bool
	stacks           bool
	hyok             bool
	registryDetail   bool
	auditTrail       bool
}

// registerArchiveFlags binds the archive flags onto cmd and returns the
// [*archiveFlags] they write into.
func registerArchiveFlags(cmd *cobra.Command) *archiveFlags {
	af := &archiveFlags{}
	fs := cmd.Flags()

	fs.StringVar(&af.address, flagAddress, config.DefaultAddress,
		"HCP Terraform API address")
	fs.StringVar(&af.organization, flagOrganization, "",
		"organization to archive (empty archives every visible organization)")
	fs.StringVarP(&af.output, flagOutput, "o", "",
		"archive root directory (required)")
	fs.IntVar(&af.concurrency, flagConcurrency, config.DefaultWorkspaceConcurrency,
		"number of workspaces archived concurrently")
	fs.StringVar(&af.progress, flagProgress, config.DefaultProgressMode.String(),
		"progress output mode (auto|human|json|quiet)")
	fs.DurationVar(&af.progressInterval, flagProgressInterval, config.DefaultProgressInterval,
		"progress reporting cadence")
	fs.BoolVar(&af.recheckAbsent, flagRecheckAbsent, false,
		"re-probe objects previously recorded as permanently gone")
	fs.BoolVar(&af.stacks, flagStacks, false,
		"archive Stacks")
	fs.BoolVar(&af.hyok, flagHYOK, false,
		"archive hold-your-own-key configurations")
	fs.BoolVar(&af.registryDetail, flagRegistryDetail, false,
		"archive deeper registry version, platform, and binary detail")
	fs.BoolVar(&af.auditTrail, flagAuditTrail, false,
		"archive the audit trail")

	fs.SetNormalizeFunc(orgAliasNormalizer)

	return af
}

// organizationsFromFlag maps the single --organization value onto the config's
// organization list: an empty value selects every visible organization.
func organizationsFromFlag(org string) []string {
	if org == "" {
		return nil
	}

	return []string{org}
}

// orgAliasNormalizer maps the --org alias onto the canonical --organization
// flag name.
func orgAliasNormalizer(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if name == flagOrgAlias {
		name = flagOrganization
	}

	return pflag.NormalizedName(name)
}

// config resolves the bound flag values into a validated [config.Config]. The
// token comes from the environment, so a missing token surfaces here as
// [config.ErrMissingToken].
func (af *archiveFlags) config() (*config.Config, error) {
	mode, err := config.ParseProgressMode(af.progress)
	if err != nil {
		return nil, err
	}

	return config.New(
		config.WithAddress(af.address),
		config.WithOrganizations(organizationsFromFlag(af.organization)),
		config.WithOutputDir(af.output),
		config.WithProgressMode(mode),
		config.WithProgressInterval(af.progressInterval),
		config.WithWorkspaceConcurrency(af.concurrency),
		config.WithRecheckAbsent(af.recheckAbsent),
		config.WithStacks(af.stacks),
		config.WithHYOK(af.hyok),
		config.WithRegistryDetail(af.registryDetail),
		config.WithAuditTrail(af.auditTrail),
	)
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

	cmd.PersistentPreRunE = func(cc *cobra.Command, _ []string) error {
		h, err := logCfg.NewHandler(cc.ErrOrStderr())
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
			return runArchive(cc, af)
		})
	}

	cmd.AddCommand(newVersionCmd())

	return cmd
}

// runArchive resolves the flags into a configuration and archives under a
// signal-aware context. A graceful interrupt exits cleanly.
func runArchive(cmd *cobra.Command, af *archiveFlags) error {
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
