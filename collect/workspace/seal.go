package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/seal"
)

var (
	// Heavy, audit-only run artifacts that seal into a workspace's logs bundle;
	// per-check policy-check-<id>.log files join them by pattern.
	runHeavyNames = map[string]struct{}{
		"plan.log":          {},
		"plan.json":         {},
		"apply.log":         {},
		"cost-estimate.log": {},
	}

	// Immutable run-child filenames, each mapped to the roll-up it coalesces into;
	// run.json is absent because it is mutable: it coalesces into runsRollup
	// through its own terminal-status gate rather than by filename alone.
	runRollups = map[string]string{
		"config-version.json":         "config-versions.ndjson",
		"config-version-ingress.json": "config-versions.ndjson",
		"plan-summary.json":           "plan-summaries.ndjson",
		"apply-summary.json":          "apply-summaries.ndjson",
		"cost-estimate.json":          "cost-estimates.ndjson",
		"comments.json":               "comments.ndjson",
		"run-events.json":             "run-events.ndjson",
		"policy-checks.json":          "policy-checks.ndjson",
		"task-stages.json":            "task-stages.ndjson",
		"tf-policy-evaluations.json":  "tf-policy-evaluations.ndjson",
		"tf-policy-outcomes.json":     "tf-policy-outcomes.ndjson",
	}
)

// stateMetaRollup is the roll-up the immutable state-version meta sidecars
// coalesce into.
const stateMetaRollup = "state-versions.ndjson"

// runsRollup is the roll-up the run.json summaries of settled, terminal runs
// coalesce into; an in-flight run's summary stays loose and keeps refreshing.
const runsRollup = "runs.ndjson"

// SealWorkspace packs the workspace's frozen cold artifacts into bundles and
// roll-ups beside its loose metadata, then removes the loose originals.
//
// The heavy, audit-only run artifacts (plan and apply logs, the structured plan
// JSON, policy-check logs) seal into a deflated logs bundle; the raw and
// JSON-format state blobs seal into a stored state bundle, kept uncompressed so
// the irreplaceable state stays greppable on disk; and the immutable small
// metadata (the per-run children and per-state-version meta sidecars) coalesces
// into per-workspace NDJSON roll-ups. The run.json summary is mutable, so it
// coalesces only once its run is terminal: a settled summary whose recorded
// status is final folds into the runs roll-up, while an in-flight run's stays
// loose and keeps refreshing ([collect.Env.Mutable]'s sealed-elsewhere gate is
// what keeps the coalesced copy from being re-materialized by the next
// re-walk). Only settled artifacts of a fully-walked collection are sealed,
// and each loose source is removed only after its bundle or roll-up is durable
// and verified, so a pass is safe to interrupt and re-run. Nothing is
// re-fetched: the ledger keys are unchanged, so a re-run treats a sealed
// object exactly as it did the loose one. Run directories emptied by the seal
// are pruned.
func (c *Collector) SealWorkspace(ctx context.Context, project, ws string) error {
	logs, err := c.frozenRunArtifacts(project, ws)
	if err != nil {
		return fmt.Errorf("gather frozen run artifacts: %w", err)
	}

	states, err := c.frozenStateArtifacts(project, ws)
	if err != nil {
		return fmt.Errorf("gather frozen state artifacts: %w", err)
	}

	metadata, err := c.frozenMetadata(project, ws)
	if err != nil {
		return fmt.Errorf("gather frozen metadata: %w", err)
	}

	err = c.sealBundle(ctx, project, ws, "logs", logs)
	if err != nil {
		return err
	}

	err = c.sealBundle(ctx, project, ws, "state", states)
	if err != nil {
		return err
	}

	err = c.coalesce(ctx, project, ws, metadata)
	if err != nil {
		return err
	}

	err = c.removeEmptyRunDirs(project, ws)
	if err != nil {
		return fmt.Errorf("prune empty run dirs: %w", err)
	}

	// With a remote store configured, sweep every sealed local bundle out to
	// it — the ones sealed just above and any survivor of an earlier run —
	// so peak local disk stays bounded to the search layer plus in-flight
	// work, and pointing a remote at an existing all-local archive migrates
	// it one re-run later.
	if c.env.Remote() != nil {
		return c.evictBundles(ctx, project, ws)
	}

	return nil
}

// evictBundles offloads each of the workspace's sealed local bundles to the
// remote store and removes the local zip once the remote copy is confirmed.
//
// Only a zip with its sidecar beside it is swept: seal.Seal writes the
// sidecar after the bundle read-back verifies, so the sidecar is the "sealed
// and verified" marker, and an orphan zip without one (a crash mid-seal) is
// unverified with its loose sources still canonical — it must never be
// uploaded. The sidecar itself always stays local; it is part of the search
// layer and, listed by nextGeneration, is what keeps an evicted generation's
// number from being reused.
//
// A failure to evict one bundle is logged and does not abort the pass or the
// workspace: the local zip simply stays canonical, exactly as if the remote
// were never configured, and the next run re-sweeps it.
func (c *Collector) evictBundles(ctx context.Context, project, ws string) error {
	st := c.env.Store()
	bundlesRel := st.BundleDir(project, ws)
	bundlesDir := st.AbsPath(bundlesRel)

	names, err := listNames(bundlesDir, func(e fs.DirEntry) bool {
		return !e.IsDir() && strings.HasSuffix(e.Name(), ".zip")
	})
	if err != nil {
		return fmt.Errorf("list bundles to evict: %w", err)
	}

	slices.Sort(names)

	for _, name := range names {
		if ctx.Err() != nil {
			return fmt.Errorf("evict bundles: %w", ctx.Err())
		}

		sidecar := filepath.Join(bundlesDir, name+seal.SidecarSuffix)

		_, statErr := os.Stat(sidecar)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				// An orphan zip from a crash mid-seal: unverified, loose
				// sources still canonical. Leave it; the next seal writes a
				// fresh generation.
				continue
			}

			return fmt.Errorf("stat sidecar %q: %w", sidecar, statErr)
		}

		relPath := st.Join(bundlesRel, name)

		evictErr := c.env.OffloadFile(ctx, relPath)
		if evictErr != nil {
			c.env.Log().LogAttrs(ctx, slog.LevelWarn, "bundle_evict_error",
				slog.String("path", relPath),
				slog.String("error", evictErr.Error()),
			)
		}
	}

	return nil
}

// frozenRunArtifacts returns the workspace's loose, settled heavy run artifacts,
// or nil until the runs collection has been walked to its end.
func (c *Collector) frozenRunArtifacts(project, ws string) ([]seal.Member, error) {
	st := c.env.Store()
	runsKey := st.Join(st.WorkspaceDir(project, ws), "runs")

	if !c.env.IsCollectionComplete(runsKey) {
		return nil, nil
	}

	runIDs, err := subdirs(st.AbsPath(runsKey))
	if err != nil {
		return nil, err
	}

	var members []seal.Member

	for _, runID := range runIDs {
		names, filesErr := heavyRunFiles(st.AbsPath(st.RunDir(project, ws, runID)))
		if filesErr != nil {
			return nil, filesErr
		}

		for _, name := range names {
			relPath := st.RunFile(project, ws, runID, name)
			if c.settled(relPath) {
				members = append(members, seal.Member{
					Name:     relPath,
					Source:   st.AbsPath(relPath),
					Compress: true,
				})
			}
		}
	}

	sortMembers(members)

	return members, nil
}

// frozenStateArtifacts returns the workspace's loose, settled state blobs, or nil
// until the state-versions collection has been walked to its end.
func (c *Collector) frozenStateArtifacts(project, ws string) ([]seal.Member, error) {
	st := c.env.Store()
	svKey := st.StateVersionDir(project, ws)

	if !c.env.IsCollectionComplete(svKey) {
		return nil, nil
	}

	names, err := stateBlobFiles(st.AbsPath(svKey))
	if err != nil {
		return nil, err
	}

	var members []seal.Member

	for _, name := range names {
		relPath := st.Join(svKey, name)
		if c.settled(relPath) {
			members = append(members, seal.Member{
				Name:     relPath,
				Source:   st.AbsPath(relPath),
				Compress: false,
			})
		}
	}

	sortMembers(members)

	return members, nil
}

// frozenMetadata returns the workspace's loose, settled frozen metadata
// grouped by the roll-up it coalesces into: the immutable per-run children,
// the run.json summaries of terminal runs, and the per-state-version meta
// sidecars. An in-flight run's mutable run.json stays loose. Each collection
// contributes only once it has been walked to its end.
func (c *Collector) frozenMetadata(project, ws string) (map[string][]seal.Member, error) {
	st := c.env.Store()
	out := make(map[string][]seal.Member)

	runsKey := st.Join(st.WorkspaceDir(project, ws), "runs")
	if c.env.IsCollectionComplete(runsKey) {
		runIDs, err := subdirs(st.AbsPath(runsKey))
		if err != nil {
			return nil, err
		}

		for _, runID := range runIDs {
			names, filesErr := metadataRunFiles(st.AbsPath(st.RunDir(project, ws, runID)))
			if filesErr != nil {
				return nil, filesErr
			}

			for _, name := range names {
				relPath := st.RunFile(project, ws, runID, name)
				if c.settled(relPath) {
					rollup := runRollups[name]
					out[rollup] = append(out[rollup], seal.Member{Name: relPath, Source: st.AbsPath(relPath)})
				}
			}

			// The mutable run.json freezes only once its recorded status is
			// terminal; the status lives in the document, not the ledger, so
			// the loose file itself is the gate.
			runJSON := st.RunFile(project, ws, runID, "run.json")
			if c.settled(runJSON) && terminalRunFile(st.AbsPath(runJSON)) {
				out[runsRollup] = append(out[runsRollup], seal.Member{Name: runJSON, Source: st.AbsPath(runJSON)})
			}
		}
	}

	svKey := st.StateVersionDir(project, ws)
	if c.env.IsCollectionComplete(svKey) {
		names, err := stateMetaFiles(st.AbsPath(svKey))
		if err != nil {
			return nil, err
		}

		for _, name := range names {
			relPath := st.Join(svKey, name)
			if c.settled(relPath) {
				out[stateMetaRollup] = append(
					out[stateMetaRollup],
					seal.Member{Name: relPath, Source: st.AbsPath(relPath)},
				)
			}
		}
	}

	return out, nil
}

// removeEmptyRunDirs removes the workspace's now-empty runs/<id>/ directories,
// then the runs/ directory itself if nothing is left — the residue of a seal
// that coalesced a run's last loose file. A vanished directory is tolerated;
// a directory that still holds anything is left alone.
func (c *Collector) removeEmptyRunDirs(project, ws string) error {
	st := c.env.Store()
	runsDir := st.AbsPath(st.Join(st.WorkspaceDir(project, ws), "runs"))

	runIDs, err := subdirs(runsDir)
	if err != nil {
		return err
	}

	for _, runID := range runIDs {
		err = removeIfEmpty(st.AbsPath(st.RunDir(project, ws, runID)))
		if err != nil {
			return err
		}
	}

	return removeIfEmpty(runsDir)
}

// removeIfEmpty removes dir when it holds no entries, tolerating a dir that
// does not exist or vanishes between the check and the remove.
func removeIfEmpty(dir string) error {
	entries, err := listNames(dir, func(fs.DirEntry) bool { return true })
	if err != nil {
		return err
	}

	if len(entries) > 0 {
		return nil
	}

	err = os.Remove(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %q: %w", dir, err)
	}

	return nil
}

// settled reports whether the object at relPath is recorded done, the gate that
// keeps a seal to artifacts the ledger has fully archived.
func (c *Collector) settled(relPath string) bool {
	entry, ok := c.env.Entry(relPath)

	return ok && entry.Status == manifest.StatusDone
}

// sealBundle seals members into the next-generation bundle named by prefix, a
// no-op when nothing is frozen to seal.
//
// The members are first reconciled against the already-sealed sidecars, so a
// loose source a prior seal committed to a bundle but did not remove (a crash,
// or a best-effort removal failure) is not swept into a fresh generation, which
// would duplicate its archive-relative name across two bundles. When nothing
// fresh remains after reconciliation, no generation is allocated.
func (c *Collector) sealBundle(ctx context.Context, project, ws, prefix string, members []seal.Member) error {
	if len(members) == 0 {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("seal %s bundle: %w", prefix, ctx.Err())
	}

	bundlesDir := c.env.Store().AbsPath(c.env.Store().BundleDir(project, ws))

	fresh, err := c.reconcileSealed(ctx, bundlesDir, prefix, members)
	if err != nil {
		return fmt.Errorf("reconcile %s bundle: %w", prefix, err)
	}

	if len(fresh) == 0 {
		return nil
	}

	gen, err := nextGeneration(bundlesDir, prefix)
	if err != nil {
		return err
	}

	bundlePath := filepath.Join(bundlesDir, fmt.Sprintf("%s.gen%04d.zip", prefix, gen))

	_, err = seal.Seal(bundlePath, fresh)
	if err != nil {
		return fmt.Errorf("seal %s bundle: %w", prefix, err)
	}

	return nil
}

// reconcileSealed splits members into those still to seal and those a prior,
// possibly crash-interrupted, run already sealed durably.
//
// Sealing commits the bundle and its sidecar before removing the loose
// sources, so a crash -- or a best-effort removal failure -- between the two
// leaves a settled source on disk whose name a sidecar already records. Left
// alone, the next seal would sweep that survivor into a fresh generation,
// writing the same archive-relative name into two bundles that the remote sweep
// would both upload. A member whose name no sidecar records is fresh; a member
// whose name is recorded and whose current bytes hash to the recorded digest is
// already sealed -- its loose source is removed best-effort and dropped; a
// member whose bytes diverge from the recorded digest is fresh, sealed into a
// new generation rather than dropped, so changed content is never lost. When no
// sidecar records any name, the members pass through untouched, the common case.
func (c *Collector) reconcileSealed(
	ctx context.Context,
	bundlesDir, prefix string,
	members []seal.Member,
) ([]seal.Member, error) {
	sealed, err := c.sealedDigests(bundlesDir, prefix)
	if err != nil {
		return nil, err
	}

	if len(sealed) == 0 {
		return members, nil
	}

	fresh := make([]seal.Member, 0, len(members))

	for i := range members {
		digest, recorded := sealed[members[i].Name]
		if !recorded {
			fresh = append(fresh, members[i])

			continue
		}

		got, digestErr := seal.SourceDigest(members[i].Source)
		if digestErr != nil {
			return nil, fmt.Errorf("digest %s source %q: %w", prefix, members[i].Name, digestErr)
		}

		// Only bytes that still hash to the sidecar's record are proven already
		// sealed; divergent content is sealed afresh, never dropped unproven.
		if got != digest {
			fresh = append(fresh, members[i])

			continue
		}

		c.env.Log().LogAttrs(ctx, slog.LevelWarn, "seal_reconcile_stranded_source",
			slog.String("prefix", prefix),
			slog.String("name", members[i].Name),
		)

		//nolint:errcheck // Best-effort; a survivor is reconciled again next run.
		_ = os.Remove(members[i].Source)
	}

	return fresh, nil
}

// sealedDigests maps the archive-relative name of every member already durably
// sealed under prefix to the SHA-256 its sidecar recorded. It reads only the
// generation-numbered sidecars, the marker seal.Seal writes after a bundle's
// read-back verifies, so a name in the map is proven to live in a verified
// bundle. Sidecars are read in generation order, so a later generation's entry
// wins for a name that recurs, though names are not expected to recur.
func (c *Collector) sealedDigests(bundlesDir, prefix string) (map[string]string, error) {
	names, err := listNames(bundlesDir, func(e fs.DirEntry) bool {
		return !e.IsDir() &&
			strings.HasSuffix(e.Name(), seal.SidecarSuffix) &&
			parseGeneration(e.Name(), prefix) > 0
	})
	if err != nil {
		return nil, fmt.Errorf("list %s sidecars: %w", prefix, err)
	}

	slices.Sort(names)

	out := make(map[string]string)

	for _, name := range names {
		entries, readErr := seal.ReadSidecar(filepath.Join(bundlesDir, name))
		if readErr != nil {
			return nil, fmt.Errorf("read %s sidecar %q: %w", prefix, name, readErr)
		}

		for i := range entries {
			out[entries[i].Name] = entries[i].SHA256
		}
	}

	return out, nil
}

// coalesce folds each roll-up's frozen members into its NDJSON file, a no-op when
// there is nothing to coalesce. Roll-up files are processed in a stable order so
// the pass is deterministic.
func (c *Collector) coalesce(ctx context.Context, project, ws string, rollups map[string][]seal.Member) error {
	if len(rollups) == 0 {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("coalesce metadata: %w", ctx.Err())
	}

	rollupDir := c.env.Store().AbsPath(c.env.Store().RollupDir(project, ws))

	for _, name := range slices.Sorted(maps.Keys(rollups)) {
		members := rollups[name]
		sortMembers(members)

		err := seal.Rollup(filepath.Join(rollupDir, name), members)
		if err != nil {
			return fmt.Errorf("coalesce %s: %w", name, err)
		}
	}

	return nil
}

// listNames returns the names of dir's entries that keep selects, tolerating a
// dir that does not exist (a workspace with no runs or state versions yet). It
// is the shared body of the frozen-artifact listers, each of which differs only
// in its predicate.
func listNames(dir string, keep func(fs.DirEntry) bool) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var out []string

	for _, e := range entries {
		if keep(e) {
			out = append(out, e.Name())
		}
	}

	return out, nil
}

// subdirs lists the immediate subdirectory names of dir.
func subdirs(dir string) ([]string, error) {
	return listNames(dir, func(e fs.DirEntry) bool {
		return e.IsDir()
	})
}

// heavyRunFiles lists the heavy, audit-only artifact filenames present in a run
// directory.
func heavyRunFiles(dir string) ([]string, error) {
	return listNames(dir, func(e fs.DirEntry) bool {
		return !e.IsDir() && isHeavyRunFile(e.Name())
	})
}

// isHeavyRunFile reports whether a run-directory filename is a heavy, audit-only
// artifact that seals rather than staying loose.
func isHeavyRunFile(name string) bool {
	if _, ok := runHeavyNames[name]; ok {
		return true
	}

	return strings.HasPrefix(name, "policy-check-") && strings.HasSuffix(name, ".log")
}

// metadataRunFiles lists the immutable run-child filenames present in a run
// directory that coalesce into roll-ups.
func metadataRunFiles(dir string) ([]string, error) {
	return listNames(dir, func(e fs.DirEntry) bool {
		if e.IsDir() {
			return false
		}

		_, ok := runRollups[e.Name()]

		return ok
	})
}

// stateMetaFiles lists the immutable state-version meta sidecars in a
// state-versions directory.
func stateMetaFiles(dir string) ([]string, error) {
	return listNames(dir, func(e fs.DirEntry) bool {
		return !e.IsDir() && strings.HasSuffix(e.Name(), ".meta.json")
	})
}

// stateBlobFiles lists the raw and JSON-format state blobs in a state-versions
// directory, leaving the meta sidecars loose.
func stateBlobFiles(dir string) ([]string, error) {
	return listNames(dir, func(e fs.DirEntry) bool {
		return !e.IsDir() && isStateBlob(e.Name())
	})
}

// isStateBlob reports whether a state-versions filename is a raw or JSON-format
// state blob, the heavy artifacts that seal; the meta sidecar stays loose. Both
// the raw ".tfstate.json" and JSON-format ".json" blobs end in ".json", so one
// suffix test covers them, excluding only the ".meta.json" sidecar.
func isStateBlob(name string) bool {
	return strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".meta.json")
}

// nextGeneration returns the generation number for a new bundle named by prefix,
// one past the highest already present, so a bundle is written once and never
// rewritten.
//
// It lists the directory literally rather than globbing: the bundles path is
// rooted at the operator-chosen archive root, so a glob metacharacter in that
// root ('*', '?', or a '[' character class) would make [filepath.Glob] match
// nothing, restart numbering at 1, and overwrite the prior generation's sealed
// bundles. [os.ReadDir] treats the whole path as a literal, and parseGeneration
// ignores any name that is not a bundle leaf.
//
// The highest generation is taken over both the zips and their sidecar
// indexes. Remote eviction removes a verified zip but always leaves its
// sidecar, so counting sidecars is what keeps numbering monotonic once
// bundles move off disk — a zip-only scan would restart at gen0001 and
// overwrite remote history at the same key. Counting the zips as well means
// a surviving zip whose sidecar was lost (a partial restore, a damaged file)
// still blocks its generation from reuse.
func nextGeneration(bundlesDir, prefix string) (int, error) {
	names, err := listNames(bundlesDir, func(e fs.DirEntry) bool {
		return !e.IsDir()
	})
	if err != nil {
		return 0, fmt.Errorf("list %s bundles: %w", prefix, err)
	}

	highest := 0

	for _, name := range names {
		gen := parseGeneration(name, prefix)
		if gen > highest {
			highest = gen
		}
	}

	return highest + 1, nil
}

// parseGeneration extracts the generation number from a bundle filename shaped
// "<prefix>.gen<NNNN>.zip" or its sidecar "<prefix>.gen<NNNN>.zip.sidecar.ndjson",
// or 0 when it matches neither.
func parseGeneration(name, prefix string) int {
	rest, ok := strings.CutPrefix(name, prefix+".gen")
	if !ok {
		return 0
	}

	digits, ok := strings.CutSuffix(rest, ".zip"+seal.SidecarSuffix)
	if !ok {
		digits, ok = strings.CutSuffix(rest, ".zip")
		if !ok {
			return 0
		}
	}

	gen, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}

	return gen
}

// sortMembers orders members by name so a bundle's contents are deterministic.
func sortMembers(members []seal.Member) {
	slices.SortFunc(members, func(a, b seal.Member) int {
		return strings.Compare(a.Name, b.Name)
	})
}
