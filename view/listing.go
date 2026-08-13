package view

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/fsid"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// Archive-layout directory names the listing walkers key on.
const (
	projectsDir   = "projects"
	workspacesDir = "workspaces"
)

// Form names the physical shape that carries an archived object's bytes.
type Form string

// The three physical forms an archived object can be held in.
const (
	FormLoose  Form = "loose"  // plain file in the archive tree
	FormRollup Form = "rollup" // one line of a workspace's NDJSON roll-up
	FormBundle Form = "bundle" // member of a sealed zip bundle
)

// Entry is one archived object as an operator addresses it.
//
// Instances are produced by [*Org.List] and [*Archive.List].
type Entry struct {
	// ModTime is the loose file's modification time; zero for a sealed object.
	ModTime time.Time
	// Org is the owning organization's name.
	Org string
	// Path is the object's archive-relative path within its organization.
	Path string
	// Container is the org-relative path of the roll-up or bundle carrying a
	// sealed object; empty for a loose file.
	Container string
	// Form is the physical shape holding the object's bytes.
	Form Form
	// Size is the object's content length in bytes, in every form.
	Size int64
	// Offloaded reports a bundle whose zip was evicted to the remote store, so
	// reading the object needs network access.
	Offloaded bool
}

// ArchivePath returns [path.Join](e.Org, e.Path), the form [*Archive.Read]
// accepts and an unseal reproduces beneath its target.
func (e Entry) ArchivePath() string {
	return path.Join(e.Org, e.Path)
}

// List returns the org's archived objects at or beneath an archive-relative
// path prefix, sorted by path.
//
// The listing is logical: one entry per object regardless of physical form,
// with a loose file winning over its sealed copy (the [Workspace.Open]
// precedence), and the archive's own machinery (roll-ups, bundles, sidecars,
// ledger shards, the remote marker, staging leftovers) never appearing. An
// empty prefix lists the whole organization; a prefix matching nothing returns
// an empty listing without error; an unclean prefix (absolute, or carrying
// ".." segments) returns [ErrInvalidPath].
func (o *Org) List(prefix string) ([]Entry, error) {
	rel, err := cleanRel(prefix)
	if err != nil {
		return nil, err
	}

	entries, seen, err := o.looseEntries(rel)
	if err != nil {
		return nil, err
	}

	workspaces, err := o.prefixWorkspaces(rel)
	if err != nil {
		return nil, err
	}

	// One offloaded probe per distinct bundle per call, shared across the
	// scoped workspaces.
	offloaded := make(map[string]bool)

	for _, ws := range workspaces {
		entries, err = o.appendSealedEntries(entries, seen, ws, rel, offloaded)
		if err != nil {
			return nil, err
		}
	}

	slices.SortFunc(entries, func(a, b Entry) int {
		return strings.Compare(a.Path, b.Path)
	})

	return entries, nil
}

// Read returns the bytes at an archive-relative path, whichever physical form
// holds it. A workspace-scoped path resolves through [Workspace.Open]; any
// other path is a loose read. A path present in no form returns
// [ErrObjectNotFound]; a directory returns [ErrNotFile], whether it is
// physically present or survives only as sealed objects beneath the path (a
// fully sealed run directory).
//
// Unlike [*Org.List], Read applies no machinery filter: a physical file the
// listing hides (the remote marker, a roll-up, a sidecar) is still readable by
// its real path, which is useful for inspecting the archive's own files.
func (o *Org) Read(relPath string) ([]byte, error) {
	rel, err := cleanRel(relPath)
	if err != nil {
		return nil, err
	}

	if rel == "" {
		return nil, fmt.Errorf("%w: %q", ErrNotFile, relPath)
	}

	info, err := os.Stat(o.AbsPath(rel))
	if err == nil && info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrNotFile, rel)
	}

	if ws := o.workspaceFor(rel); ws != nil {
		return readWorkspacePath(ws, rel)
	}

	data, err := os.ReadFile(o.AbsPath(rel))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, rel)
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", rel, err)
	}

	return data, nil
}

// readWorkspacePath reads one workspace-scoped path, upgrading a miss over a
// logical directory: a path holding no object but with sealed objects
// strictly beneath it is a directory in the logical tree whose physical form
// sealing removed, not a missing object. Only sealed keys can create such a
// directory (a loose file's parents physically exist and are caught by Read's
// stat), so the check needs just the workspace's index. An exact sealed key
// stays a miss: it names an object whose bytes are unreachable (an evicted
// bundle with no remote), not a directory. An index fault during the check
// also keeps the miss, which already tells the caller the path resolved to
// nothing.
func readWorkspacePath(ws *Workspace, rel string) ([]byte, error) {
	data, err := ws.Open(rel)
	if !errors.Is(err, ErrObjectNotFound) {
		return data, err
	}

	sealed, sealedErr := ws.sealedUnder(rel)
	if sealedErr != nil {
		return nil, err
	}

	for _, se := range sealed {
		if se.rel != rel {
			return nil, fmt.Errorf("%w: %s holds archived objects", ErrNotFile, rel)
		}
	}

	return nil, err
}

// cleanRel normalizes a caller-supplied archive-relative path or prefix:
// slashes are canonicalized, trailing separators dropped, and the empty path
// (the whole scope) is legal. Anything else must be a clean archive-relative
// path: an absolute path (including a bare "/") or one carrying "..", ".", or
// empty segments is refused with [ErrInvalidPath] rather than silently
// collapsed the way [*Org.AbsPath] confines it.
func cleanRel(p string) (string, error) {
	slashed := filepath.ToSlash(p)

	s := strings.TrimRight(slashed, "/")
	if s == "" {
		// Only a truly empty input names the whole scope; a path that was all
		// separators is the archive root spelled absolutely, not a prefix.
		if slashed != "" {
			return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
		}

		return "", nil
	}

	err := seal.ValidName(s)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidPath, err)
	}

	return s, nil
}

// looseEntries builds the loose-file entries at or beneath an archive-relative
// prefix and the set of paths they claim, which the sealed pass dedups
// against.
func (o *Org) looseEntries(prefix string) ([]Entry, map[string]struct{}, error) {
	loose, err := o.looseFiles(prefix)
	if err != nil {
		return nil, nil, err
	}

	entries := make([]Entry, 0, len(loose))
	seen := make(map[string]struct{}, len(loose))

	for _, rel := range loose {
		entry := Entry{Org: o.Name, Path: rel, Form: FormLoose}

		// Per-file resilience, matching how the sealed index skips a damaged
		// line: a file that vanished between the walk and the stat drops out,
		// and any other stat fault keeps the entry with a zero size rather
		// than failing the whole listing.
		info, statErr := os.Stat(o.AbsPath(rel))

		switch {
		case statErr == nil:
			entry.Size = info.Size()
			entry.ModTime = info.ModTime()

		case errors.Is(statErr, fs.ErrNotExist):
			continue
		}

		seen[rel] = struct{}{}

		entries = append(entries, entry)
	}

	return entries, seen, nil
}

// prefixWorkspaces returns the workspaces whose sealed objects can sit at or
// beneath an archive-relative prefix: every workspace for an org-wide or
// projects-wide prefix, one project's for a project-scoped prefix, exactly one
// for a workspace-scoped prefix, and none for a subtree (users, stacks,
// config-versions) that is never sealed.
func (o *Org) prefixWorkspaces(prefix string) ([]*Workspace, error) {
	if prefix == "" || prefix == projectsDir {
		return o.allWorkspaces()
	}

	segs := strings.Split(prefix, "/")
	if segs[0] != projectsDir {
		return nil, nil
	}

	project := segs[1]

	switch {
	case len(segs) == 2, len(segs) == 3 && segs[2] == workspacesDir:
		return o.projectWorkspaces(project)
	case len(segs) >= 4 && segs[2] == workspacesDir:
		return []*Workspace{o.Workspace(project, segs[3])}, nil
	default:
		return nil, nil
	}
}

// allWorkspaces returns every workspace in the organization.
func (o *Org) allWorkspaces() ([]*Workspace, error) {
	projects, err := o.Projects()
	if err != nil {
		return nil, err
	}

	var workspaces []*Workspace

	for _, project := range projects {
		ws, wsErr := o.projectWorkspaces(project)
		if wsErr != nil {
			return nil, wsErr
		}

		workspaces = append(workspaces, ws...)
	}

	return workspaces, nil
}

// projectWorkspaces returns one project's workspaces.
func (o *Org) projectWorkspaces(project string) ([]*Workspace, error) {
	names, err := o.Workspaces(project)
	if err != nil {
		return nil, err
	}

	workspaces := make([]*Workspace, 0, len(names))
	for _, name := range names {
		workspaces = append(workspaces, o.Workspace(project, name))
	}

	return workspaces, nil
}

// appendSealedEntries appends entries for the workspace's sealed objects at or
// beneath prefix that no loose file already claims, memoizing each distinct
// bundle's offloaded probe in offloaded across the whole listing call.
func (o *Org) appendSealedEntries(entries []Entry, seen map[string]struct{}, ws *Workspace,
	prefix string, offloaded map[string]bool,
) ([]Entry, error) {
	sealed, err := ws.sealedUnder(prefix)
	if err != nil {
		return nil, err
	}

	for _, se := range sealed {
		if _, ok := seen[se.rel]; ok {
			continue
		}

		seen[se.rel] = struct{}{}

		entry := Entry{Org: o.Name, Path: se.rel, Size: se.ref.size}

		if se.ref.rollup != "" {
			// Roll-ups are indexed by absolute path; the entry reports the
			// org-relative container so every path in a listing addresses the
			// same way.
			container, relErr := filepath.Rel(o.root, se.ref.rollup)
			if relErr != nil {
				return nil, fmt.Errorf("relativize roll-up %q: %w", se.ref.rollup, relErr)
			}

			entry.Form = FormRollup
			entry.Container = filepath.ToSlash(container)
		} else {
			entry.Form = FormBundle
			entry.Container = se.ref.bundle
			entry.Offloaded = o.bundleOffloaded(se.ref.bundle, offloaded)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// bundleOffloaded reports whether a bundle's zip is absent locally (evicted to
// the remote store), memoizing one stat per distinct bundle in memo. Only a
// definite absence reports offloaded: any other stat fault assumes the bundle
// is local, so a permission blip never misreports a listing; a later read
// surfaces the real fault.
func (o *Org) bundleOffloaded(relBundle string, memo map[string]bool) bool {
	if cached, ok := memo[relBundle]; ok {
		return cached
	}

	_, err := os.Stat(o.AbsPath(relBundle))
	gone := errors.Is(err, fs.ErrNotExist)
	memo[relBundle] = gone

	return gone
}

// workspaceFor returns the workspace owning an archive-relative path, or nil
// for a path outside any workspace subtree. Only a path strictly beneath a
// workspace directory is workspace-scoped (projects/<p>/workspaces/<w>/<leaf>
// and deeper); the workspace directory itself is not a file. The path grammar
// is owned by [*Org.prefixWorkspaces], which resolves a deep enough path to
// exactly its one workspace without touching the filesystem.
func (o *Org) workspaceFor(rel string) *Workspace {
	const scopedSeparators = 4 // projects/<p>/workspaces/<w>/<leaf> and deeper

	if strings.Count(rel, "/") < scopedSeparators {
		return nil
	}

	workspaces, err := o.prefixWorkspaces(rel)
	if err != nil || len(workspaces) != 1 {
		return nil
	}

	return workspaces[0]
}

// looseFiles returns the archive-relative (slash-separated) paths of the loose
// files at or beneath an archive-relative path, so they dedup exactly against
// the sealed-index keys. Machinery is skipped (see [isMachinery]); the walk
// goes through [fsid.WalkFiles], which owns the archive's symlink-aliasing
// rules and handles a file root, so a prefix naming one loose file lists it.
//
// The stored browse context is the intended parent: the browser plans inside
// tea.Cmds, which carry no context of their own, and a command-line caller
// installs its own with [WithContext].
//
//nolint:contextcheck // See above; there is no caller context to pass.
func (o *Org) looseFiles(relDir string) ([]string, error) {
	var rels []string

	_, err := fsid.WalkFiles(o.context(), o.AbsPath(relDir), func(logical string) error {
		rel, relErr := filepath.Rel(o.root, logical)
		if relErr != nil {
			return fmt.Errorf("relativize %q: %w", logical, relErr)
		}

		slashed := filepath.ToSlash(rel)
		if isMachinery(slashed) {
			return nil
		}

		rels = append(rels, slashed)

		return nil
	})

	switch {
	// A scope with no loose files at all (a fully coalesced workspace whose
	// runs/ directory sealing removed) is empty, not an error.
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("walk %q: %w", relDir, err)
	}

	return rels, nil
}

// isMachinery reports whether a slash-separated archive-relative path names
// the archive's own machinery rather than archived content: anything inside a
// sealed-form directory ([store.BundlesDirName], [store.RollupsDirName]), a
// [manifest.LedgerDirName] bookkeeping directory, the org-root remote marker,
// or an atomic writer's staging leftover. Listings hide machinery and unseals
// skip it; [*Org.Read] deliberately does not filter on it.
func isMachinery(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		switch seg {
		case store.BundlesDirName, store.RollupsDirName, manifest.LedgerDirName:
			return true
		}
	}

	base := path.Base(rel)

	return base == remote.MarkerName || atomicfile.IsTemp(base)
}
