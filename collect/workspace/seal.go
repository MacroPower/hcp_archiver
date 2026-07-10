package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/seal"
)

// runHeavyNames are the fixed-name heavy, audit-only run artifacts that seal into
// a workspace's logs bundle; per-check policy-check-<id>.log files join them by
// pattern.
var runHeavyNames = map[string]struct{}{
	"plan.log":          {},
	"plan.json":         {},
	"apply.log":         {},
	"cost-estimate.log": {},
}

// SealWorkspace packs the workspace's frozen cold artifacts into generational zip
// bundles beside its loose metadata, then removes the loose originals.
//
// The heavy, audit-only run artifacts (plan and apply logs, the structured plan
// JSON, policy-check logs) seal into a deflated logs bundle; the raw and
// JSON-format state blobs seal into a stored state bundle, kept uncompressed so
// the irreplaceable state stays greppable on disk. Only settled artifacts of a
// fully-walked collection are sealed, and each loose source is removed only after
// its bundle verifies, so a sealing pass is safe to interrupt and re-run. The
// greppable metadata (run.json, the state meta sidecar, the small run children)
// stays loose, and nothing is re-fetched: the ledger keys are unchanged, so a
// re-run treats a sealed object exactly as it did the loose one.
func (c *Collector) SealWorkspace(ctx context.Context, project, ws string) error {
	logs, err := c.frozenRunArtifacts(project, ws)
	if err != nil {
		return fmt.Errorf("gather frozen run artifacts: %w", err)
	}

	states, err := c.frozenStateArtifacts(project, ws)
	if err != nil {
		return fmt.Errorf("gather frozen state artifacts: %w", err)
	}

	err = c.sealBundle(ctx, project, ws, "logs", logs)
	if err != nil {
		return err
	}

	return c.sealBundle(ctx, project, ws, "state", states)
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

// subdirs lists the immediate subdirectory names of dir, tolerating a dir that
// does not exist (a workspace with no runs).
func subdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var out []string

	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}

	return out, nil
}

// heavyRunFiles lists the heavy, audit-only artifact filenames present in a run
// directory.
func heavyRunFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var out []string

	for _, e := range entries {
		if !e.IsDir() && isHeavyRunFile(e.Name()) {
			out = append(out, e.Name())
		}
	}

	return out, nil
}

// isHeavyRunFile reports whether a run-directory filename is a heavy, audit-only
// artifact that seals rather than staying loose.
func isHeavyRunFile(name string) bool {
	if _, ok := runHeavyNames[name]; ok {
		return true
	}

	return strings.HasPrefix(name, "policy-check-") && strings.HasSuffix(name, ".log")
}

// stateBlobFiles lists the raw and JSON-format state blobs in a state-versions
// directory, leaving the meta sidecars loose.
func stateBlobFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var out []string

	for _, e := range entries {
		if !e.IsDir() && isStateBlob(e.Name()) {
			out = append(out, e.Name())
		}
	}

	return out, nil
}

// isStateBlob reports whether a state-versions filename is a raw or JSON-format
// state blob, the heavy artifacts that seal; the meta sidecar stays loose.
func isStateBlob(name string) bool {
	if strings.HasSuffix(name, ".meta.json") {
		return false
	}

	return strings.HasSuffix(name, ".tfstate.json") || strings.HasSuffix(name, ".json")
}

// nextGeneration returns the generation number for a new bundle named by prefix,
// one past the highest already present, so a bundle is written once and never
// rewritten.
func nextGeneration(bundlesDir, prefix string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(bundlesDir, prefix+".gen*.zip"))
	if err != nil {
		return 0, fmt.Errorf("glob %s bundles: %w", prefix, err)
	}

	highest := 0

	for _, match := range matches {
		gen := parseGeneration(filepath.Base(match), prefix)
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
