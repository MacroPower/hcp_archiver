package main

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
	"time"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/archiver"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/theme"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// Flag names shared by the inspect commands.
const (
	flagJSON         = "json"
	flagTarget       = "target"
	flagDryRun       = "dry-run"
	flagVerbose      = "verbose"
	flagRemote       = "remote"
	flagRemotePrefix = "remote-prefix"
)

var (
	// ErrUnsealIncomplete indicates an unseal run finished but some objects
	// could not be read or written.
	ErrUnsealIncomplete = errors.New("some objects could not be unsealed")

	// ErrTargetInArchive indicates an unseal target sits inside the archive
	// directory being read, where recovered files would be written into the
	// archive itself.
	ErrTargetInArchive = errors.New("unseal target must be outside the archive directory")

	// ErrPrefixNeedsRemote indicates --remote-prefix was given with nothing to
	// apply it to: no --remote URL and no configuration file naming a remote.
	ErrPrefixNeedsRemote = errors.New("--remote-prefix needs --remote or a configuration file naming a remote")
)

// inspectLong is the addressing contract the three read commands share.
const inspectLong = `Objects are addressed by org-prefixed archive paths ("<org>/<path>"), the
same layout an unseal reproduces beneath its target, whether the archive
directory is the archive root or a single organization's directory.

Positional arguments bind left to right: with two arguments the first is the
archive directory and the second the archive path.`

// remoteLong is the mirror read-through contract the browse and inspect
// commands share.
const remoteLong = `The archive's object-store mirror can stand in for absent local files: name
it with --remote (and --remote-prefix), or with --config naming a
configuration file whose remote section records it. Anything read that is not
on disk is fetched from the mirror and persisted at its local archive path,
so later reads need no network; a directory holding nothing at all is
bootstrapped from the mirror outright, and the recorded marker means the
flags are only needed once. A supplied remote that disagrees with the mirror
an organization's marker records is refused. Object-store credentials come
from the backend provider's default chain.`

// remoteFlags holds the raw mirror-location flag values bound onto the browse
// and inspect commands. Its resolve method turns them into the remote
// configuration [view.OpenArchive] accepts, or nil when none is named.
type remoteFlags struct {
	configPath string
	url        string
	prefix     string
}

// registerRemoteFlags binds the mirror-location flags onto cmd and returns
// the [*remoteFlags] they write into.
func registerRemoteFlags(cmd *cobra.Command) *remoteFlags {
	rf := &remoteFlags{}
	fs := cmd.Flags()

	fs.StringVarP(&rf.configPath, flagConfig, "c", "",
		fmt.Sprintf("path to the YAML configuration file whose remote section names the mirror (defaults to $%s)",
			config.EnvConfigPath))
	fs.StringVar(&rf.url, flagRemote, "",
		"mirror bucket URL in gocloud.dev form (s3://, azblob://, file://); overrides the configuration file")
	fs.StringVar(&rf.prefix, flagRemotePrefix, "",
		"key prefix the archive is mirrored under")

	return rf
}

// resolve turns the flag values into the remote configuration the archive
// opens with: an explicit --remote wins, then the configuration file's remote
// section (the --config flag or the environment), and nil means no remote was
// named anywhere, leaving the archive to its local tree and markers.
func (rf *remoteFlags) resolve() (*remote.Config, error) {
	if rf.url != "" {
		return &remote.Config{URL: rf.url, Prefix: rf.prefix}, nil
	}

	cfgPath := rf.configPath
	if cfgPath == "" {
		cfgPath = os.Getenv(config.EnvConfigPath)
	}

	if cfgPath == "" {
		if rf.prefix != "" {
			return nil, ErrPrefixNeedsRemote
		}

		return nil, nil //nolint:nilnil // No remote named anywhere is the local-only default.
	}

	file, err := config.LoadFile(cfgPath)
	if err != nil {
		return nil, err //nolint:wrapcheck // Source-annotated configuration errors render as-is.
	}

	if file.Remote.IsZero() {
		if rf.prefix != "" {
			return nil, ErrPrefixNeedsRemote
		}

		return nil, nil //nolint:nilnil // The file names no remote; same local-only default.
	}

	rc := file.Remote.RemoteConfig()
	cfg := archiver.RemoteConfig(&rc)

	if rf.prefix != "" {
		cfg.Prefix = rf.prefix
	}

	return &cfg, nil
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

` + remoteLong + `

A single argument names the archive directory; pass "." explicitly to address
a path in the current directory.`,
		Args: cobra.MaximumNArgs(2),
	}

	rf := registerRemoteFlags(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		dir, prefix := archiveArgs(args)

		ctx, stop := signalContext(cc.Context())
		defer stop()

		rcfg, err := rf.resolve()
		if err != nil {
			return err
		}

		arc, err := openArchive(ctx, dir, rcfg)
		if err != nil {
			return hintLoneArg(err, "list", args)
		}

		entries, err := arc.List(prefix)
		if err != nil {
			return describeNoOrg(err, arc)
		}

		warnDegraded(cc.ErrOrStderr(), arc)

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
		Use:   "show [archive-dir] <path>",
		Short: "Print one archived object's bytes to stdout",
		Long: `Print the exact bytes of one archived object to stdout, whichever physical
form (loose, roll-up, bundle) holds it.

` + inspectLong + `

` + remoteLong + `

A single argument is the archive path, read from the archive in the current
directory.`,
		Args: cobra.RangeArgs(1, 2),
	}

	rf := registerRemoteFlags(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		dir, archivePath := showArgs(args)

		ctx, stop := signalContext(cc.Context())
		defer stop()

		rcfg, err := rf.resolve()
		if err != nil {
			return err
		}

		arc, err := openArchive(ctx, dir, rcfg)
		if err != nil {
			return err
		}

		data, err := arc.Read(archivePath)

		// Whatever the read answered, a degraded mirror is context the reader
		// needs: a miss may be explained by it, and a hit does not disprove it.
		warnDegraded(cc.ErrOrStderr(), arc)

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
	}

	return cmd
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

An object whose bytes were evicted to the archive's mirror is fetched back
from it, needing object-store credentials from the backend provider's default
chain. An organization whose archive root records no mirror leaves one
unrecoverable before the run starts, counted and named among the run's
failures; a fetch can still fail against a mirror that is configured.

` + inspectLong + `

` + remoteLong + `

A single argument names the archive directory; pass "." explicitly to address
a path in the current directory. With --dry-run the plan, including how much it
would fetch from the mirror, is summarized from the listing and nothing is
written into the target; --target is not required. Sizing the plan can itself
fetch a workspace's absent sealed indexes (roll-ups and sidecars) from the
mirror, the one egress a dry run may cost.`,
		Args: cobra.MaximumNArgs(2),
	}

	rf := registerRemoteFlags(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		dir, prefix := archiveArgs(args)

		ctx, stop := signalContext(cc.Context())
		defer stop()

		rcfg, err := rf.resolve()
		if err != nil {
			return err
		}

		arc, err := openArchive(ctx, dir, rcfg)
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

	warnDegraded(errW, arc)

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			eprintf(errW, "unseal interrupted: %s (%s) written into %s\n",
				theme.CountNoun(sum.Files, "object", "objects"), theme.HumanBytes(sum.Bytes), target)

			return nil
		}

		return describeNoOrg(err, arc)
	}

	err = writeUnsealSummary(cc.OutOrStdout(), unsealOutcome{summary: sum, target: target}, jsonOut)
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
func unsealDryRun(out, errW io.Writer, arc *view.Archive, prefix, target string, jsonOut bool) error {
	entries, err := arc.List(prefix)
	if err != nil {
		return describeNoOrg(err, arc)
	}

	warnDegraded(errW, arc)

	mirrored := make(map[string]bool, len(arc.Orgs()))
	for _, org := range arc.Orgs() {
		mirrored[org.Name] = org.HasRemote()
	}

	outcome := unsealOutcome{target: target, dryRun: true}

	for _, e := range entries {
		if e.Offloaded && !mirrored[e.Org] {
			outcome.summary.Errored++

			eprintf(errW, "%s: cannot be unsealed; its bytes are in the remote store "+
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

	err = writeUnsealSummary(out, outcome, jsonOut)
	if err != nil {
		return err
	}

	if outcome.summary.Errored > 0 {
		return fmt.Errorf("%w: %d of %d objects",
			ErrUnsealIncomplete, outcome.summary.Errored, outcome.summary.Errored+outcome.summary.Files)
	}

	return nil
}

// unsealVolume counts the objects an unseal plans to pull down from the
// mirror, an estimate of the egress a run costs.
//
// The byte figure is each object's archived length, which is exact for a
// tarball (the object is downloaded whole) and an overestimate for a member of
// an evicted bundle, whose compressed span in the remote zip is what actually
// crosses the wire. Only the bundle's central directory knows that span, and
// reading it is a network round trip per bundle, which sizing the plan
// deliberately skips.
type unsealVolume struct {
	bytes int64
	files int
}

// unsealOutcome is one unseal's reported result: a finished run's totals, or a
// dry run's prediction of them.
//
// The remote volume is a dry run's alone. A finished run has already spent
// whatever egress it spent, so reporting it there would only add a field that
// is always zero to every summary the real command writes.
type unsealOutcome struct {
	target  string
	summary view.UnsealSummary
	remote  unsealVolume
	dryRun  bool
}

// unsealReport is the wire shape of an unseal summary under --json.
type unsealReport struct {
	Target      string `json:"target"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	Errored     int    `json:"errored"`
	RemoteFiles int    `json:"remoteFiles,omitempty"`
	RemoteBytes int64  `json:"remoteBytes,omitempty"`
	DryRun      bool   `json:"dryRun"`
}

// writeUnsealSummary reports one run's totals to out, as one human line or one
// JSON object.
func writeUnsealSummary(out io.Writer, oc unsealOutcome, jsonOut bool) error {
	if jsonOut {
		err := json.NewEncoder(out).Encode(unsealReport{
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
		b.WriteString("would unseal ")
	} else {
		b.WriteString("unsealed ")
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

// openArchive opens the archive at dir under ctx, against the supplied mirror
// when rcfg is non-nil, and wraps its organizations in a [*view.Archive].
//
//nolint:contextcheck // The context rides in through view.WithContext; remote reads derive from it.
func openArchive(ctx context.Context, dir string, rcfg *remote.Config) (*view.Archive, error) {
	opts := []view.ArchiveOption{view.WithContext(ctx)}
	if rcfg != nil {
		opts = append(opts, view.WithRemote(*rcfg))
	}

	orgs, err := view.OpenArchive(dir, opts...)
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
