package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/theme"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// Flag names shared by the inspect commands.
const (
	flagJSON    = "json"
	flagTarget  = "target"
	flagDryRun  = "dry-run"
	flagVerbose = "verbose"
)

var (
	// ErrUnsealIncomplete indicates an unseal run finished but some objects
	// could not be read or written.
	ErrUnsealIncomplete = errors.New("some objects could not be unsealed")

	// ErrTargetInArchive indicates an unseal target sits inside the archive
	// directory being read, where recovered files would be written into the
	// archive itself.
	ErrTargetInArchive = errors.New("unseal target must be outside the archive directory")
)

// inspectLong is the addressing contract the three read commands share.
const inspectLong = `Objects are addressed by org-prefixed archive paths ("<org>/<path>"), the
same layout an unseal reproduces beneath its target, whether the archive
directory is the archive root or a single organization's directory.

Positional arguments bind left to right: with two arguments the first is the
archive directory and the second the archive path.`

// newListCmd returns a command that lists archived objects as plain text or
// NDJSON, one line per object, transparent to sealing.
func newListCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list [archive-dir] [path]",
		Short: "List archived objects under a path prefix",
		Long: `List the archived objects at or beneath an archive path, one line per object
whichever physical form (loose, roll-up, bundle) holds it.

` + inspectLong + `

A single argument names the archive directory; pass "." explicitly to address
a path in the current directory.`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cc *cobra.Command, args []string) error {
			dir, prefix := archiveArgs(args)

			ctx, stop := signalContext(cc.Context())
			defer stop()

			arc, err := openArchive(ctx, dir)
			if err != nil {
				return hintLoneArg(err, "list", args)
			}

			entries, err := arc.List(prefix)
			if err != nil {
				return describeNoOrg(err, arc)
			}

			if jsonOut {
				return writeEntriesJSON(cc.OutOrStdout(), entries)
			}

			return writeEntriesText(cc.OutOrStdout(), entries)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, flagJSON, false, "emit NDJSON, one object per line")

	return cmd
}

// newShowCmd returns a command that prints one archived object's exact bytes
// to stdout, whichever physical form holds it.
func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [archive-dir] <path>",
		Short: "Print one archived object's bytes to stdout",
		Long: `Print the exact bytes of one archived object to stdout, whichever physical
form (loose, roll-up, bundle) holds it.

` + inspectLong + `

A single argument is the archive path, read from the archive in the current
directory.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cc *cobra.Command, args []string) error {
			dir, archivePath := showArgs(args)

			ctx, stop := signalContext(cc.Context())
			defer stop()

			arc, err := openArchive(ctx, dir)
			if err != nil {
				return err
			}

			data, err := arc.Read(archivePath)
			if err != nil {
				if errors.Is(err, view.ErrNotFile) {
					return fmt.Errorf("%w (try: %s list %s %s)", err, appName, dir, archivePath)
				}

				return describeNoOrg(err, arc)
			}

			_, err = cc.OutOrStdout().Write(data)
			if err != nil {
				return fmt.Errorf("write output: %w", err)
			}

			return nil
		},
	}
}

// newUnsealCmd returns a command that extracts archived objects into a plain
// directory tree, expanding sealed forms back into loose files.
func newUnsealCmd() *cobra.Command {
	var (
		target  string
		dryRun  bool
		verbose bool
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "unseal [archive-dir] [path]",
		Short: "Extract archived objects into a plain directory tree",
		Long: `Extract every archived object at or beneath an archive path into a target
directory, expanding roll-ups and bundles back into loose files. The target
reproduces the "<org>/<path>" layout, so a listed path names the same file
after recovery.

` + inspectLong + `

A single argument names the archive directory; pass "." explicitly to address
a path in the current directory. With --dry-run the plan is summarized from
the listing and nothing is written; --target is not required.`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cc *cobra.Command, args []string) error {
			dir, prefix := archiveArgs(args)

			ctx, stop := signalContext(cc.Context())
			defer stop()

			arc, err := openArchive(ctx, dir)
			if err != nil {
				return hintLoneArg(err, "unseal", args)
			}

			if dryRun {
				return unsealDryRun(cc.OutOrStdout(), cc.ErrOrStderr(), arc, prefix, target, jsonOut)
			}

			if target == "" {
				return view.ErrNoTarget
			}

			err = checkTargetOutside(dir, target)
			if err != nil {
				return err
			}

			return runUnsealCmd(ctx, cc, arc, prefix, target, verbose, jsonOut)
		},
	}

	fs := cmd.Flags()
	fs.StringVarP(&target, flagTarget, "t", "", "directory to write recovered files into")
	fs.BoolVar(&dryRun, flagDryRun, false, "summarize what would be unsealed without writing")
	fs.BoolVarP(&verbose, flagVerbose, "v", false, "stream one line per recovered file to stderr")
	fs.BoolVar(&jsonOut, flagJSON, false, "emit the summary as JSON")

	return cmd
}

// runUnsealCmd drives one unseal run and reports its summary. Per-file
// failures always stream to stderr; verbose adds a line per recovered file.
// An interrupt reports the partial totals to stderr and exits cleanly,
// matching the archive command's signal semantics.
func runUnsealCmd(ctx context.Context, cc *cobra.Command, arc *view.Archive,
	prefix, target string, verbose, jsonOut bool,
) error {
	errW := cc.ErrOrStderr()

	progress := func(archivePath string, bytes int64, err error) {
		switch {
		case err != nil:
			eprintf(errW, "%s: %v\n", archivePath, err)
		case verbose:
			eprintf(errW, "%s (%s)\n", archivePath, theme.HumanBytes(bytes))
		}
	}

	sum, err := arc.Unseal(ctx, target, prefix, progress)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			eprintf(errW, "unseal interrupted: %s (%s) written into %s\n",
				theme.CountNoun(sum.Files, "object", "objects"), theme.HumanBytes(sum.Bytes), target)

			return nil
		}

		return describeNoOrg(err, arc)
	}

	err = writeUnsealSummary(cc.OutOrStdout(), sum, target, false, jsonOut)
	if err != nil {
		return err
	}

	if sum.Errored > 0 {
		return fmt.Errorf("%w: %d of %d objects", ErrUnsealIncomplete, sum.Errored, sum.Errored+sum.Files)
	}

	return nil
}

// unsealDryRun summarizes an unseal from the listing without writing: the
// listing carries the same entries in the same order as the plan the real run
// executes, so the totals predict it.
//
// An object the listing reports as remote-only counts as errored rather than
// as a file to write, and when the plan holds any the dry run exits 1 the way
// the run would. Each is named on stderr, where the real run streams its
// per-file failures, so the answer to "which ones" does not need a second
// command. The prediction is a lower bound on the failures: a bundle that
// turns out to be corrupt errors only when something tries to read it, which a
// dry run never does, so a clean dry run does not promise a clean run.
func unsealDryRun(out, errW io.Writer, arc *view.Archive, prefix, target string, jsonOut bool) error {
	entries, err := arc.List(prefix)
	if err != nil {
		return describeNoOrg(err, arc)
	}

	var sum view.UnsealSummary

	for _, e := range entries {
		if e.RemoteOnly() {
			sum.Errored++

			eprintf(errW, "%s: cannot be unsealed; its bytes are in the remote store\n", e.ArchivePath())

			continue
		}

		sum.Files++
		sum.Bytes += e.Size
	}

	err = writeUnsealSummary(out, sum, target, true, jsonOut)
	if err != nil {
		return err
	}

	if sum.Errored > 0 {
		return fmt.Errorf("%w: %d of %d objects", ErrUnsealIncomplete, sum.Errored, sum.Errored+sum.Files)
	}

	return nil
}

// unsealReport is the wire shape of an unseal summary under --json.
type unsealReport struct {
	Target  string `json:"target"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Errored int    `json:"errored"`
	DryRun  bool   `json:"dryRun"`
}

// writeUnsealSummary reports one run's totals to out, as one human line or one
// JSON object.
func writeUnsealSummary(out io.Writer, sum view.UnsealSummary, target string, dryRun, jsonOut bool) error {
	if jsonOut {
		err := json.NewEncoder(out).Encode(unsealReport{
			Target:  target,
			Files:   sum.Files,
			Bytes:   sum.Bytes,
			Errored: sum.Errored,
			DryRun:  dryRun,
		})
		if err != nil {
			return fmt.Errorf("encode summary: %w", err)
		}

		return nil
	}

	var b strings.Builder

	if dryRun {
		b.WriteString("would unseal ")
	} else {
		b.WriteString("unsealed ")
	}

	fmt.Fprintf(&b, "%s (%s)", theme.CountNoun(sum.Files, "object", "objects"), theme.HumanBytes(sum.Bytes))

	if target != "" {
		b.WriteString(" into " + target)
	}

	if sum.Errored > 0 {
		// A dry run has not tried anything yet, so nothing has errored; what it
		// counted is what it can already tell will not come back.
		if dryRun {
			fmt.Fprintf(&b, "; %d not recoverable", sum.Errored)
		} else {
			fmt.Fprintf(&b, "; %d errored", sum.Errored)
		}
	}

	b.WriteString("\n")

	_, err := io.WriteString(out, b.String())
	if err != nil {
		return fmt.Errorf("write summary: %w", err)
	}

	return nil
}

// listRecord is the wire shape of one listed object under --json: a
// command-local contract decoupled from the library's [view.Entry], with
// every path org-prefixed so it is directly addressable with show.
type listRecord struct {
	Path      string `json:"path"`
	Org       string `json:"org"`
	Form      string `json:"form"`
	Container string `json:"container,omitempty"`
	Modified  string `json:"modified,omitempty"`
	Size      int64  `json:"size"`
	Offloaded bool   `json:"offloaded,omitempty"`
}

// writeEntriesJSON emits one JSON object per entry, NDJSON-style.
func writeEntriesJSON(out io.Writer, entries []view.Entry) error {
	enc := json.NewEncoder(out)

	for _, e := range entries {
		rec := listRecord{
			Path:      e.ArchivePath(),
			Org:       e.Org,
			Form:      string(e.Form),
			Size:      e.Size,
			Offloaded: e.Offloaded,
		}

		if e.Container != "" {
			rec.Container = path.Join(e.Org, e.Container)
		}

		if !e.ModTime.IsZero() {
			rec.Modified = e.ModTime.UTC().Format(time.RFC3339)
		}

		err := enc.Encode(rec)
		if err != nil {
			return fmt.Errorf("encode entry %q: %w", rec.Path, err)
		}
	}

	return nil
}

// writeEntriesText renders entries as aligned columns: size, form, path. An
// evicted object displays as "remote", flagging that its bytes are in the
// mirror rather than here.
func writeEntriesText(out io.Writer, entries []view.Entry) error {
	sizes := make([]string, len(entries))

	var sizeW, formW int

	for i, e := range entries {
		sizes[i] = theme.HumanBytes(e.Size)
		sizeW = max(sizeW, len(sizes[i]))
		formW = max(formW, len(displayForm(e)))
	}

	var b strings.Builder

	for i, e := range entries {
		fmt.Fprintf(&b, "%*s  %-*s  %s\n", sizeW, sizes[i], formW, displayForm(e), e.ArchivePath())
	}

	_, err := io.WriteString(out, b.String())
	if err != nil {
		return fmt.Errorf("write listing: %w", err)
	}

	return nil
}

// displayForm names an entry's physical form for the text listing, folding
// every evicted object into "remote": what matters to a reader of the listing
// is that the bytes are not here, not which shape held them before they left.
func displayForm(e view.Entry) string {
	if e.Offloaded {
		return "remote"
	}

	return string(e.Form)
}

// archiveArgs binds the list/unseal positionals to (directory, archive path):
// required arguments first, so a lone argument names the archive directory
// and the optional archive path stays empty (the whole archive).
func archiveArgs(args []string) (string, string) {
	switch len(args) {
	case 1:
		return args[0], ""
	case 2: //nolint:mnd // Two positionals: dir then path, per the Use line.
		return args[0], args[1]
	default:
		return ".", ""
	}
}

// showArgs binds the show positionals to (directory, archive path): the
// archive path is required, so a lone argument is the path and the directory
// defaults to the current one.
func showArgs(args []string) (string, string) {
	if len(args) == 1 {
		return ".", args[0]
	}

	return args[0], args[1]
}

// openArchive opens the archive at dir under ctx and wraps its organizations
// in a [*view.Archive].
func openArchive(ctx context.Context, dir string) (*view.Archive, error) {
	orgs, err := view.OpenArchive(dir, view.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	return view.NewArchive(orgs), nil
}

// hintLoneArg augments a not-an-archive failure when a lone positional was
// most likely meant as an archive path: the argument named no archive but the
// current directory holds one.
func hintLoneArg(err error, cmdName string, args []string) error {
	if !errors.Is(err, view.ErrNotArchive) || len(args) != 1 {
		return err
	}

	_, cwdErr := view.OpenArchive(".")
	if cwdErr != nil {
		return err
	}

	return fmt.Errorf("%w\na lone argument names the archive directory; to address a path here, run: %s %s . %s",
		err, appName, cmdName, args[0])
}

// describeNoOrg augments an unknown-organization failure with the archive's
// known organization names.
func describeNoOrg(err error, arc *view.Archive) error {
	if !errors.Is(err, view.ErrNoOrg) {
		return err
	}

	names := make([]string, 0, len(arc.Orgs()))
	for _, org := range arc.Orgs() {
		names = append(names, org.Name)
	}

	return fmt.Errorf("%w (known organizations: %s)", err, strings.Join(names, ", "))
}

// checkTargetOutside refuses a target inside the archive directory. The
// comparison is segment-wise over absolute paths, so a sibling like
// "archive-backup" beside "archive" is not flagged. Symlinks are not
// resolved: a symlinked target can dodge the check, which is accepted rather
// than pulling physical identity resolution into a CLI guard.
func checkTargetOutside(archiveDir, target string) error {
	absArchive, err := filepath.Abs(archiveDir)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", archiveDir, err)
	}

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target, err)
	}

	rel, err := filepath.Rel(absArchive, absTarget)
	if err != nil {
		// No relative path between the two (a different volume): the target
		// cannot be inside the archive.
		return nil //nolint:nilerr // Unrelatable paths are definitionally outside.
	}

	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s is inside %s", ErrTargetInArchive, target, archiveDir)
	}

	return nil
}

// eprintf writes best-effort progress to w; a stderr write fault mid-run has
// no recovery path.
func eprintf(w io.Writer, format string, args ...any) {
	//nolint:errcheck // Best-effort progress output.
	_, _ = fmt.Fprintf(w, format, args...)
}
