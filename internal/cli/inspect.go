package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/archiver"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// Flag names shared by the inspect commands.
const (
	flagJSON    = "json"
	flagDryRun  = "dry-run"
	flagVerbose = "verbose"
)

var (
	// ErrExtractIncomplete indicates an extract run finished but some objects
	// could not be read or written.
	ErrExtractIncomplete = errors.New("some objects could not be extracted")

	// ErrTargetInArchive indicates an extract or export target sits inside the
	// archive directory being read, where the written files would land in the
	// archive itself.
	ErrTargetInArchive = errors.New("target must be outside the archive directory")
)

// inspectLong is the addressing contract the three read commands share.
const inspectLong = `Objects are addressed by org-prefixed archive paths ("<org>/<path>"), the
same layout an extract reproduces beneath its target, whether the configured
archive directory is the archive root or a single organization's directory.`

// remoteLong is the mirror read-through contract the browse and inspect
// commands share.
const remoteLong = `The archive's object-store mirror can stand in for absent local files: the
configuration file's remote section names it. Anything read that is not on
disk is fetched from the mirror and persisted at its local archive path, so
later reads need no network; a directory holding nothing at all is
bootstrapped from the mirror outright, and the recorded marker means the
remote section is only needed once. A configured remote that disagrees with
the mirror an organization's marker records is refused. Object-store
credentials come from the backend provider's default chain.`

// remoteFromFile turns a loaded configuration file's remote section into the
// remote configuration the archive opens with; nil means no remote is named,
// leaving the archive to its local tree and markers.
func remoteFromFile(file *config.File) *remote.Config {
	if file.Remote.IsZero() {
		return nil
	}

	rc := file.Remote.RemoteConfig()
	cfg := archiver.RemoteConfig(&rc)

	return &cfg
}

// warnDegraded reports each organization whose mirror listing failed, so a
// result covering local content only never passes silently for the whole
// archive. It writes at most one line per organization per run.
func warnDegraded(errW io.Writer, arc *view.Archive) {
	for _, org := range arc.Orgs() {
		err := org.RemoteWarning()
		if err != nil {
			eprintf(errW, "warning: organization %q: mirror unreachable; results cover local content only (%v)\n",
				org.Name, err)
		}
	}
}

// listNotice returns the callback [view.WithListNotice] takes, naming an
// organization whose mirror listing is running long. The commands it serves
// print nothing until they finish, so a large mirror's enumeration would
// otherwise look like a hang.
//
// The callback fires on a timer goroutine while the command writes its own
// output, so a mutex guards the writer; it lands on stderr, leaving stdout
// clean for the bytes a caller is piping.
func listNotice(errW io.Writer) func(org string) {
	var mu sync.Mutex

	return func(org string) {
		mu.Lock()
		defer mu.Unlock()

		eprintf(errW, "listing the mirror's inventory for organization %q; a large mirror takes a while\n", org)
	}
}

// newListCmd returns a command that lists archived objects as plain text or
// NDJSON, one line per object, transparent to sealing.
func newListCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list [path]",
		Short: "List archived objects under a path prefix",
		Long: `List the archived objects at or beneath an archive path, one line per object
whichever physical form (loose, roll-up, bundle) holds it. The archive
directory comes from the configuration file's archive.path.

The text listing prints size, physical form, and path, with sealed members
labeled by the form that carries them and anything evicted to the mirror
shown as "remote" (reading one back needs object-store credentials). With
--json each line is one JSON object carrying "path", "org", "form", and
"size", plus "container", "modified", and "offloaded" where they apply.

` + inspectLong + `

` + remoteLong + `

The optional argument narrows the listing to one archive path's subtree; with
none the whole archive is listed.`,
		Args: cobra.MaximumNArgs(1),
	}

	cfgFlag := registerConfigFlag(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		cfg, err := loadCmdConfig(*cfgFlag)
		if err != nil {
			return err
		}

		var prefix string

		if len(args) == 1 {
			prefix = args[0]
		}

		arc, err := cfg.open(ctx, listNotice(cc.ErrOrStderr()))
		if err != nil {
			return err
		}

		entries, err := arc.List(prefix)

		// Whatever the listing answered, a degraded mirror is context the
		// reader needs: an unknown organization may exist only in the mirror
		// the session could not list, and a clean listing still covered local
		// content alone.
		warnDegraded(cc.ErrOrStderr(), arc)

		if err != nil {
			return hintDirArg(describeNoOrg(err, arc), args)
		}

		if jsonOut {
			return writeEntriesJSON(cc.OutOrStdout(), entries)
		}

		return writeEntriesText(cc.OutOrStdout(), entries)
	}

	cmd.Flags().BoolVar(&jsonOut, flagJSON, false, "emit NDJSON, one object per line")

	return cmd
}

// newShowCmd returns a command that prints one archived object's exact bytes
// to stdout, whichever physical form holds it.
func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <path>",
		Short: "Print one archived object's bytes to stdout",
		Long: `Print the exact bytes of one archived object to stdout, whichever physical
form (loose, roll-up, bundle) holds it. The archive directory comes from the
configuration file's archive.path.

The one object show refuses is an evicted configuration-version tarball: it
holds what it reads whole in memory and a tarball has no bound, so the error
names the object's mirrored key instead, to fetch with any client for the
backing store (extract streams such an object to disk without this limit).

` + inspectLong + `

` + remoteLong,
		Args: cobra.ExactArgs(1),
	}

	cfgFlag := registerConfigFlag(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		cfg, err := loadCmdConfig(*cfgFlag)
		if err != nil {
			return err
		}

		archivePath := args[0]

		arc, err := cfg.open(ctx, listNotice(cc.ErrOrStderr()))
		if err != nil {
			return err
		}

		data, err := arc.Read(archivePath)

		// Whatever the read answered, a degraded mirror is context the reader
		// needs: a miss may be explained by it, and a hit does not disprove it.
		warnDegraded(cc.ErrOrStderr(), arc)

		if err != nil {
			if errors.Is(err, view.ErrNotFile) {
				return fmt.Errorf("%w (try: %s list %s)", err, appName, archivePath)
			}

			return hintDirArg(describeNoOrg(err, arc), args)
		}

		_, err = cc.OutOrStdout().Write(data)
		if err != nil {
			return fmt.Errorf("write output: %w", err)
		}

		return nil
	}

	return cmd
}

// newExtractCmd returns a command that extracts archived objects into a plain
// directory tree, expanding sealed forms back into loose files.
func newExtractCmd() *cobra.Command {
	var (
		dryRun  bool
		verbose bool
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "extract [path]",
		Short: "Extract archived objects into a plain directory tree",
		Long: `Extract every archived object at or beneath an archive path into the target
directory the configuration file's extract.path names, expanding roll-ups and
bundles back into loose files. The archive directory comes from the file's
archive.path, and the target reproduces the "<org>/<path>" layout, so a
listed path names the same file after recovery.

An object whose bytes were evicted to the archive's mirror is fetched back
from it, needing object-store credentials from the backend provider's default
chain. An organization whose archive root records no mirror leaves one
unrecoverable before the run starts, counted and named among the run's
failures; a fetch can still fail against a mirror that is configured.

Per-file failures always stream to stderr, and --verbose adds a line per
recovered file. A run in which every object recovers exits 0; when any
object fails, the failures are counted and reported and the command exits 1,
and a dry run whose plan already holds an unrecoverable object exits the
same way. An interrupted run reports its partial totals to stderr and exits
0; a fetch cut off mid-object leaves no partial file behind.

` + inspectLong + `

` + remoteLong + `

The optional argument narrows the extract to one archive path's subtree; with
none the whole archive is extracted. With --dry-run the plan, including how
much it would fetch from the mirror, is summarized from the listing and
nothing is written into the target; extract.path is not required. Sizing the
plan can itself fetch a workspace's absent sealed indexes (roll-ups and
sidecars) from the mirror, the one egress a dry run may cost.`,
		Args: cobra.MaximumNArgs(1),
	}

	cfgFlag := registerConfigFlag(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		cfg, err := loadCmdConfig(*cfgFlag)
		if err != nil {
			return err
		}

		var prefix string

		if len(args) == 1 {
			prefix = args[0]
		}

		// The target comes from the file alone, so a real run with none is
		// refused before the archive opens (an open can cost mirror I/O);
		// only a dry run proceeds without one.
		target := configDir(cfg.path, cfg.file.Extract.Path)
		if !dryRun && target == "" {
			return fmt.Errorf("%w (set extract.path in the configuration file)", view.ErrNoTarget)
		}

		arc, err := cfg.open(ctx, listNotice(cc.ErrOrStderr()))
		if err != nil {
			return err
		}

		// A dry run's job is to predict the real run, so a target the real
		// run refuses fails the dry run identically; only the target itself
		// stays optional there.
		if target != "" {
			err = checkTargetOutside(cfg.archiveDir, target, orgNames(arc))
			if err != nil {
				return err
			}
		}

		if dryRun {
			return hintDirArg(
				extractDryRun(cc.OutOrStdout(), cc.ErrOrStderr(), arc, prefix, target, jsonOut), args)
		}

		return hintDirArg(runExtractCmd(ctx, cc, arc, prefix, target, verbose, jsonOut), args)
	}

	flags := cmd.Flags()
	flags.BoolVar(&dryRun, flagDryRun, false, "summarize what would be extracted without writing")
	flags.BoolVarP(&verbose, flagVerbose, "v", false, "stream one line per recovered file to stderr")
	flags.BoolVar(&jsonOut, flagJSON, false, "emit the summary as JSON")

	return cmd
}

// runExtractCmd drives one extract run and reports its summary. Per-file
// failures always stream to stderr; verbose adds a line per recovered file.
// An interrupt reports the partial totals to stderr and exits cleanly,
// matching the archive command's signal semantics.
func runExtractCmd(ctx context.Context, cc *cobra.Command, arc *view.Archive,
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

	sum, err := arc.Extract(ctx, target, prefix, progress)

	warnDegraded(errW, arc)

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			eprintf(errW, "extract interrupted: %s (%s) written into %s\n",
				theme.CountNoun(sum.Files, "object", "objects"), theme.HumanBytes(sum.Bytes), target)

			return nil
		}

		return describeNoOrg(err, arc)
	}

	err = writeExtractSummary(cc.OutOrStdout(), extractOutcome{summary: sum, target: target}, jsonOut)
	if err != nil {
		return err
	}

	if sum.Errored > 0 {
		return fmt.Errorf("%w: %d of %d objects", ErrExtractIncomplete, sum.Errored, sum.Errored+sum.Files)
	}

	return nil
}

// extractDryRun summarizes an extract from the listing without writing: the
// listing carries the same entries in the same order as the plan the real run
// executes, so the totals predict it.
//
// An evicted object in an organization whose root records no mirror counts as
// errored rather than as a file to write, because nothing can fetch its bytes
// back, and when the plan holds any the dry run exits 1 the way the run would.
// Each is named on stderr, where the real run streams its per-file failures, so
// the answer to "which ones" does not need a second command.
//
// Everything else evicted is counted as recoverable and reported separately as
// the volume the run will pull down, since a run with no opt-out flag fetches
// it without asking and this is the only place an operator sees that cost
// before paying it.
//
// The prediction is a lower bound on the failures: a bundle that turns out to
// be corrupt, a mirror that refuses the credentials it is offered, or an object
// missing from the bucket all error only when something tries to read them,
// which sizing the plan does not, so a clean dry run does not promise a clean
// run. The one egress a dry run may cost is the listing's own machinery: a
// workspace whose sealed indexes (roll-ups, sidecars) are not on disk has them
// fetched from the mirror so the plan covers its sealed objects at all.
func extractDryRun(out, errW io.Writer, arc *view.Archive, prefix, target string, jsonOut bool) error {
	entries, err := arc.List(prefix)

	// The warning precedes the error check for the same reason show's does: a
	// listing miss may be explained by the degraded mirror, and going quiet
	// about it would report a local-only plan as the whole archive.
	warnDegraded(errW, arc)

	if err != nil {
		return describeNoOrg(err, arc)
	}

	mirrored := make(map[string]bool, len(arc.Orgs()))
	for _, org := range arc.Orgs() {
		mirrored[org.Name] = org.HasRemote()
	}

	outcome := extractOutcome{target: target, dryRun: true}

	for _, e := range entries {
		if e.Offloaded && !mirrored[e.Org] {
			outcome.summary.Errored++

			eprintf(errW, "%s: cannot be extracted; its bytes are in the remote store "+
				"and the organization records no mirror to fetch them from\n", e.ArchivePath())

			continue
		}

		if e.Offloaded {
			outcome.remote.files++
			outcome.remote.bytes += e.Size
		}

		outcome.summary.Files++
		outcome.summary.Bytes += e.Size
	}

	err = writeExtractSummary(out, outcome, jsonOut)
	if err != nil {
		return err
	}

	if outcome.summary.Errored > 0 {
		return fmt.Errorf("%w: %d of %d objects",
			ErrExtractIncomplete, outcome.summary.Errored, outcome.summary.Errored+outcome.summary.Files)
	}

	return nil
}

// extractVolume counts the objects an extract plans to pull down from the
// mirror, an estimate of the egress a run costs.
//
// The byte figure is each object's archived length, which is exact for a
// tarball (the object is downloaded whole) and an overestimate for a member of
// an evicted bundle, whose compressed span in the remote zip is what actually
// crosses the wire. Only the bundle's central directory knows that span, and
// reading it is a network round trip per bundle, which sizing the plan
// deliberately skips.
type extractVolume struct {
	bytes int64
	files int
}

// extractOutcome is one extract's reported result: a finished run's totals, or a
// dry run's prediction of them.
//
// The remote volume is a dry run's alone. A finished run has already spent
// whatever egress it spent, so reporting it there would only add a field that
// is always zero to every summary the real command writes.
type extractOutcome struct {
	target  string
	summary view.ExtractSummary
	remote  extractVolume
	dryRun  bool
}

// extractReport is the wire shape of an extract summary under --json.
type extractReport struct {
	Target      string `json:"target"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	Errored     int    `json:"errored"`
	RemoteFiles int    `json:"remoteFiles,omitempty"`
	RemoteBytes int64  `json:"remoteBytes,omitempty"`
	DryRun      bool   `json:"dryRun"`
}

// writeExtractSummary reports one run's totals to out, as one human line or one
// JSON object.
func writeExtractSummary(out io.Writer, oc extractOutcome, jsonOut bool) error {
	if jsonOut {
		err := json.NewEncoder(out).Encode(extractReport{
			Target:      oc.target,
			Files:       oc.summary.Files,
			Bytes:       oc.summary.Bytes,
			Errored:     oc.summary.Errored,
			RemoteFiles: oc.remote.files,
			RemoteBytes: oc.remote.bytes,
			DryRun:      oc.dryRun,
		})
		if err != nil {
			return fmt.Errorf("encode summary: %w", err)
		}

		return nil
	}

	var b strings.Builder

	if oc.dryRun {
		b.WriteString("would extract ")
	} else {
		b.WriteString("extracted ")
	}

	fmt.Fprintf(&b, "%s (%s)",
		theme.CountNoun(oc.summary.Files, "object", "objects"), theme.HumanBytes(oc.summary.Bytes))

	if oc.target != "" {
		b.WriteString(" into " + oc.target)
	}

	if oc.remote.files > 0 {
		fmt.Fprintf(&b, "; %s (up to %s) to fetch from the remote store",
			theme.CountNoun(oc.remote.files, "object", "objects"), theme.HumanBytes(oc.remote.bytes))
	}

	if oc.summary.Errored > 0 {
		// A dry run has not tried anything yet, so nothing has errored; what it
		// counted is what it can already tell will not come back.
		if oc.dryRun {
			fmt.Fprintf(&b, "; %d not recoverable", oc.summary.Errored)
		} else {
			fmt.Fprintf(&b, "; %d errored", oc.summary.Errored)
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

// configDir resolves a directory named by the configuration file against the
// file's own location; an absolute or empty path is returned as-is.
func configDir(cfgPath, dir string) string {
	if dir == "" || filepath.IsAbs(dir) {
		return dir
	}

	return filepath.Join(filepath.Dir(cfgPath), dir)
}

// openArchive opens the archive at dir under ctx, against the supplied mirror
// when rcfg is non-nil, and wraps its organizations in a [*view.Archive]. A
// non-nil notify reports an organization whose mirror listing is running long.
//
//nolint:contextcheck // The context rides in through view.WithContext; remote reads derive from it.
func openArchive(
	ctx context.Context, dir string, rcfg *remote.Config, notify func(org string),
) (*view.Archive, error) {
	opts := []view.ArchiveOption{view.WithContext(ctx)}
	if rcfg != nil {
		opts = append(opts, view.WithRemote(*rcfg))
	}

	if notify != nil {
		opts = append(opts, view.WithListNotice(notify))
	}

	orgs, err := view.OpenArchive(dir, opts...)
	if err != nil {
		return nil, err
	}

	return view.NewArchive(orgs), nil
}

// hintDirArg augments an unknown-organization or invalid-path failure whose
// lone positional names an existing directory: the likely mistake is
// addressing the archive by directory rather than by the org-prefixed archive
// path the argument takes.
func hintDirArg(err error, args []string) error {
	if (!errors.Is(err, view.ErrNoOrg) && !errors.Is(err, view.ErrInvalidPath)) || len(args) != 1 {
		return err
	}

	info, statErr := os.Stat(args[0])
	if statErr != nil || !info.IsDir() {
		return err
	}

	return fmt.Errorf("%w\nthe archive directory comes from the configuration file's archive.path; "+
		"a positional argument addresses an org-prefixed archive path (\"<org>/<path>\") inside it",
		err)
}

// describeNoOrg augments an unknown-organization failure with the archive's
// known organization names.
func describeNoOrg(err error, arc *view.Archive) error {
	if !errors.Is(err, view.ErrNoOrg) {
		return err
	}

	return fmt.Errorf("%w (known organizations: %s)", err, strings.Join(orgNames(arc), ", "))
}

// checkTargetOutside refuses a target whose writes could land inside the
// archive directory. The target itself must sit outside the archive
// (segment-wise over absolute paths, so a sibling like "archive-backup"
// beside "archive" is not flagged), and each organization's destination
// under the target must not overlap the archive either: recovery reproduces
// "<org>/<path>" under the target, so a target that is an ancestor of the
// archive writes straight back into it when an organization's directory
// joins onto the archive dir (a single-organization archive extracted into
// its parent). Symlinks are not resolved: a symlinked target can dodge the
// check, which is accepted rather than pulling physical identity resolution
// into a CLI guard.
func checkTargetOutside(archiveDir, target string, orgs []string) error {
	absArchive, err := filepath.Abs(archiveDir)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", archiveDir, err)
	}

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target, err)
	}

	if pathkit.Contains(absArchive, absTarget) {
		return fmt.Errorf("%w: %s is inside %s", ErrTargetInArchive, target, archiveDir)
	}

	for _, org := range orgs {
		if pathkit.Overlaps(filepath.Join(absTarget, org), absArchive) {
			return fmt.Errorf("%w: writing organization %q under %s reaches %s",
				ErrTargetInArchive, org, target, archiveDir)
		}
	}

	return nil
}

// orgNames returns the archive's organization names in listing order.
func orgNames(arc *view.Archive) []string {
	names := make([]string, 0, len(arc.Orgs()))
	for _, org := range arc.Orgs() {
		names = append(names, org.Name)
	}

	return names
}

// eprintf writes best-effort progress to w; a stderr write fault mid-run has
// no recovery path.
func eprintf(w io.Writer, format string, args ...any) {
	//nolint:errcheck // Best-effort progress output.
	_, _ = fmt.Fprintf(w, format, args...)
}
