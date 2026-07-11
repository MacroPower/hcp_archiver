package view

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// Run is one archived run's summary, parsed from its loose run.json.
//
// Instances are produced by [*Workspace.Runs].
type Run struct {
	CreatedAt        time.Time
	ID               string
	Status           string
	Message          string
	Source           string
	TerraformVersion string
	IsDestroy        bool
	HasChanges       bool
}

// StateVersion is one archived state version's summary, parsed from its
// meta.json sidecar in whichever physical form holds it.
//
// Instances are produced by [*Workspace.StateVersions].
type StateVersion struct {
	CreatedAt time.Time
	ID        string
	Stem      string
	Status    string
	Serial    int64
	Size      int64
	HasRaw    bool
	HasJSON   bool
}

// metaSuffix marks a state version's metadata sidecar.
const metaSuffix = ".meta.json"

// runArtifactRank orders a run's artifact leaves the way an operator reads
// them: execution output first, then the structured plan, then the surrounding
// metadata. Unranked names (per-check policy logs, future leaves) sort after
// these, alphabetically.
var runArtifactRank = map[string]int{
	"plan.log":                1,
	"plan.json":               2,
	"apply.log":               3,
	"cost-estimate.log":       4,
	"config-version.json":     5,
	"cost-estimate.json":      6,
	"comments.json":           7,
	"run-events.json":         8,
	"policy-checks.json":      9,
	"task-stages.json":        10,
	"tf-policy-outcomes.json": 11,
}

// Runs returns the workspace's archived runs, newest first.
//
// Every run keeps its run.json as a loose file (it is the one mutable per-run
// object), so the run list needs no sealed-form lookups. A run whose run.json
// is missing or malformed still lists by id, with its parsed fields zero, so
// one damaged file does not hide the run.
func (w *Workspace) Runs() ([]Run, error) {
	runsDir := path.Join(w.dir, "runs")

	ids, err := subdirNames(w.org.AbsPath(runsDir))
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	runs := make([]Run, 0, len(ids))

	for _, id := range ids {
		run := Run{ID: id}

		data, readErr := os.ReadFile(w.org.AbsPath(path.Join(runsDir, id, "run.json")))
		if readErr == nil {
			resources, decodeErr := DecodeResources(data)
			if decodeErr == nil && len(resources) == 1 {
				fillRun(&run, &resources[0])
			}
		}

		runs = append(runs, run)
	}

	slices.SortFunc(runs, func(a, b Run) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}

		return strings.Compare(a.ID, b.ID)
	})

	return runs, nil
}

// fillRun copies a run resource's display attributes onto run.
func fillRun(run *Run, r *Resource) {
	run.Status = r.String("status")
	run.Message = r.String("message")
	run.Source = r.String("source")
	run.TerraformVersion = r.String("terraform-version")
	run.CreatedAt = r.Time("created-at")
	run.IsDestroy = r.Bool("is-destroy")
	run.HasChanges = r.Bool("has-changes")
}

// RunArtifacts returns the archive-relative paths of a run's artifact leaves in
// reading order, whichever physical form each is in: loose children, rolled-up
// metadata, and bundled logs all list together. Run.json itself is excluded;
// it is the run's summary, not an artifact.
func (w *Workspace) RunArtifacts(runID string) ([]string, error) {
	runDir := path.Join(w.dir, "runs", runID)

	names, err := looseNames(w.org.AbsPath(runDir))
	if err != nil {
		return nil, fmt.Errorf("list run artifacts: %w", err)
	}

	sealed, err := w.sealedNames(runDir)
	if err != nil {
		return nil, err
	}

	for _, relPath := range sealed {
		names = append(names, path.Base(relPath))
	}

	names = dedupe(names)
	names = slices.DeleteFunc(names, func(name string) bool {
		return name == "run.json"
	})

	slices.SortFunc(names, cmpArtifact)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, path.Join(runDir, name))
	}

	return paths, nil
}

// StateVersions returns the workspace's archived state versions, newest first,
// gathering the meta.json sidecars from both their loose and rolled-up forms.
func (w *Workspace) StateVersions() ([]StateVersion, error) {
	svDir := path.Join(w.dir, "state-versions")

	stems, err := w.StateVersionNames()
	if err != nil {
		return nil, err
	}

	var versions []StateVersion

	for _, stem := range stems {
		sv, svErr := w.stateVersion(svDir, stem)
		if svErr != nil {
			return nil, svErr
		}

		versions = append(versions, sv)
	}

	slices.SortFunc(versions, func(a, b StateVersion) int {
		return strings.Compare(b.Stem, a.Stem)
	})

	return versions, nil
}

// StateVersionNames returns the stems of the workspace's archived state
// versions, gathered from both loose files and sealed bundles, without opening
// or decoding any meta sidecar. It is the cheap enumeration behind a count,
// mirroring how the run count uses subdirNames rather than materializing every
// version just to size the list.
func (w *Workspace) StateVersionNames() ([]string, error) {
	svDir := path.Join(w.dir, "state-versions")

	names, err := looseNames(w.org.AbsPath(svDir))
	if err != nil {
		return nil, fmt.Errorf("list state versions: %w", err)
	}

	sealed, err := w.sealedNames(svDir)
	if err != nil {
		return nil, err
	}

	for _, relPath := range sealed {
		names = append(names, path.Base(relPath))
	}

	var stems []string

	for _, name := range dedupe(names) {
		if !strings.HasSuffix(name, metaSuffix) {
			continue
		}

		stems = append(stems, strings.TrimSuffix(name, metaSuffix))
	}

	return stems, nil
}

// stateVersion builds one state version's summary from its meta sidecar and the
// presence of its sibling blobs.
func (w *Workspace) stateVersion(svDir, stem string) (StateVersion, error) {
	sv := StateVersion{Stem: stem}

	data, err := w.Open(path.Join(svDir, stem+metaSuffix))
	if err == nil {
		resources, decodeErr := DecodeResources(data)
		if decodeErr == nil && len(resources) == 1 {
			r := &resources[0]
			sv.ID = r.ID
			sv.Status = r.String("status")
			sv.CreatedAt = r.Time("created-at")
			sv.Serial = r.Int("serial")
			sv.Size = r.Int("size")
		}
	}

	sv.HasRaw, err = w.Exists(path.Join(svDir, stem+".tfstate.json"))
	if err != nil {
		return StateVersion{}, err
	}

	sv.HasJSON, err = w.Exists(path.Join(svDir, stem+".json"))
	if err != nil {
		return StateVersion{}, err
	}

	return sv, nil
}

// RawStatePath returns the archive-relative path of a state version's raw
// state blob.
func (w *Workspace) RawStatePath(sv *StateVersion) string {
	return path.Join(w.dir, "state-versions", sv.Stem+".tfstate.json")
}

// JSONStatePath returns the archive-relative path of a state version's
// JSON-format state blob.
func (w *Workspace) JSONStatePath(sv *StateVersion) string {
	return path.Join(w.dir, "state-versions", sv.Stem+".json")
}

// StateMetaPath returns the archive-relative path of a state version's
// metadata sidecar.
func (w *Workspace) StateMetaPath(sv *StateVersion) string {
	return path.Join(w.dir, "state-versions", sv.Stem+metaSuffix)
}

// cmpArtifact orders two artifact names by their reading rank, then
// alphabetically among the unranked.
func cmpArtifact(a, b string) int {
	ra, aOK := runArtifactRank[a]
	rb, bOK := runArtifactRank[b]

	switch {
	case aOK && bOK:
		return ra - rb
	case aOK:
		return -1
	case bOK:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// dedupe returns names sorted with duplicates removed; a loose survivor of an
// interrupted seal would otherwise list beside its sealed copy.
func dedupe(names []string) []string {
	slices.Sort(names)

	return slices.Compact(names)
}

// subdirNames returns the immediate subdirectory names of dir, tolerating a dir
// that does not exist.
func subdirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var names []string

	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}

	return names, nil
}

// looseNames returns the regular-file names directly under dir, tolerating a
// dir that does not exist.
func looseNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var names []string

	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}

	return names, nil
}
