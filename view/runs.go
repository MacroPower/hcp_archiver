package view

import (
	"fmt"
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

var (
	// RunArtifacts lists a run's artifact leaves in the order an operator reads
	// them (execution output first, then the structured plan, then the
	// surrounding metadata), each paired with the description its list row
	// shows. Both runArtifactRank and artifactDesc derive from this one table,
	// so an artifact's ordering and its label cannot drift apart. A leaf absent
	// here (a per-check policy log, a future leaf) sorts after these
	// alphabetically and falls back to its filename.
	runArtifacts = []struct {
		name string
		desc string
	}{
		{"plan.log", "plan output"},
		{"plan.json", "structured plan (JSON)"},
		{"apply.log", "apply output"},
		{"cost-estimate.log", "cost estimate breakdown"},
		{"config-version.json", "configuration version and ingress attributes"},
		{"cost-estimate.json", "cost estimate"},
		{"comments.json", "run comments"},
		{"run-events.json", "actor-attributed event timeline"},
		{"policy-checks.json", "Sentinel policy checks"},
		{"task-stages.json", "task stages and results"},
		{"tf-policy-outcomes.json", "native Terraform policy outcomes"},
		{"run.history.ndjson", "superseded run.json versions and tombstones"},
	}

	// RunArtifactRank maps each ranked artifact leaf to its reading order,
	// derived from runArtifacts.
	runArtifactRank = func() map[string]int {
		m := make(map[string]int, len(runArtifacts))
		for i, a := range runArtifacts {
			m[a.name] = i + 1
		}

		return m
	}()

	// ArtifactDesc names each run artifact for its list row, derived from
	// runArtifacts; an unrecognized leaf falls back to its filename.
	artifactDesc = func() map[string]string {
		m := make(map[string]string, len(runArtifacts))
		for _, a := range runArtifacts {
			m[a.name] = a.desc
		}

		return m
	}()
)

// Runs returns the workspace's archived runs, newest first.
//
// Run ids are enumerated across both physical forms (loose run directories
// and sealed keys), and each run.json is read through [Workspace.Open], so an
// in-flight run's loose summary and a terminal run's roll-up line list
// together. A run whose run.json is missing or malformed still lists by id,
// with its parsed fields zero, so one damaged file or roll-up line does not
// hide the run.
func (w *Workspace) Runs() ([]Run, error) {
	runsDir := path.Join(w.dir, "runs")

	ids, err := w.runIDs()
	if err != nil {
		return nil, err
	}

	runs := make([]Run, 0, len(ids))

	for _, id := range ids {
		run := Run{ID: id}

		data, readErr := w.Open(path.Join(runsDir, id, "run.json"))
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

// runIDs enumerates the workspace's archived run ids across both physical
// forms: the loose runs/<id>/ directories and the first path segment of every
// sealed key under runs/, deduplicated. Any sealed child keeps its run
// visible, so a run whose own roll-up line is the corrupt one still lists. It
// is the one enumeration behind both the run list and the run count, so a
// fully coalesced workspace (no runs/ directory at all) counts the same runs
// it lists.
func (w *Workspace) runIDs() ([]string, error) {
	runsDir := path.Join(w.dir, "runs")

	ids, err := w.org.subdirs(runsDir)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	sealed, err := w.sealedNames(runsDir)
	if err != nil {
		return nil, err
	}

	for _, relPath := range sealed {
		// A sealed key is archive-relative: trim the runs dir to expose the
		// run id as the first remaining segment.
		rest := strings.TrimPrefix(relPath, runsDir+"/")

		id, _, _ := strings.Cut(rest, "/")
		if id != "" {
			ids = append(ids, id)
		}
	}

	return dedupe(ids), nil
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

	names, err := w.org.looseNames(runDir)
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

	names, err := w.org.looseNames(svDir)
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
