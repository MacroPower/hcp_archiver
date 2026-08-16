package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/export"
	"go.jacobcolvin.com/hcp_archiver/theme"
)

// flagForce names the export command's overwrite flag.
const flagForce = "force"

// newExportCmd returns a command that renders an archive's metadata as a
// markdown tree a static site generator can build.
func newExportCmd() *cobra.Command {
	var (
		target string
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "export [archive-dir]",
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

Pages render through Go text/template. A configuration file's export.templates
key names a directory of *.md.tmpl files overriding the built-in page
templates by filename; pages without an override keep their default, and a
relative path resolves against the configuration file's directory.

` + remoteLong + `

A single argument names the archive directory, defaulting to the configuration
file's archiveDir or, with none set, the current one. The file's extractDir
stands in for an omitted --target.`,
		Args: cobra.MaximumNArgs(1),
	}

	rf := registerRemoteFlags(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		// The file loads once and unconditionally: its export section and
		// directory defaults apply even when the mirror comes from the flag.
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

		if target == "" && file != nil {
			target = configDir(cfgPath, file.ExtractDir)
		}

		if target == "" {
			return export.ErrNoTarget
		}

		err = checkTargetOutside(dir, target)
		if err != nil {
			return err
		}

		arc, err := openArchive(ctx, dir, rcfg)
		if err != nil {
			return err
		}

		var opts []export.Option

		if force {
			opts = append(opts, export.WithForce())
		}

		if file != nil && file.Export.Templates != "" {
			opts = append(opts, export.WithTemplatesDir(configDir(cfgPath, file.Export.Templates)))
		}

		sum, err := export.New(arc, target, opts...).Run(ctx)

		warnDegraded(cc.ErrOrStderr(), arc)

		if err != nil {
			// The library sentinel is flag-agnostic; the flag hint belongs to
			// this consumer.
			if errors.Is(err, export.ErrTargetNotEmpty) {
				return fmt.Errorf("%w (use --%s to replace its contents)", err, flagForce)
			}

			return err //nolint:wrapcheck // Sentinel-bearing export errors render as-is.
		}

		eprintf(cc.OutOrStdout(), "exported %s for %s into %s\n",
			theme.CountNoun(sum.Pages, "page", "pages"),
			theme.CountNoun(sum.Orgs, "organization", "organizations"), target)

		return nil
	}

	flags := cmd.Flags()
	flags.StringVarP(&target, flagTarget, "t", "",
		"directory to write the markdown tree into (defaults to the configuration file's extractDir)")
	flags.BoolVar(&force, flagForce, false, "replace a non-empty target directory's contents")

	return cmd
}
