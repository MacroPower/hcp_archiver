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
// and sealed keys), and each run.json is read with [Workspace.Open]'s
// precedence, so an in-flight run's loose summary and a terminal run's roll-up
// line list together. A run whose run.json is missing or malformed still lists
// by id, with its parsed fields zero, so one damaged file or roll-up line does
// not hide the run.
func (w *Workspace) Runs() ([]Run, error) {
	runsDir := path.Join(w.dir, "runs")

	idx, err := w.indexRuns()
	if err != nil {
		return nil, err
	}

	runs := make([]Run, 0, len(idx.ids))

	for _, id := range idx.ids {
		run := Run{ID: id}

		// Only a run the listing named can hold a loose run.json; for the rest
		// the probe is a certain miss, and there is one per rolled-up run.
		data, readErr := w.openListed(path.Join(runsDir, id, "run.json"), idx.listed[id])
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

// runIDs enumerates the workspace's archived run ids, the enumeration behind
// both the run list and the run count, so a fully coalesced workspace (no
// runs/ directory at all) counts the same runs it lists. See
// [Workspace.indexRuns], which answers it.
func (w *Workspace) runIDs() ([]string, error) {
	idx, err := w.indexRuns()
	if err != nil {
		return nil, err
	}

	return idx.ids, nil
}

// runIndex is one enumeration of a workspace's runs, everything a caller can
// learn about them from a single listing of runs/ and a single pass over the
// sealed keys beneath it.
//
// It is built per call and retained by nobody: the browser re-reads the tree
// on every screen push by design, so what is short-lived here stays
// short-lived (only the sealed index itself is memoized, per workspace
// handle). Instances are produced by [*Workspace.indexRuns].
type runIndex struct {
	// The run ids the merged listing named. The field is deliberately not
	// called "loose": the listing is the local directory unioned with the
	// organization's mirror, so a run only the mirror holds is listed too.
	// Narrowing it to local names would drop a mirror-only run's artifacts
	// outright, because the listing this gates has no read-through behind it
	// the way a read does.
	listed map[string]bool
	// Each run's sealed leaf names, derived the way [Workspace.mergedLeafForms]
	// derives them, so a batched artifact listing names exactly what a per-run
	// listing does. The two merge loose and sealed leaves in separate places
	// and must keep agreeing; TestAllRunArtifactsMatchesPerRun holds them to
	// it, over a run whose directory sealing removed.
	sealed map[string][]string
	// Every run id across both forms, sorted and deduplicated.
	ids []string
}

// indexRuns enumerates the workspace's archived runs across both physical
// forms: the runs/<id>/ directories the merged listing names and the first path
// segment of every sealed key under runs/, deduplicated. Any sealed child keeps
// its run visible, so a run whose own roll-up line is the corrupt one still
// lists.
//
// It is the one place the runs directory is enumerated. A caller that would
// otherwise ask the filesystem again about a run it has already seen here (does
// this run hold a loose run.json, what leaves does it hold) reads the answer
// off the index instead.
func (w *Workspace) indexRuns() (runIndex, error) {
	runsDir := path.Join(w.dir, "runs")

	ids, err := w.org.subdirs(runsDir)
	if err != nil {
		return runIndex{}, fmt.Errorf("list runs: %w", err)
	}

	idx := runIndex{
		listed: make(map[string]bool, len(ids)),
		sealed: make(map[string][]string),
	}

	for _, id := range ids {
		idx.listed[id] = true
	}

	sealed, err := w.sealedNames(runsDir)
	if err != nil {
		return runIndex{}, err
	}

	for _, relPath := range sealed {
		// A sealed key is archive-relative: trim the runs dir to expose the
		// run id as the first remaining segment.
		rest := strings.TrimPrefix(relPath, runsDir+"/")

		id, _, hasLeaf := strings.Cut(rest, "/")
		if id == "" {
			continue
		}

		ids = append(ids, id)

		// A key naming the run directory itself (a corrupt record) keeps the
		// run visible but contributes no leaf inside it.
		if hasLeaf {
			idx.sealed[id] = append(idx.sealed[id], path.Base(relPath))
		}
	}

	idx.ids = dedupe(ids)

	return idx, nil
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

	names, err := w.mergedLeafNames(runDir)
	if err != nil {
		return nil, fmt.Errorf("list run artifacts: %w", err)
	}

	return artifactPaths(runDir, names), nil
}

// AllRunArtifacts returns every run's artifact leaves keyed by run id, each in
// the same reading order [Workspace.RunArtifacts] gives that run, for a caller
// rendering the whole history at once. Every id this call's own enumeration
// names is keyed, holding an empty slice rather than being absent when the run
// has no artifacts. [Workspace.Runs] enumerates separately, so a run that
// appears between the two calls is listed by one and absent from the other.
//
// It costs one listing of runs/ and one listing per run that listing named,
// while calling [Workspace.RunArtifacts] per run costs one listing per run in
// the archive: a run whose directory sealing removed has nothing left to list,
// so its loose listing is a certain miss. Both project their leaves through
// the same tail, so the run.json exclusion and the reading order cannot drift
// apart.
func (w *Workspace) AllRunArtifacts() (map[string][]string, error) {
	idx, err := w.indexRuns()
	if err != nil {
		return nil, err
	}

	all := make(map[string][]string, len(idx.ids))

	for _, id := range idx.ids {
		runDir := path.Join(w.dir, "runs", id)
		names := slices.Clone(idx.sealed[id])

		if idx.listed[id] {
			loose, looseErr := w.org.looseNames(runDir)
			if looseErr != nil {
				return nil, fmt.Errorf("list run artifacts: %w", looseErr)
			}

			names = append(names, loose...)
		}

		all[id] = artifactPaths(runDir, dedupe(names))
	}

	return all, nil
}

// artifactPaths projects one run's merged leaf names onto archive-relative
// paths in reading order, dropping the run's own summary. It is the tail
// [Workspace.RunArtifacts] and [Workspace.AllRunArtifacts] share.
func artifactPaths(runDir string, names []string) []string {
	names = slices.DeleteFunc(names, func(name string) bool {
		return name == "run.json"
	})

	slices.SortFunc(names, cmpArtifact)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, path.Join(runDir, name))
	}

	return paths
}

// StateVersions returns the workspace's archived state versions, newest first,
// gathering the meta.json sidecars from both their loose and rolled-up forms.
// A version whose sidecar is missing or malformed still lists by stem, with
// its parsed fields zero.
func (w *Workspace) StateVersions() ([]StateVersion, error) {
	svDir := path.Join(w.dir, "state-versions")

	forms, err := w.mergedLeafForms(svDir)
	if err != nil {
		return nil, fmt.Errorf("list state versions: %w", err)
	}

	stems := stateStems(forms.names())

	var versions []StateVersion

	for _, stem := range stems {
		versions = append(versions, w.stateVersion(svDir, stem, forms))
	}

	slices.SortFunc(versions, func(a, b StateVersion) int {
		return strings.Compare(b.Stem, a.Stem)
	})

	return versions, nil
}

// StateVersionNames returns the stems of the workspace's archived state
// versions, gathered from both loose files and sealed bundles, without opening
// or decoding any meta sidecar. It is the cheap enumeration behind a count,
// mirroring how the run count enumerates run ids rather than materializing
// every version just to size the list.
func (w *Workspace) StateVersionNames() ([]string, error) {
	svDir := path.Join(w.dir, "state-versions")

	names, err := w.mergedLeafNames(svDir)
	if err != nil {
		return nil, fmt.Errorf("list state versions: %w", err)
	}

	return stateStems(names), nil
}

// stateStems picks the state-version stems out of a directory's leaf names:
// one per metadata sidecar, which every archived version has whatever form its
// blobs are in.
func stateStems(names []string) []string {
	var stems []string

	for _, name := range names {
		if !strings.HasSuffix(name, metaSuffix) {
			continue
		}

		stems = append(stems, strings.TrimSuffix(name, metaSuffix))
	}

	return stems
}

// stateVersion builds one state version's summary from its meta sidecar and
// the presence of its sibling blobs, both answered from the leaf forms the
// caller already listed rather than probed for again: the sidecar's loose read
// is skipped unless the listing named it loose, and each blob's presence is
// the listing's answer instead of a stat.
//
// A listing and a [Workspace.Exists] probe answer "is it there" by different
// routes, so a small residue divides them, in each case the listing being the
// more forgiving:
//
//   - A per-file stat fault degrades to a listed leaf of zero size rather than
//     failing the call, matching the recursive loose listing's per-file
//     resilience, which differs only in dropping an entry that vanishes
//     mid-listing where this one keeps it. A directory-level read fault still
//     fails the call.
//   - A dangling symlink at a blob's path counts as present, since the listing
//     reads entry types rather than following the link.
//   - An eviction stub folds onto its target in the listing but answers absent
//     to a probe with no remote configured. No stub reaches here: only bundle
//     zips and configuration-version tarballs are evictable, so none can land
//     beside a state version. It is worth re-checking if that set ever grows.
func (w *Workspace) stateVersion(svDir, stem string, forms leafForms) StateVersion {
	sv := StateVersion{Stem: stem}
	meta := stem + metaSuffix

	data, err := w.openListed(path.Join(svDir, meta), forms.listed[meta])
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

	sv.HasRaw = forms.has(stem + ".tfstate.json")
	sv.HasJSON = forms.has(stem + ".json")

	return sv
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

// mergedLeafNames returns the deduplicated leaf names directly under a
// workspace-scoped directory across both physical forms. Callers project and
// order the names for their own presentation; one that also needs to know
// which form named a leaf takes [Workspace.mergedLeafForms] instead.
func (w *Workspace) mergedLeafNames(relDir string) ([]string, error) {
	forms, err := w.mergedLeafForms(relDir)
	if err != nil {
		return nil, err
	}

	return forms.names(), nil
}

// leafForms records a directory's leaves by the form each was named in:
// listed holds what the merged listing answered (the clean merged tree,
// machinery hidden, a local file or a mirror record alike), sealed the base
// name of every sealed key there. A name in both is a loose survivor of an
// interrupted seal, which the loose-wins rule keeps canonical.
//
// Instances are produced by [*Workspace.mergedLeafForms] and die with the call:
// the browser re-reads a directory on every screen push by design, and nothing
// here is a cache of what the tree held a moment ago.
type leafForms struct {
	listed map[string]bool
	sealed map[string]bool
}

// has reports whether a leaf is present in either form.
func (f leafForms) has(name string) bool {
	return f.listed[name] || f.sealed[name]
}

// names returns the deduplicated leaf names across both forms.
func (f leafForms) names() []string {
	names := make([]string, 0, len(f.listed)+len(f.sealed))

	for name := range f.listed {
		names = append(names, name)
	}

	for name := range f.sealed {
		names = append(names, name)
	}

	return dedupe(names)
}

// mergedLeafForms returns the leaf names directly under a workspace-scoped
// directory across both physical forms: the leaves the merged listing names
// (see [*Org.looseNames]) and the base name of every sealed key there.
func (w *Workspace) mergedLeafForms(relDir string) (leafForms, error) {
	names, err := w.org.looseNames(relDir)
	if err != nil {
		return leafForms{}, err
	}

	sealed, err := w.sealedNames(relDir)
	if err != nil {
		return leafForms{}, err
	}

	forms := leafForms{
		listed: make(map[string]bool, len(names)),
		sealed: make(map[string]bool, len(sealed)),
	}

	for _, name := range names {
		forms.listed[name] = true
	}

	for _, relPath := range sealed {
		forms.sealed[path.Base(relPath)] = true
	}

	return forms, nil
}
