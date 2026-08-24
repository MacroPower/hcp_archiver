package restore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

var (
	// ErrOrgMismatch reports a target directory whose archived organization
	// record names a different organization than the one being restored, the
	// fingerprint of a tree moved under another organization's root.
	ErrOrgMismatch = errors.New("target directory belongs to a different organization")

	// ErrLocalReplayLog reports a local ledger replay log standing where the
	// restore would write snapshots. Restored snapshots replayed under a live
	// local log would splice two ledger histories; a clean archive run folds
	// the log away, or deleting the .ledger directory consents to losing it.
	ErrLocalReplayLog = errors.New("a local ledger replay log conflicts with restoring snapshots")
)

// orgFileName is the organization metadata file at an org root, the identity
// record the wrong-root refusal reads.
const orgFileName = "org.json"

// PlanAction is what one plan entry does to its local path.
type PlanAction int

// The actions a [PlanEntry] can carry.
const (
	// ActionSkip leaves a local file already verified identical to the
	// mirrored object.
	ActionSkip PlanAction = iota
	// ActionCreate materializes an object with no local file.
	ActionCreate
	// ActionReplace replaces a local file whose content differs from the
	// mirrored object.
	ActionReplace
	// ActionRefuse declines to touch a path whose local and mirrored copies
	// conflict in a way the restore cannot order; Reason says why.
	ActionRefuse
)

// String names the action the way the plan reports it.
func (a PlanAction) String() string {
	switch a {
	case ActionSkip:
		return "skip"
	case ActionCreate:
		return "create"
	case ActionReplace:
		return "replace"
	case ActionRefuse:
		return "refuse"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// PlanEntry is one restorable object's planned disposition.
type PlanEntry struct {
	// Rel is the archive-relative path, equally the local path under the org
	// root and the mirror key's suffix.
	Rel string
	// Reason explains a refusal or a kept local file; empty otherwise.
	Reason string
	// Info is the object as the mirror's listing reported it. Its digests are
	// unresolved (listings carry none); the execute step resolves them with a
	// Head before any byte lands.
	Info remote.ObjectInfo
	// Action is what the restore will do at Rel.
	Action PlanAction
	// Snapshot marks a ledger snapshot, the entries held back until every
	// data file has landed.
	Snapshot bool
}

// StubEntry is one mirrored configuration-version tarball whose local
// eviction stub the restore ensures (see [store.RemoteStubPath]): the trace
// that lets a reader list and fetch the evicted object without the mirror's
// inventory.
type StubEntry struct {
	// Rel is the tarball's archive-relative path.
	Rel string
	// Existing reports a stub file already standing at plan time; the
	// execute step verifies it against the mirror instead of creating it.
	Existing bool
}

// Plan is the classified work one restore would perform. Create instances
// with [Restorer.Plan].
type Plan struct {
	Entries  []PlanEntry
	Refusals []PlanEntry
	// Stubs lists the mirrored configuration-version tarballs whose local
	// eviction stubs the restore ensures, sorted by path.
	Stubs []StubEntry
	// Leftovers names the mirrored keys nothing in the restored tree
	// accounts for, sorted; while any stand, a marker not already complete
	// settles partial.
	Leftovers    []string
	RestoreFiles int
	RestoreBytes int64
	Skipped      int
}

// Plan lists the mirror and classifies every restorable object against the
// local tree at orgRoot, writing nothing. The caller holds the archive lock,
// which also pins the plan: nothing else can reshape the tree between
// planning and executing.
//
// Classifying a local file of the mirrored object's exact size costs one
// metadata Head to resolve the digest the listing cannot carry, and an
// org-root snapshot that differs is downloaded whole (it holds only
// org-level entries) to order the two copies; these are the whole of a dry
// run's egress. The refusals a plan carries ([PlanEntry.Reason] on
// [ActionRefuse]) are conflicts the restore will not resolve by guessing; a
// local replay log where snapshots must land refuses the plan itself with
// [ErrLocalReplayLog], and a target whose organization record names another
// organization refuses with [ErrOrgMismatch].
func (r *Restorer) Plan(ctx context.Context, orgRoot, org string) (Plan, error) {
	err := checkOrgIdentity(orgRoot, org)
	if err != nil {
		return Plan{}, err
	}

	inv, err := r.inventory(ctx, orgRoot, org)
	if err != nil {
		return Plan{}, err
	}

	entries, err := r.classify(ctx, orgRoot, org, inv.restorable)
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{Entries: entries, Leftovers: inv.leftovers}

	for _, e := range entries {
		switch e.Action {
		case ActionCreate, ActionReplace:
			plan.RestoreFiles++
			plan.RestoreBytes += e.Info.Size

		case ActionSkip:
			plan.Skipped++
		case ActionRefuse:
			plan.Refusals = append(plan.Refusals, e)
		}
	}

	plan.Stubs, err = planStubs(orgRoot, inv.tarballs)
	if err != nil {
		return Plan{}, err
	}

	err = checkReplayLog(orgRoot, plan)
	if err != nil {
		return Plan{}, err
	}

	return plan, nil
}

// planStubs classifies each mirrored tarball's local eviction stub with a
// stat, so a dry run prices the stub work without a request. A tarball whose
// own bytes sit locally needs no stub at all: the file itself accounts for
// the key, and a stub beside a live tarball is the eviction's crash shape,
// not a state to create deliberately.
func planStubs(orgRoot string, tarballs []string) ([]StubEntry, error) {
	var stubs []StubEntry

	for _, rel := range tarballs {
		local, err := fileExists(pathkit.ConfineJoin(orgRoot, rel))
		if err != nil {
			return nil, err
		}

		if local {
			continue
		}

		existing, err := fileExists(pathkit.ConfineJoin(orgRoot, store.RemoteStubPath(rel)))
		if err != nil {
			return nil, err
		}

		stubs = append(stubs, StubEntry{Rel: rel, Existing: existing})
	}

	return stubs, nil
}

// fileExists reports whether a regular file stands at abs, returning any
// stat fault other than absence: a plan cannot classify a path it could not
// probe.
func fileExists(abs string) (bool, error) {
	st, err := os.Stat(abs)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("stat %q: %w", abs, err)
	}

	return st.Mode().IsRegular(), nil
}

// rawInventory is the relativized mirror listing split by what the restore
// does with each key: the restorable set it materializes, the evicted
// tarballs whose local stubs it ensures, and the leftovers nothing in the
// restored tree accounts for.
type rawInventory struct {
	restorable map[string]remote.ObjectInfo
	tarballs   []string
	leftovers  []string
}

// inventory lists the organization's mirror prefix and classifies every key
// into the restore's accounting. The restorable set (see [collect.Restorable])
// is what the plan materializes; an evicted configuration-version tarball is
// accounted for by the local stub the restore ensures; an evicted bundle zip
// is accounted for by its seal sidecar, which is itself restorable, or by one
// already on disk; and the organization marker's lifecycle belongs to the
// restore. Every other key is a leftover: a key spelling no honest archive
// path (recorded raw, since it has no relative form), a shape the mirror must
// never hold (a replay log, a lock, a staging temp, a mirrored stub copy), or
// foreign junk. Leftovers restore nothing and hold a marker not already
// complete at partial, so a reader never trusts a "complete" tree that
// cannot account for what the mirror holds.
func (r *Restorer) inventory(ctx context.Context, orgRoot, org string) (rawInventory, error) {
	prefix := r.cfg.Key(org, "") + "/"

	raw, err := r.client.List(ctx, prefix)
	if err != nil {
		return rawInventory{}, fmt.Errorf("list mirror inventory: %w", err)
	}

	inv := rawInventory{restorable: make(map[string]remote.ObjectInfo, len(raw))}

	var zips []string

	for key, info := range raw {
		rel, ok := relFromKey(key, prefix)

		switch {
		case !ok:
			inv.leftovers = append(inv.leftovers, key)
		case rel == remote.MarkerName:
			// Accounted: the restore owns the marker's lifecycle.
		case collect.Restorable(rel):
			inv.restorable[rel] = info
		case store.IsConfigTarball(rel):
			inv.tarballs = append(inv.tarballs, rel)
		case store.IsBundleZip(rel):
			zips = append(zips, rel)
		default:
			inv.leftovers = append(inv.leftovers, rel)
		}
	}

	// The zips resolve after the walk, since a zip's sidecar key may list
	// after the zip itself. A zip whose sidecar the mirror holds is accounted
	// for once the sidecar restores; one whose sidecar is only local is
	// accounted for by that file; one with no sidecar anywhere is a leftover,
	// because nothing local can ever list its members.
	for _, zip := range slices.Sorted(slices.Values(zips)) {
		sidecar := zip + seal.SidecarSuffix
		if _, ok := inv.restorable[sidecar]; ok {
			continue
		}

		present, perr := fileExists(pathkit.ConfineJoin(orgRoot, sidecar))
		if perr != nil {
			return rawInventory{}, perr
		}

		if !present {
			inv.leftovers = append(inv.leftovers, zip)
		}
	}

	slices.Sort(inv.tarballs)
	slices.Sort(inv.leftovers)

	return inv, nil
}

// relFromKey strips the organization prefix from one listed key, reporting
// false for a key outside the prefix, the prefix itself, or a key whose
// remainder spells no honest archive path (an empty, ".", or ".." segment, a
// backslash), which joined under the root could land outside it.
func relFromKey(key, prefix string) (string, bool) {
	rel, ok := strings.CutPrefix(key, prefix)
	if !ok || rel == "" || seal.ValidName(rel) != nil {
		return "", false
	}

	return rel, true
}

// classify resolves each listed object's disposition against the local tree,
// bounded by the restorer's concurrency: most paths settle on a stat alone,
// and only a local file of the exact mirrored size costs a Head and a local
// hash to tell identical from superseded.
func (r *Restorer) classify(
	ctx context.Context, orgRoot, org string, listing map[string]remote.ObjectInfo,
) ([]PlanEntry, error) {
	rels := slices.Sorted(maps.Keys(listing))

	entries := make([]PlanEntry, len(rels))

	// The derived context stops in-flight metadata reads once one
	// classification has already doomed the plan.
	g, ctx := errgroup.WithContext(ctx)

	g.SetLimit(r.concurrency)

	for i, rel := range rels {
		g.Go(func() error {
			entry, err := r.classifyOne(ctx, orgRoot, org, rel, listing[rel])
			if err != nil {
				return err
			}

			entries[i] = entry

			return nil
		})
	}

	err := g.Wait()
	if err != nil {
		return nil, fmt.Errorf("classify restore plan: %w", err)
	}

	return entries, nil
}

// classifyOne settles one object's disposition. An absent local file
// restores; a present one is proven identical (skip) or superseded (replace)
// by size and recorded digest, except a ledger snapshot, whose conflicts are
// ordered by run metadata or refused (see [classifySnapshot]).
func (r *Restorer) classifyOne(
	ctx context.Context, orgRoot, org, rel string, info remote.ObjectInfo,
) (PlanEntry, error) {
	entry := PlanEntry{Rel: rel, Info: info, Snapshot: manifest.IsSnapshotPath(rel)}

	abs := pathkit.ConfineJoin(orgRoot, rel)

	st, err := os.Stat(abs)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		entry.Action = ActionCreate

		return entry, nil

	case err != nil:
		return PlanEntry{}, fmt.Errorf("stat %q: %w", abs, err)
	}

	same, err := r.sameContent(ctx, org, rel, abs, st.Size(), info)
	if err != nil {
		return PlanEntry{}, err
	}

	if same {
		entry.Action = ActionSkip

		return entry, nil
	}

	if entry.Snapshot {
		return r.classifySnapshot(ctx, orgRoot, org, entry)
	}

	entry.Action = ActionReplace

	return entry, nil
}

// sameContent reports whether the local file at abs already holds the
// mirrored object's content. A size mismatch settles it without a request;
// an exact size resolves the recorded digest through one Head (listings
// carry none) and hashes the local bytes against it. An object the mirror
// recorded no digest for is taken as identical on size alone: the same trust
// the sweep's incremental gate and the viewer extend, and the restore's own
// transfer verification can do no better for such an object either.
func (r *Restorer) sameContent(
	ctx context.Context, org, rel, abs string, localSize int64, info remote.ObjectInfo,
) (bool, error) {
	if localSize != info.Size {
		return false, nil
	}

	head, err := r.client.Head(ctx, r.cfg.Key(org, rel))
	if err != nil {
		return false, fmt.Errorf("resolve digest for %q: %w", rel, err)
	}

	var (
		sum  hash.Hash
		want []byte
	)

	switch {
	case head.SHA256 != "":
		decoded, derr := hex.DecodeString(head.SHA256)
		if derr != nil {
			return false, fmt.Errorf("recorded sha256 for %q: %w", rel, derr)
		}

		sum, want = sha256.New(), decoded

	case len(head.MD5) > 0:
		//nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
		sum, want = md5.New(), head.MD5
	default:
		return true, nil
	}

	//nolint:gosec // The path is confined under the org root being restored.
	f, err := os.Open(abs)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", abs, err)
	}

	defer func() {
		//nolint:errcheck // Read-only file; the hash result is what matters.
		_ = f.Close()
	}()

	_, err = io.Copy(sum, f)
	if err != nil {
		return false, fmt.Errorf("hash %q: %w", abs, err)
	}

	return bytes.Equal(sum.Sum(nil), want), nil
}

// classifySnapshot settles a ledger snapshot whose local and mirrored copies
// differ. Only the org-root snapshot carries run metadata to order the two:
// a strictly newer mirrored copy replaces, a strictly newer local copy is
// kept (the mirror never saw its state, and overwriting would discard it
// silently), and copies the metadata cannot order refuse. A non-root
// snapshot offers nothing to order by, so a differing one always refuses:
// guessing which ledger state survives is exactly what a restore must not
// do. Metadata that cannot be read at all refuses the same way, per path
// rather than aborting the whole plan: one corrupt snapshot must not block
// restoring everything else in exactly the lost-tree scenario the restore
// serves.
func (r *Restorer) classifySnapshot(
	ctx context.Context, orgRoot, org string, entry PlanEntry,
) (PlanEntry, error) {
	if entry.Rel != path.Join(manifest.LedgerDirName, manifest.SnapshotFileName) {
		entry.Action = ActionRefuse
		entry.Reason = "the local shard snapshot differs from the mirrored one and carries no run " +
			"metadata to order them; resolve it by hand (keep one copy) before re-running"

		return entry, nil
	}

	local, err := snapshotMetaFile(pathkit.ConfineJoin(orgRoot, entry.Rel))
	if err != nil {
		entry.Action = ActionRefuse
		entry.Reason = "the local org-root snapshot's run metadata cannot be read (" + err.Error() +
			"); re-run pull, and if the fault persists resolve it by hand (keep one copy)"

		//nolint:nilerr // The fault becomes a per-path refusal, not a plan abort.
		return entry, nil
	}

	mirrored, err := r.snapshotMetaRemote(ctx, org, entry)

	switch {
	case ctx.Err() != nil:
		// A wind-down surfacing through the fetch is the interrupt, not a
		// property of the snapshot; refusing here would dress it in
		// conflict-resolution advice.
		return PlanEntry{}, ctx.Err() //nolint:wrapcheck // The cancellation names itself.
	case err != nil:
		entry.Action = ActionRefuse
		entry.Reason = "the mirrored org-root snapshot's run metadata cannot be read (" + err.Error() +
			"); re-run pull, and if the fault persists resolve it by hand (keep one copy)"

		//nolint:nilerr // The fault becomes a per-path refusal, not a plan abort.
		return entry, nil
	}

	switch {
	case mirrored.Newer(local):
		entry.Action = ActionReplace
	case local.Newer(mirrored):
		entry.Action = ActionSkip
		entry.Reason = "the local snapshot is newer than the mirrored one; kept"

		r.logger.LogAttrs(ctx, slog.LevelWarn, "pull_snapshot_kept",
			slog.String("org", org),
			slog.String("path", entry.Rel),
			slog.Time("local_run", local.LastRunAt),
			slog.Time("mirrored_run", mirrored.LastRunAt),
		)

	default:
		entry.Action = ActionRefuse
		entry.Reason = "the local and mirrored org-root snapshots differ but their run metadata " +
			"does not order them; resolve it by hand (keep one copy) before re-running"
	}

	return entry, nil
}

// snapshotMetaFile decodes the run metadata of the snapshot at abs.
func snapshotMetaFile(abs string) (manifest.SnapshotMeta, error) {
	//nolint:gosec // The path is confined under the org root being restored.
	f, err := os.Open(abs)
	if err != nil {
		return manifest.SnapshotMeta{}, fmt.Errorf("open %q: %w", abs, err)
	}

	defer func() {
		//nolint:errcheck // Read-only file; the decode result is what matters.
		_ = f.Close()
	}()

	meta, err := manifest.DecodeSnapshotMeta(f)
	if err != nil {
		return manifest.SnapshotMeta{}, fmt.Errorf("%q: %w", abs, err)
	}

	return meta, nil
}

// snapshotMetaRemote downloads the mirrored org-root snapshot, verified
// against its recorded digest, and decodes its run metadata. The snapshot
// holds only org-level entries, so buffering it whole is fine.
func (r *Restorer) snapshotMetaRemote(
	ctx context.Context, org string, entry PlanEntry,
) (manifest.SnapshotMeta, error) {
	key := r.cfg.Key(org, entry.Rel)

	head, err := r.client.Head(ctx, key)
	if err != nil {
		return manifest.SnapshotMeta{}, fmt.Errorf("resolve digest for %q: %w", entry.Rel, err)
	}

	var buf bytes.Buffer

	_, err = r.client.DownloadVerified(ctx, key, head, &buf)
	if err != nil {
		return manifest.SnapshotMeta{}, fmt.Errorf("fetch mirrored snapshot %q: %w", entry.Rel, err)
	}

	meta, err := manifest.DecodeSnapshotMeta(&buf)
	if err != nil {
		return manifest.SnapshotMeta{}, fmt.Errorf("mirrored %q: %w", entry.Rel, err)
	}

	return meta, nil
}

// checkOrgIdentity refuses a target directory whose archived organization
// record names another organization: the marker records only the mirror's
// location, so a tree moved under the wrong organization's root would
// otherwise splice two organizations' archives together. An absent or
// nameless record proves nothing and passes.
func checkOrgIdentity(orgRoot, org string) error {
	//nolint:gosec // The path is composed from the org root being restored.
	data, err := os.ReadFile(filepath.Join(orgRoot, orgFileName))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read %q: %w", orgFileName, err)
	}

	// The record is the jsonapi document the collector archives, so the name
	// lives at data.attributes.name.
	var record struct {
		Data struct {
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}

	err = json.Unmarshal(data, &record)
	if err != nil {
		return fmt.Errorf("parse %q: %w", orgFileName, err)
	}

	if name := record.Data.Attributes.Name; name != "" && name != org {
		return fmt.Errorf("%w: %s records organization %q, not %q",
			ErrOrgMismatch, orgFileName, name, org)
	}

	return nil
}

// checkReplayLog refuses a plan that would write snapshots while a local
// replay log stands at the org root: the next ledger load would replay the
// log's records over the restored snapshots, splicing the local history the
// log holds with the mirrored history the snapshots hold. The check runs at
// plan time, before the restoring marker or any byte lands, so the escape it
// names (a clean archive run, which folds the log away) is still reachable;
// a marker stamped first would refuse that very run.
func checkReplayLog(orgRoot string, plan Plan) error {
	writesSnapshot := slices.ContainsFunc(plan.Entries, func(e PlanEntry) bool {
		return e.Snapshot && (e.Action == ActionCreate || e.Action == ActionReplace)
	})

	if !writesSnapshot {
		return nil
	}

	logPath := filepath.Join(orgRoot, manifest.LedgerDirName, manifest.LogFileName)

	_, err := os.Stat(logPath)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat %q: %w", logPath, err)
	}

	return fmt.Errorf("%w: %s holds local ledger changes the mirrored snapshots do not; "+
		"run the archiver once to fold it away, or delete the .ledger directory to consent "+
		"to losing them", ErrLocalReplayLog, logPath)
}
