package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/archiver"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/restore"
	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
)

var (
	// ErrPullIncomplete indicates a pull run finished with the restored set
	// not fully on disk: files failed, conflicts were refused, or the run was
	// interrupted.
	ErrPullIncomplete = errors.New("some files could not be restored")

	// ErrNoPullRemote indicates no mirror is reachable for an organization:
	// the configuration file names no remote and the organization's archive
	// root records no marker to fall back on.
	ErrNoPullRemote = errors.New("no mirror to restore from")

	// ErrNoPullOrgs indicates nothing named an organization to restore: no
	// argument, no configuration filter, no mirror to enumerate, and no local
	// directory carrying a marker.
	ErrNoPullOrgs = errors.New("no organizations to restore")
)

// pullListNoticeDelay is how long an organization's mirror listing runs
// before the command says so on stderr; a pull prints nothing else until the
// plan is sized.
const pullListNoticeDelay = 3 * time.Second

// newPullCmd returns a command that restores organizations' warm layers from
// their mirrors.
func newPullCmd() *cobra.Command {
	var (
		dryRun  bool
		verbose bool
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "pull [org]...",
		Short: "Restore a local archive from its mirror",
		Long: `Restore each organization's local archive from its object-store mirror: the
search layer (roll-up files and bundle sidecars), the loose metadata, the
project and workspace settings, the history sidecars, the identity records,
and the ledger snapshots. The mirror is the only source; nothing contacts
the HCP Terraform API. The evicted surfaces (sealed-bundle zips and
configuration-version tarballs) stay in the mirror, where eviction put them,
and the ledger's replay log is never restored: replayed over a newer
snapshot it would resurrect ledger state a prior run discarded.

Every restored file is verified against the digest the mirror records before
it counts, and lands by atomic rename, so an interrupted run leaves no
partial file. Data files land before the ledger snapshots that describe
them, and while the restore is incomplete the archive carries a marker that
makes every archive run refuse the tree and every sweep refuse to prune the
mirror; builds that predate this command refuse the marker outright by its
version. Re-running completes an interrupted restore, downloading only what
is missing, and a re-run against a restored archive changes nothing and says
so.

A local file that differs from its mirrored copy is replaced (a roll-up
above all: the mirrored copy already holds every line, so replacing, never
appending, is what avoids duplicates). A ledger snapshot that differs is
ordered by its run metadata where it carries any; a conflict the metadata
cannot order is refused and named, never guessed at. A local replay log
standing where snapshots must land refuses the run before anything is
written; one clean archiver run folds the log away.

The mirror comes from the configuration file's remote section, or, absent
one, from the marker each organization's archive root records. A configured
remote that disagrees with a recorded marker is refused. Organizations come
from the arguments, then the configuration file's organization filter, then
the mirror's own listing, then the local directories carrying markers. Each
organization restores into "<archive.path>/<org>".

After the restore the command backfills the local stubs for evicted
configuration-version tarballs from the mirror's own metadata, then settles
the marker: complete when every mirrored key is accounted for locally, so
the archive browses and extracts with no further mirror listing; partial
when anything is not (a stub that could not be ensured, a mirrored key
nothing local accounts for), with each blocking key named on stderr. A
marker already recording complete is never demoted. Stub faults and
unaccounted keys never fail the restore; the exit code reflects only the
restored set.

Per-file failures always stream to stderr, and --verbose adds a line per
restored file. A run in which the whole set lands exits 0; any failure,
refused conflict, or interrupt leaves the in-progress marker in place, names
what is missing, and exits 1. With --dry-run the command reports what it
would restore, the bytes it would transfer, and the conflicts it would hit,
and writes nothing; sizing the plan still costs mirror metadata reads.`,
		Args: cobra.ArbitraryArgs,
	}

	cfgFlag := registerConfigFlag(cmd)

	cmd.RunE = func(cc *cobra.Command, args []string) error {
		ctx, stop := signalContext(cc.Context())
		defer stop()

		cfg, err := loadCmdConfig(*cfgFlag)
		if err != nil {
			return err
		}

		return runPull(ctx, cc, cfg, args, dryRun, verbose, jsonOut)
	}

	flags := cmd.Flags()
	flags.BoolVar(&dryRun, flagDryRun, false, "report what would be restored without writing")
	flags.BoolVarP(&verbose, flagVerbose, "v", false, "stream one line per restored file to stderr")
	flags.BoolVar(&jsonOut, flagJSON, false, "emit each organization's summary as JSON")

	return cmd
}

// runPull resolves the organizations to restore and pulls each in turn. A
// per-organization fault (a held lock, a mismatched marker, a refused
// conflict) is named on stderr and the remaining organizations still
// restore; any incompleteness exits through [ErrPullIncomplete].
func runPull(
	ctx context.Context, cc *cobra.Command, cfg cmdConfig, args []string,
	dryRun, verbose, jsonOut bool,
) error {
	errW := cc.ErrOrStderr()
	fileRemote := remoteFromFile(cfg.file)

	orgs, err := resolvePullOrgs(ctx, cfg, fileRemote, args, errW)
	if err != nil {
		return err
	}

	incomplete := false

	for _, org := range orgs {
		err := pullOrg(ctx, cc, cfg, fileRemote, org, dryRun, verbose, jsonOut)

		switch {
		case err == nil:
		case errors.Is(err, ErrPullIncomplete):
			incomplete = true
		default:
			// A precondition fault stops this organization alone; the others
			// still restore, and the named fault carries into the exit code.
			eprintf(errW, "organization %q: %v\n", org, err)

			incomplete = true
		}
	}

	if ctx.Err() != nil {
		eprintf(errW, "pull interrupted; the in-progress marker stays until a re-run finishes the restore\n")

		incomplete = true
	}

	if incomplete {
		return ErrPullIncomplete
	}

	return nil
}

// resolvePullOrgs names the organizations to restore: the arguments, then
// the configuration file's organization filter, then the mirror's top-level
// listing, then the local directories carrying markers. Every name passes
// [config.ValidateOrganizationName], the boundary that keeps a name from
// addressing a path outside its own root.
func resolvePullOrgs(
	ctx context.Context, cfg cmdConfig, fileRemote *remote.Config, args []string, errW io.Writer,
) ([]string, error) {
	orgs := args

	if len(orgs) == 0 {
		orgs = cfg.file.Organizations
	}

	if len(orgs) == 0 && fileRemote != nil {
		discovered, err := discoverMirrorOrgs(ctx, *fileRemote)
		if err != nil {
			return nil, err
		}

		orgs = discovered
	}

	if len(orgs) == 0 {
		local, err := discoverLocalOrgs(cfg.archiveDir, errW)
		if err != nil {
			return nil, err
		}

		orgs = local
	}

	if len(orgs) == 0 {
		return nil, fmt.Errorf("%w: name organizations as arguments, set organizations or a remote "+
			"in the configuration file, or run against an archive whose directories record markers",
			ErrNoPullOrgs)
	}

	for _, org := range orgs {
		err := config.ValidateOrganizationName(org)
		if err != nil {
			return nil, err //nolint:wrapcheck // The validator names the organization and the rule.
		}
	}

	return orgs, nil
}

// discoverMirrorOrgs lists the mirror's top-level child prefixes, the
// organizations it holds.
func discoverMirrorOrgs(ctx context.Context, rcfg remote.Config) ([]string, error) {
	client, err := remote.New(ctx, rcfg)
	if err != nil {
		return nil, fmt.Errorf("open mirror: %w", err)
	}

	defer func() {
		//nolint:errcheck // Read-only listing client.
		_ = client.Close()
	}()

	prefix := rcfg.Key("", "")
	if prefix != "" {
		prefix += "/"
	}

	orgs, err := client.Children(ctx, prefix)
	if err != nil {
		return nil, err //nolint:wrapcheck // The client names the listing and the fault.
	}

	return orgs, nil
}

// discoverLocalOrgs lists the archive directory's immediate subdirectories
// that record a remote marker, the organizations restorable with no
// configured remote. A directory whose marker cannot be read is skipped with
// a note on errW rather than silently, so an operator can tell a skip from
// an absence; naming that organization explicitly still surfaces the fault
// in full.
func discoverLocalOrgs(archiveDir string, errW io.Writer) ([]string, error) {
	dirents, err := os.ReadDir(archiveDir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read archive directory %q: %w", archiveDir, err)
	}

	var orgs []string

	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}

		_, ok, merr := remote.ReadMarker(filepath.Join(archiveDir, d.Name()))

		switch {
		case merr != nil:
			eprintf(errW, "warning: organization %q: omitted from discovery; its marker cannot be read (%v)\n",
				d.Name(), merr)

		case ok:
			orgs = append(orgs, d.Name())
		}
	}

	return orgs, nil
}

// pullOrg restores one organization: it settles the preconditions (marker
// agreement, the archive lock), plans against the mirror, and either reports
// the plan (--dry-run) or executes it.
func pullOrg(
	ctx context.Context, cc *cobra.Command, cfg cmdConfig, fileRemote *remote.Config,
	org string, dryRun, verbose, jsonOut bool,
) error {
	errW := cc.ErrOrStderr()
	orgRoot := filepath.Join(cfg.archiveDir, org)

	rcfg, err := resolveOrgRemote(orgRoot, fileRemote)
	if err != nil {
		return err
	}

	unlock, err := lockForPull(orgRoot, dryRun)
	if err != nil {
		return err
	}
	defer unlock()

	client, err := remote.New(ctx, rcfg)
	if err != nil {
		return fmt.Errorf("open mirror: %w", err)
	}

	defer func() {
		//nolint:errcheck // The transfers' own results are what matter.
		_ = client.Close()
	}()

	restorer := restore.NewRestorer(client, rcfg,
		restore.WithLogger(cmdLogger(ctx)),
		restore.WithProgress(func(relPath string, bytes int64, perr error) {
			switch {
			case perr != nil:
				eprintf(errW, "%s/%s: %v\n", org, relPath, perr)
			case verbose:
				eprintf(errW, "%s/%s (%s)\n", org, relPath, theme.HumanBytes(bytes))
			}
		}),
	)

	// The plan sizes itself from the mirror's listing, which on a mirror of
	// millions of objects runs minutes; say so rather than sit silent.
	notice := time.AfterFunc(pullListNoticeDelay, func() { listNotice(errW)(org) })

	plan, err := restorer.Plan(ctx, orgRoot, org)

	notice.Stop()

	if err != nil {
		return err
	}

	for _, e := range plan.Refusals {
		eprintf(errW, "%s/%s: refused: %s\n", org, e.Rel, e.Reason)
	}

	if dryRun {
		// The prediction assumes the stub phase succeeds; a marker already
		// recording complete is never demoted, so it predicts complete over
		// any leftovers.
		predicted := markerComplete(orgRoot) ||
			(len(plan.Refusals) == 0 && len(plan.Leftovers) == 0)

		// A refused run would settle nothing, so its leftovers carry no
		// marker consequence to claim.
		printLeftovers(errW, org, plan.Leftovers, len(plan.Refusals) == 0 && !predicted)

		return writePullSummary(cc.OutOrStdout(), pullOutcome{
			org:       org,
			target:    orgRoot,
			dryRun:    true,
			restored:  plan.RestoreFiles,
			skipped:   plan.Skipped,
			refused:   len(plan.Refusals),
			bytes:     plan.RestoreBytes,
			stubs:     len(plan.Stubs),
			complete:  predicted,
			leftovers: plan.Leftovers,
		}, jsonOut)
	}

	sum, err := restorer.Pull(ctx, orgRoot, org, plan)
	if err != nil {
		return err
	}

	markerClause, markerPartial := markerState(orgRoot, len(plan.Leftovers))

	printLeftovers(errW, org, plan.Leftovers, markerPartial)

	return writePullSummary(cc.OutOrStdout(), pullOutcome{
		org:         org,
		target:      orgRoot,
		restored:    sum.Restored,
		skipped:     sum.Skipped,
		failed:      sum.Failed,
		refused:     sum.Refused,
		bytes:       sum.Bytes,
		stubs:       sum.Stubs,
		stubsFailed: sum.StubsFailed,
		complete:    sum.Complete,
		leftovers:   plan.Leftovers,
		marker:      markerClause,
	}, jsonOut)
}

// markerComplete reports whether the org root's marker already records the
// tree as complete, the state a settlement never demotes.
func markerComplete(orgRoot string) bool {
	marker, ok, err := remote.ReadMarker(orgRoot)

	return err == nil && ok && !marker.Restoring && !marker.Partial && marker.URL != ""
}

// printLeftovers names each mirrored key nothing in the restored tree
// accounts for. The "(keeps the marker partial)" suffix appears only when
// the settled or predicted marker is in fact partial: under a marker
// already recording complete, leftovers do not flip it.
func printLeftovers(errW io.Writer, org string, leftovers []string, partial bool) {
	suffix := ""
	if partial {
		suffix = " (keeps the marker partial)"
	}

	for _, key := range leftovers {
		eprintf(errW, "%s: unaccounted mirror key: %s%s\n", org, key, suffix)
	}
}

// resolveOrgRemote settles which mirror one organization restores from: the
// configured remote when the file names one, else the marker its root
// records. A recorded marker that names a different mirror than the
// configured one refuses, the same stance the archiver takes: evicted
// surfaces live only at the recorded location, so a re-point must be an
// explicit migration.
func resolveOrgRemote(orgRoot string, fileRemote *remote.Config) (remote.Config, error) {
	marker, hasMarker, err := remote.ReadMarker(orgRoot)
	if err != nil {
		return remote.Config{}, err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	if fileRemote == nil {
		if !hasMarker || marker.URL == "" {
			return remote.Config{}, fmt.Errorf("%w: the configuration names no remote and %s records none",
				ErrNoPullRemote, remote.MarkerName)
		}

		return marker.Config(), nil
	}

	if hasMarker && marker.Conflicts(fileRemote.Marker()) {
		return remote.Config{}, fmt.Errorf(
			"%w: the archive records its mirror at %q prefix %q, but the configuration names %q prefix %q",
			archiver.ErrRemoteRelocated, marker.URL, marker.Prefix, fileRemote.URL, fileRemote.Prefix)
	}

	return *fileRemote, nil
}

// lockForPull takes the organization's archive lock, refusing when another
// process holds it, so a restore and an archive run can never interleave. A
// dry run predicts the same refusal but takes the lock only when the lock
// file already exists: acquiring it would create the .ledger directory, and
// a dry run writes nothing; a root with no lock file trivially has no
// holder.
func lockForPull(orgRoot string, dryRun bool) (func(), error) {
	if dryRun {
		_, err := os.Stat(filepath.Join(orgRoot, manifest.LedgerDirName, manifest.LockFileName))

		switch {
		case errors.Is(err, fs.ErrNotExist):
			return func() {}, nil
		case err != nil:
			return nil, fmt.Errorf("stat archive lock: %w", err)
		}
	}

	lock, err := manifest.LockArchive(orgRoot)
	if err != nil {
		return nil, err //nolint:wrapcheck // The lock names the file and the holder semantics.
	}

	return func() {
		//nolint:errcheck // The kernel releases the flock regardless.
		_ = lock.Close()
	}, nil
}

// pullOutcome is one organization's reported result: a finished restore's
// totals, or a dry run's prediction of them.
type pullOutcome struct {
	org         string
	target      string
	marker      string
	leftovers   []string
	restored    int
	skipped     int
	failed      int
	refused     int
	stubs       int
	stubsFailed int
	bytes       int64
	dryRun      bool
	complete    bool
}

// pullReport is the wire shape of a pull summary under --json.
type pullReport struct {
	Org         string   `json:"org"`
	Target      string   `json:"target"`
	Leftovers   []string `json:"leftovers,omitempty"`
	Restored    int      `json:"restored"`
	Skipped     int      `json:"skipped"`
	Failed      int      `json:"failed"`
	Refused     int      `json:"refused"`
	Stubs       int      `json:"stubs"`
	StubsFailed int      `json:"stubsFailed,omitempty"`
	Bytes       int64    `json:"bytes"`
	Complete    bool     `json:"complete"`
	DryRun      bool     `json:"dryRun,omitempty"`
}

// writePullSummary reports one organization's outcome on out, as one human
// line or one JSON object.
func writePullSummary(out io.Writer, o pullOutcome, jsonOut bool) error {
	if jsonOut {
		err := json.NewEncoder(out).Encode(pullReport{
			Org:         o.org,
			Target:      o.target,
			Restored:    o.restored,
			Skipped:     o.skipped,
			Failed:      o.failed,
			Refused:     o.refused,
			Stubs:       o.stubs,
			StubsFailed: o.stubsFailed,
			Bytes:       o.bytes,
			Complete:    o.complete,
			Leftovers:   o.leftovers,
			DryRun:      o.dryRun,
		})
		if err != nil {
			return fmt.Errorf("write pull summary: %w", err)
		}

		return pullExitErr(o)
	}

	var line string

	switch {
	case o.dryRun:
		line = fmt.Sprintf("%s: pull would restore %s (%s) into %s; %d already present; %d conflicts; "+
			"would ensure %s; would settle the %s",
			o.org, theme.CountNoun(o.restored, "object", "objects"), theme.HumanBytes(o.bytes),
			o.target, o.skipped, o.refused,
			theme.CountNoun(o.stubs, "stub", "stubs"), o.markerClause())

	case o.restored == 0 && o.failed == 0 && o.refused == 0:
		line = fmt.Sprintf("%s: nothing to restore (%s verified); %s",
			o.org, theme.CountNoun(o.skipped, "object", "objects"), o.markerClause())

	default:
		line = fmt.Sprintf("%s: restored %s (%s) into %s; %d skipped; %d failed; %d refused%s; %s",
			o.org, theme.CountNoun(o.restored, "object", "objects"), theme.HumanBytes(o.bytes),
			o.target, o.skipped, o.failed, o.refused, o.stubClause(), o.markerClause())
	}

	_, err := fmt.Fprintln(out, line)
	if err != nil {
		return fmt.Errorf("write pull summary: %w", err)
	}

	return pullExitErr(o)
}

// stubClause renders the stub tally for the human summary, empty when the
// run had no stub work at all.
func (o pullOutcome) stubClause() string {
	switch {
	case o.stubsFailed > 0:
		return fmt.Sprintf("; %d stubs (%d could not be ensured)", o.stubs+o.stubsFailed, o.stubsFailed)
	case o.stubs > 0:
		return fmt.Sprintf("; %d stubs", o.stubs)
	default:
		return ""
	}
}

// markerClause renders the marker state for the human summary: the
// prediction on a dry run, the file's own state on a real one, so a run
// that left the restoring marker standing says so instead of guessing.
func (o pullOutcome) markerClause() string {
	if o.marker != "" {
		return o.marker
	}

	if o.complete {
		return "marker complete"
	}

	if n := len(o.leftovers); n > 0 {
		return fmt.Sprintf("marker partial (%d unaccounted)", n)
	}

	return "marker partial"
}

// markerState reads the marker at orgRoot and renders its settled state for
// the summary line, also reporting whether it stands settled at partial,
// the one state a leftover's stderr line may claim to cause.
func markerState(orgRoot string, leftovers int) (string, bool) {
	marker, ok, err := remote.ReadMarker(orgRoot)

	switch {
	case err != nil:
		return "marker unreadable", false
	case !ok:
		return "no marker", false
	case marker.Restoring:
		return "marker restoring (restore incomplete)", false
	case marker.Partial && leftovers > 0:
		return fmt.Sprintf("marker partial (%d unaccounted)", leftovers), true
	case marker.Partial:
		return "marker partial", true
	default:
		return "marker complete", false
	}
}

// pullExitErr maps an outcome onto the exit contract: any failure or refused
// conflict, predicted or real, reports [ErrPullIncomplete].
func pullExitErr(o pullOutcome) error {
	if o.failed > 0 || o.refused > 0 {
		return fmt.Errorf("%w: %d failed, %d refused", ErrPullIncomplete, o.failed, o.refused)
	}

	return nil
}
