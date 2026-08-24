package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/export"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
)

// flagForce names the export command's overwrite flag.
const flagForce = "force"

// phaseOpen names the phase covering the archive open, the one stage that runs
// before the exporter names its own; a sealed archive fetches its offloaded
// roll-ups back here, so it is worth labeling.
const phaseOpen = "open"

// newExportCmd returns a command that renders an archive's metadata as a
// markdown tree a static site generator can build. The sink resolves the
// shared log writer the root command builds in PersistentPreRunE, so log
// lines scroll above the progress panel while it runs.
func newExportCmd(sink func() progress.LogSink) *cobra.Command {
	var (
		force            bool
		progressFlag     string
		progressInterval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Render an archive's metadata as a browsable markdown tree",
		Long: `Render an archive's non-sensitive metadata as a tree of markdown files a
static site generator with directory-based navigation (mkdocs and its kin)
can build into a browsable view of the backup.

Pages carry metadata only: workspace settings, variable keys (values shown
only for non-sensitive variables), run and state-version history, and
org-scope objects. Content that can embed secret values, such as state blobs,
plan and apply logs, and cost estimates, is represented by presence alone:
names, sizes, and timestamps, never bytes.

The target directory is created when absent and refused when non-empty;
--force replaces its contents. Exporting a sealed archive whose roll-ups were
offloaded fetches that metadata back from the mirror.

Pages render through Go text/template. A configuration file's
export.templates.path key names a directory of *.md.tmpl files overriding the
built-in page templates by filename; pages without an override keep their
default, and a relative path resolves against the configuration file's
directory.

` + remoteLong + `

The archive directory comes from the configuration file's archive.path, and
the markdown tree is written into the directory its export.path names.`,
		Args: cobra.NoArgs,
	}

	cfgFlag := registerConfigFlag(cmd)

	cmd.RunE = func(cc *cobra.Command, _ []string) error {
		// The mode parses before any I/O, so a bad value fails without
		// touching the archive or the target.
		mode, err := config.ParseProgressMode(progressFlag)
		if err != nil {
			return err //nolint:wrapcheck // The sentinel-bearing config error renders as-is.
		}

		ctx, stop := signalContext(cc.Context())
		defer stop()

		// The run gets its own cancelable child: under the terminal UI's raw
		// mode the kernel never raises SIGINT, so ctrl+c arrives through the
		// reporter's interrupt callback instead.
		runCtx, cancelRun := context.WithCancel(ctx)
		defer cancelRun()

		cfg, err := loadCmdConfig(*cfgFlag)
		if err != nil {
			return err
		}

		target := configDir(cfg.path, cfg.file.Export.Path)
		if target == "" {
			return fmt.Errorf("%w (set export.path in the configuration file)", export.ErrNoTarget)
		}

		// The reporter starts before the archive opens: a sealed archive's
		// roll-up fetch-back from the mirror can be the slow part of the
		// command, so the open runs under its own named phase, with the
		// interrupt path already live. Nothing has counted the archive yet, so
		// the phase carries no total; the export names its own from here on.
		prog := &exportProgress{}
		reporter := progress.New(cc.ErrOrStderr(), mode, prog,
			progress.WithInterval(progressInterval),
			progress.WithInterrupt(cancelRun),
			progress.WithLogSink(sink()),
		)
		prog.reporter = reporter
		reporter.SetPhase(phaseOpen)

		// Every exit past this point erases the panel first, so an error
		// prints on a restored terminal; the success path stops it inline
		// before the stderr warning and the stdout summary.
		stopReporter := reporter.RunBackground(ctx, nil)
		defer stopReporter()

		// No slow-listing notice: the reporter above owns stderr, and a line
		// written from a timer goroutine would corrupt whatever shape it is
		// serving there, a panel or a stream of JSON events alike.
		arc, err := cfg.open(runCtx, nil)
		if err != nil {
			return err
		}

		// The guard needs the organization names the open discovered, since an
		// organization's directory under the target can reach back into the
		// archive even when the target itself sits outside it.
		err = checkTargetOutside(cfg.archiveDir, target, orgNames(arc))
		if err != nil {
			return err
		}

		var opts []export.Option

		if force {
			opts = append(opts, export.WithForce())
		}

		if cfg.file.Export.Templates.Path != "" {
			opts = append(opts, export.WithTemplatesDir(configDir(cfg.path, cfg.file.Export.Templates.Path)))
		}

		opts = append(opts, export.WithProgress(prog))

		sum, err := export.New(arc, target, opts...).Run(runCtx)

		stopReporter()

		warnDegraded(cc.ErrOrStderr(), arc)

		if err != nil {
			// The library sentinel is flag-agnostic; the flag hint belongs to
			// this consumer.
			if errors.Is(err, export.ErrTargetNotEmpty) {
				return fmt.Errorf("%w (use --%s to replace its contents)", err, flagForce)
			}

			return err //nolint:wrapcheck // Sentinel-bearing export errors render as-is.
		}

		// The summary is the command's only output, so a stdout write fault
		// surfaces rather than exiting 0 in silence, matching extract's
		// summary; eprintf's swallowing is for best-effort stderr progress.
		_, err = fmt.Fprintf(cc.OutOrStdout(), "exported %s for %s into %s\n",
			theme.CountNoun(sum.Pages, "page", "pages"),
			theme.CountNoun(sum.Orgs, "organization", "organizations"), target)
		if err != nil {
			return fmt.Errorf("write summary: %w", err)
		}

		return nil
	}

	flags := cmd.Flags()
	flags.BoolVar(&force, flagForce, false, "replace a non-empty target directory's contents")
	registerProgressFlags(flags, &progressFlag, &progressInterval)

	// Completion registration panics on a missing flag, so it follows the
	// --progress registration above.
	registerProgressCompletion(cmd)

	return cmd
}
