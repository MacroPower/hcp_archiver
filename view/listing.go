package view

import (
	"encoding/json"
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
	FormLoose  Form = "loose"  // standalone object, present or evicted
	FormRollup Form = "rollup" // one line of a workspace's NDJSON roll-up
	FormBundle Form = "bundle" // member of a sealed zip bundle
)

// Entry is one archived object as an operator addresses it.
//
// Instances are produced by [*Org.List] and [*Archive.List].
type Entry struct {
	// ModTime is the loose file's modification time; zero for a sealed object
	// and for an evicted one, neither of which has a local file to carry it.
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
	// Offloaded reports an object whose bytes are only in the remote store,
	// so reading it needs network access: a bundle member is fetched from its
	// remote bundle with ranged reads, a configuration-version tarball is
	// downloaded whole, and a warm object the mirror holds but the local tree
	// does not yet is fetched and persisted locally by the read itself. None
	// is reachable in an organization that records no mirror (see
	// [*Org.HasRemote]), where an unseal counts the object as one it could
	// not recover.
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
// ledger shards, the remote marker, identity sidecars, eviction stubs, staging
// leftovers) never appearing. An object evicted to the remote store is still
// listed, from whatever the eviction left behind: a bundle member from its
// sidecar, a configuration-version tarball from its stub, both flagged
// Offloaded. An empty prefix lists the whole organization; a prefix matching
// nothing returns an empty listing without error; an unclean prefix (absolute,
// or carrying ".." segments) returns [ErrInvalidPath].
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

	entries = o.appendRemoteEntries(entries, seen, rel)

	slices.SortFunc(entries, func(a, b Entry) int {
		return strings.Compare(a.Path, b.Path)
	})

	return entries, nil
}

// appendRemoteEntries appends one entry per object the organization's mirror
// holds at or beneath prefix that nothing local already claims, so a listing
// over a partly or wholly absent tree is still the whole archive. Machinery
// keys are hidden exactly as local machinery is; a stub key is skipped as
// belt-and-braces (stubs are never mirrored, and the tarball one stands for
// lists from its own key). The entries are flagged Offloaded: their bytes are
// only in the mirror until a read pulls them down.
func (o *Org) appendRemoteEntries(entries []Entry, seen map[string]struct{}, prefix string) []Entry {
	for rel, info := range o.remoteKeysUnder(prefix) {
		if _, ok := seen[rel]; ok {
			continue
		}

		if isMachinery(rel) {
			continue
		}

		if _, ok := store.RemoteStubTarget(rel); ok {
			continue
		}

		seen[rel] = struct{}{}

		entries = append(entries, Entry{
			Org:       o.Name,
			Path:      rel,
			Form:      FormLoose,
			Size:      info.Size,
			Offloaded: true,
		})
	}

	return entries
}

// Read returns the bytes at an archive-relative path, whichever physical form
// holds it. A workspace-scoped path resolves through [Workspace.Open]; any
// other path is a loose read, and a loose object absent locally is fetched
// from the organization's mirror when it has one, persisting at the same path
// so the next read is local. A path present in no form returns
// [ErrObjectNotFound]; a directory returns [ErrNotFile], whether it is
// physically present or survives only as sealed objects beneath the path (a
// fully sealed run directory). A configuration-version tarball evicted to the
// mirror returns [ErrRemoteOnly], naming the mirrored key when the
// organization records where its mirror is, since the archive holds that
// object elsewhere rather than not at all.
//
// Unlike [*Org.List], Read applies no machinery filter: a physical file the
// listing hides (the remote marker, an identity sidecar, an eviction stub, a
// roll-up, a bundle sidecar) is still readable by its real path, which is
// useful for inspecting the archive's own files.
func (o *Org) Read(relPath string) ([]byte, error) {
	rel, err := o.resolveRel(relPath)
	if err != nil {
		return nil, err
	}

	if ws := o.workspaceFor(rel); ws != nil {
		return readWorkspacePath(ws, rel)
	}

	data, err := os.ReadFile(o.AbsPath(rel))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return o.readThrough(rel, o.describeMiss(rel))
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", rel, err)
	}

	return data, nil
}

// resolveRel narrows a caller-supplied archive path to the archive-relative
// path of one object: an unclean path is refused with [ErrInvalidPath], and
// both the empty path (the whole scope) and a physically present directory with
// [ErrNotFile].
//
// It is the prologue [*Org.Read] and [*Org.writeObject] share, so a path that
// names no single object is refused identically whichever shape the caller
// asked its bytes in. Only a directory that physically exists is caught here; a
// directory whose physical form sealing removed exists in the logical tree
// alone, which only the owning workspace's index can see (see
// [Workspace.missOrDir]).
func (o *Org) resolveRel(relPath string) (string, error) {
	rel, err := cleanRel(relPath)
	if err != nil {
		return "", err
	}

	if rel == "" {
		return "", fmt.Errorf("%w: %q", ErrNotFile, relPath)
	}

	info, err := os.Stat(o.AbsPath(rel))
	if err == nil && info.IsDir() {
		return "", fmt.Errorf("%w: %s is a directory", ErrNotFile, rel)
	}

	// A directory the mirror holds but the local tree does not yet is a
	// directory all the same, so a bootstrap archive answers exactly what a
	// fully local one would.
	if errors.Is(err, fs.ErrNotExist) && o.remoteDirExists(rel) {
		return "", fmt.Errorf("%w: %s is a directory", ErrNotFile, rel)
	}

	return rel, nil
}

// describeMiss names what an absent file means: an eviction stub beside it
// says the object was moved to the mirror whole, which is a different answer
// than never having been archived, and the one an operator can act on. The
// mirrored key is named when the organization records where its mirror is,
// since fetching the object directly is the only way to read it. A bootstrap
// archive holds no local stub, so the mirror's own inventory stands in for
// one: a tarball the mirror lists is just as evicted, only proven from the
// other side.
//
// A stub's mere existence is enough here, where a listing insists on parsing
// one first. A read quotes no size it could get wrong, so a stub too damaged
// to list is still evidence that eviction happened, and saying so beats
// sending an operator to look for a collection bug.
func (o *Org) describeMiss(rel string) error {
	if !store.IsConfigTarball(rel) {
		return fmt.Errorf("%w: %s", ErrObjectNotFound, rel)
	}

	_, err := os.Stat(o.AbsPath(store.RemoteStubPath(rel)))
	if err != nil {
		if _, ok := o.remoteHas(rel); !ok {
			return fmt.Errorf("%w: %s", ErrObjectNotFound, rel)
		}
	}

	if o.remote == nil {
		return fmt.Errorf("%w: %s was evicted to the remote store", ErrRemoteOnly, rel)
	}

	return fmt.Errorf("%w: %s was evicted to the remote store; fetch it from %q",
		ErrRemoteOnly, rel, o.remote.cfg.Key(o.Name, rel))
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

	return nil, ws.missOrDir(rel, err)
}

// missOrDir upgrades a workspace miss over a logical directory to [ErrNotFile],
// returning miss unchanged for a path that names no object and holds none
// beneath it either. See [readWorkspacePath] for why the two answers differ,
// and [Workspace.writeObject], which shares the rule so a stream and a read
// describe the same path the same way.
func (w *Workspace) missOrDir(rel string, miss error) error {
	sealed, sealedErr := w.sealedUnder(rel)
	if sealedErr != nil {
		return miss
	}

	for _, se := range sealed {
		if se.rel != rel {
			return fmt.Errorf("%w: %s holds archived objects", ErrNotFile, rel)
		}
	}

	return miss
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

// looseEntries builds the entries the filesystem alone accounts for at or
// beneath an archive-relative prefix (the loose files, plus the objects an
// eviction stub stands in for) and the set of paths they claim, which the
// sealed pass dedups against.
func (o *Org) looseEntries(prefix string) ([]Entry, map[string]struct{}, error) {
	loose, stubs, err := o.looseFiles(prefix)
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

	return o.stubEntries(entries, seen, stubs, prefix), seen, nil
}

// stubEntries appends one entry per eviction stub, standing in for the object
// whose bytes live only in the remote store. A stub whose object is
// present locally is ignored: the file is authoritative and the stub is a
// leftover, the same precedence a loose file has over its sealed copy.
//
// An accepted stub claims its target in seen, so the sealed pass dedups
// against it exactly the way it does against a loose file.
//
// A stub [*Org.readRemoteStub] refuses, for any of the grounds it names, is
// skipped rather than fatal, matching how a stat fault drops one loose file
// instead of failing the listing. The listing is then short by that object,
// which is what it would be with no stub at all.
//
// The prefix filter guards one case: a prefix naming a stub file resolves to
// the sibling object beside it, which sits outside the requested scope and
// must not be emitted.
func (o *Org) stubEntries(entries []Entry, seen map[string]struct{}, stubs []string, prefix string) []Entry {
	for _, rel := range stubs {
		target, ok := store.RemoteStubTarget(rel)
		if !ok || !underPrefix(target, prefix) {
			continue
		}

		if _, claimed := seen[target]; claimed {
			continue
		}

		stub, ok := o.readRemoteStub(rel)
		if !ok {
			continue
		}

		seen[target] = struct{}{}

		entries = append(entries, Entry{
			Org:       o.Name,
			Path:      target,
			Form:      FormLoose,
			Size:      stub.Size,
			Offloaded: true,
		})
	}

	return entries
}

// readRemoteStub reads one eviction stub, reporting whether it can be rendered
// truthfully. It is refused when it cannot be read or parsed; when its version
// is newer than this build writes, so its fields cannot be trusted to mean what
// they mean here; when its version is below 1, which no eviction ever wrote,
// so the field was left unset or set to nonsense; and when it records a
// negative size, which is not a length anything can have. A size of zero is
// legal and listed as such: the writer records whatever the object measured,
// and refusing it here would drop a genuinely empty object from listings, the
// silent absence stubs exist to prevent.
//
// A refusal carries no reason because there is nowhere to carry one to: the
// browser holds no logger, and the caller drops the object either way, exactly
// as a stat fault drops one loose file. Rendering the stub anyway would be
// worse than an unlisted object, since a wrong size is a figure an operator
// reads, compares, and plans a restore against.
func (o *Org) readRemoteStub(rel string) (store.RemoteStub, bool) {
	data, err := os.ReadFile(o.AbsPath(rel))
	if err != nil {
		return store.RemoteStub{}, false
	}

	var stub store.RemoteStub

	err = json.Unmarshal(data, &stub)
	if err != nil {
		return store.RemoteStub{}, false
	}

	if stub.Version < 1 || stub.Version > store.RemoteStubVersion {
		return store.RemoteStub{}, false
	}

	if stub.Size < 0 {
		return store.RemoteStub{}, false
	}

	return stub, true
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
// the sealed-index keys, and separately the eviction stubs found alongside
// them. Machinery is skipped (see [isMachinery]); the walk goes through
// [fsid.WalkFiles], which owns the archive's symlink-aliasing rules and
// handles a file root, so a prefix naming one loose file lists it.
//
// Stubs come back separately because they are neither content nor ordinary
// machinery: a stub is never listed as itself, but it is the only local
// evidence of the object it stands in for, which the caller turns into that
// object's entry.
//
// The stored browse context is the intended parent: the browser plans inside
// tea.Cmds, which carry no context of their own, and a command-line caller
// installs its own with [WithContext].
//
//nolint:contextcheck // See above; there is no caller context to pass.
func (o *Org) looseFiles(relDir string) ([]string, []string, error) {
	var rels, stubs []string

	_, err := fsid.WalkFiles(o.context(), o.AbsPath(relDir), func(logical string) error {
		rel, relErr := filepath.Rel(o.root, logical)
		if relErr != nil {
			return fmt.Errorf("relativize %q: %w", logical, relErr)
		}

		slashed := filepath.ToSlash(rel)

		if _, ok := store.RemoteStubTarget(slashed); ok {
			stubs = append(stubs, slashed)

			return nil
		}

		if isMachinery(slashed) {
			return nil
		}

		rels = append(rels, slashed)

		return nil
	})

	switch {
	// A scope with no loose files at all (a fully coalesced workspace whose
	// runs/ directory sealing removed) is empty, not an error. The absent path
	// may also be an evicted object addressed directly, which is the natural
	// thing to type after a read reports it remote-only, so its stub is looked
	// for before the scope is called empty.
	case errors.Is(err, fs.ErrNotExist):
		return nil, o.rootStub(relDir), nil
	case err != nil:
		return nil, nil, fmt.Errorf("walk %q: %w", relDir, err)
	}

	return rels, stubs, nil
}

// rootStub returns the eviction stub standing in for relDir itself, for a walk
// root that resolved to nothing. Only an evicted object leaves one, so any
// other absent path yields none.
func (o *Org) rootStub(relDir string) []string {
	if !store.IsConfigTarball(relDir) {
		return nil
	}

	stub := store.RemoteStubPath(relDir)

	_, err := os.Stat(o.AbsPath(stub))
	if err != nil {
		return nil
	}

	return []string{stub}
}

// isMachinery reports whether a slash-separated archive-relative path names
// the archive's own machinery rather than archived content: anything inside a
// workspace's sealed-form directory ([store.InSealedForm]), a
// [manifest.LedgerDirName] bookkeeping directory, the org-root remote marker,
// a directory's [store.IdentityFileName] sidecar, or an atomic writer's
// staging leftover. Listings hide machinery and unseals skip it; [*Org.Read]
// deliberately does not filter on it.
//
// The sealed-form match is positional: only the bundles/ and rollups/
// directories sitting directly under a workspace level count, so a project or
// workspace whose own display name is "bundles" or "rollups" keeps its
// content listed. The ledger directory needs no such care, since its leading
// dot cannot appear in an upstream display name; it is matched at any depth.
//
// The identity sidecar sits beside archived objects rather than in a
// directory of its own, so it is matched by name at any depth: the collector
// stamps one into every project, workspace, and stack directory to bind the
// name-keyed directory to the id it archives, which is bookkeeping about the
// archive rather than anything the archive collected.
//
// One shape of machinery is deliberately not matched here. An eviction stub is
// hidden too, but [*Org.looseFiles] diverts it before this filter, because it
// does not merely disappear: it becomes the entry for the object it stands in
// for.
func isMachinery(rel string) bool {
	if store.InSealedForm(rel) {
		return true
	}

	for seg := range strings.SplitSeq(rel, "/") {
		if seg == manifest.LedgerDirName {
			return true
		}
	}

	base := path.Base(rel)

	return base == remote.MarkerName || base == store.IdentityFileName || atomicfile.IsTemp(base)
}
