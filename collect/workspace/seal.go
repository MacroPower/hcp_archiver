package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// run.json is absent, being mutable, and stays loose.
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

// SealWorkspace packs the workspace's frozen cold artifacts into bundles and
// roll-ups beside its loose metadata, then removes the loose originals.
//
// The heavy, audit-only run artifacts (plan and apply logs, the structured plan
// JSON, policy-check logs) seal into a deflated logs bundle; the raw and
// JSON-format state blobs seal into a stored state bundle, kept uncompressed so
// the irreplaceable state stays greppable on disk; and the immutable small
// metadata (the per-run children and per-state-version meta sidecars) coalesces
// into per-workspace NDJSON roll-ups. Only run.json, the mutable run summary,
// stays a loose file. Only settled artifacts of a fully-walked collection are
// sealed, and each loose source is removed only after its bundle or roll-up is
// durable and verified, so a pass is safe to interrupt and re-run. Nothing is
// re-fetched: the ledger keys are unchanged, so a re-run treats a sealed object
// exactly as it did the loose one.
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

	return c.coalesce(ctx, project, ws, metadata)
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

// frozenMetadata returns the workspace's loose, settled immutable metadata
// grouped by the roll-up it coalesces into: the immutable per-run children and
// the per-state-version meta sidecars, excluding the mutable run.json, which
// stays loose. Each collection contributes only once it has been walked to its
// end.
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

// settled reports whether the object at relPath is recorded done, the gate that
// keeps a seal to artifacts the ledger has fully archived.
func (c *Collector) settled(relPath string) bool {
	entry, ok := c.env.Entry(relPath)

	return ok && entry.Status == manifest.StatusDone
}

// sealBundle seals members into the next-generation bundle named by prefix, a
// no-op when nothing is frozen to seal.
func (c *Collector) sealBundle(ctx context.Context, project, ws, prefix string, members []seal.Member) error {
	if len(members) == 0 {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("seal %s bundle: %w", prefix, ctx.Err())
	}

	bundlesDir := c.env.Store().AbsPath(c.env.Store().BundleDir(project, ws))

	gen, err := nextGeneration(bundlesDir, prefix)
	if err != nil {
		return err
	}

	bundlePath := filepath.Join(bundlesDir, fmt.Sprintf("%s.gen%04d.zip", prefix, gen))

	_, err = seal.Seal(bundlePath, members)
	if err != nil {
		return fmt.Errorf("seal %s bundle: %w", prefix, err)
	}

	return nil
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
// "<prefix>.gen<NNNN>.zip", or 0 when it does not match.
func parseGeneration(name, prefix string) int {
	rest, ok := strings.CutPrefix(name, prefix+".gen")
	if !ok {
		return 0
	}

	digits, ok := strings.CutSuffix(rest, ".zip")
	if !ok {
		return 0
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
