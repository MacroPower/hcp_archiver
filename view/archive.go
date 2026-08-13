package view

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/remote"
)

var (
	// ErrNotArchive indicates the given path holds no archive: neither it nor
	// any of its immediate subdirectories carries an org.json.
	ErrNotArchive = errors.New("not an archive directory")

	// ErrNoOrg indicates an archive path names an organization the archive
	// does not hold.
	ErrNoOrg = errors.New("no such organization in archive")

	// ErrNotFile indicates an archive path names a directory (or a scope of
	// sealed objects) rather than a single archived file.
	ErrNotFile = errors.New("archive path is not a file")

	// ErrInvalidPath indicates a caller-supplied archive path is not a clean
	// archive-relative path: absolute, or carrying "..", ".", or empty
	// segments.
	ErrInvalidPath = errors.New("invalid archive path")

	// ErrRemoteOnly indicates an archived object whose bytes were evicted to
	// the remote store leaving nothing local to read them out of. It is
	// distinct from a missing object: the archive holds the object, elsewhere.
	//
	// It is what a whole-object read ([*Org.Read]) answers for an evicted
	// configuration tarball, naming the mirrored key to fetch, because that
	// shape holds the bytes in memory and such a tarball has no bound. An
	// unseal streams instead and fetches the object back, so it meets this
	// error only where no fetch is possible, in an organization recording no
	// mirror or over a stub too damaged to trust.
	ErrRemoteOnly = errors.New("archived object is remote-only")

	// ErrRemoteMismatch indicates a remote supplied through [WithRemote]
	// disagrees with the mirror location an organization's marker records.
	// Evicted surfaces live only at the recorded location, so a re-point must
	// be an explicit migration, not an option the browser silently follows;
	// the archiver refuses the same way before a run.
	ErrRemoteMismatch = errors.New("supplied remote does not match the archive's recorded mirror")
)

// orgFile is the marker leaf identifying an organization's archive root.
const orgFile = "org.json"

// Org is a read handle on one organization's archive tree.
//
// Instances are produced by [OpenArchive].
type Org struct {
	remote *orgRemote

	// The browse context the archive was opened under. Long-running work
	// started from the browser (an unseal) derives its cancelable child from
	// it, so an external cancellation (SIGINT) stops that work even on a
	// local-only archive, where no orgRemote carries the context.
	ctx context.Context //nolint:containedctx // Screens start work from tea.Cmds, which take none.

	// Workspace handles memoized by project/name, so repeated lookups (a
	// per-file unseal read) reuse one handle and its lazily built sealed
	// index instead of rebuilding it per call.
	workspaces map[string]*Workspace

	// Name is the organization's directory name, which the archiver keys on the
	// organization name.
	Name string

	root string

	mu sync.Mutex
}

// ArchiveOption configures [OpenArchive].
//
// Options of this type:
//   - [WithContext]
//   - [WithRemote]
//   - [WithRemoteFactory]
type ArchiveOption func(*archiveOptions)

// archiveOptions carries the resolved [OpenArchive] settings.
type archiveOptions struct {
	ctx       context.Context //nolint:containedctx // Plumbed into readers whose interfaces take none.
	newClient remoteClientFactory
	remoteCfg *remote.Config
}

// WithContext sets the context every remote bundle read of the opened
// archive runs under, so canceling it (the browser quitting) retires any
// in-flight request. It defaults to [context.Background]. It returns an
// [ArchiveOption].
func WithContext(ctx context.Context) ArchiveOption {
	return func(o *archiveOptions) {
		if ctx != nil {
			o.ctx = ctx
		}
	}
}

// WithRemote supplies the mirror location for an archive whose local files may
// be partly or wholly absent. [OpenArchive] then unions the organizations it
// finds on disk with those the mirror holds, materializing a mirror-only
// organization's org.json and [remote.MarkerName] marker locally so later
// opens need no supplied remote, and every organization's reads fall through
// to the mirror, persisting what they fetch at its local archive path.
//
// The directory is treated as the archive root unless it already carries an
// org.json, in which case it is one organization's root and the mirror is
// consulted for that organization alone. An organization whose existing
// marker records a different mirror location refuses with
// [ErrRemoteMismatch]. It returns an [ArchiveOption].
func WithRemote(cfg remote.Config) ArchiveOption {
	return func(o *archiveOptions) {
		o.remoteCfg = &cfg
	}
}

// WithRemoteFactory overrides how an organization's remote client is built
// from its marker, defaulting to [remote.New] over the SDK credential chain;
// tests inject a fake-backed builder through it. A nil factory keeps the
// default. It returns an [ArchiveOption].
func WithRemoteFactory(factory func(ctx context.Context, cfg remote.Config) (*remote.Client, error)) ArchiveOption {
	return func(o *archiveOptions) {
		if factory != nil {
			o.newClient = factory
		}
	}
}

// OpenArchive opens the archive at dir and returns its organizations, sorted by
// name.
//
// The path may name the archive root (whose subdirectories are organizations)
// or a single organization's directory; either way each returned [*Org] reads
// one organization tree. A path holding neither shape returns [ErrNotArchive].
//
// An organization whose root carries a remote marker (its sealed bundles were
// offloaded) reads those bundles on demand from its remote store; without the
// marker every read is local and no client is ever constructed. A remote
// supplied through [WithRemote] goes further: the mirror's organizations are
// unioned with the local ones and every read falls through to the mirror,
// persisting what it fetches, so the archive opens even over an empty
// directory.
func OpenArchive(dir string, opts ...ArchiveOption) ([]*Org, error) {
	options := archiveOptions{
		ctx: context.Background(),
		newClient: func(ctx context.Context, cfg remote.Config) (*remote.Client, error) {
			//nolint:wrapcheck // A transparent default factory; callers wrap.
			return remote.New(ctx, cfg)
		},
	}

	for _, opt := range opts {
		opt(&options)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", dir, err)
	}

	if options.remoteCfg != nil {
		return openWithRemote(abs, dir, options)
	}

	return openLocal(abs, dir, options)
}

// openLocal opens the archive from the local tree alone, the [OpenArchive]
// path taken when no remote was supplied.
func openLocal(abs, dir string, options archiveOptions) ([]*Org, error) {
	ok, err := isOrgRoot(abs)
	if err != nil {
		return nil, err
	}

	if ok {
		org, orgErr := newOrg(filepath.Base(abs), abs, options)
		if orgErr != nil {
			return nil, orgErr
		}

		return []*Org{org}, nil
	}

	names, err := localOrgNames(abs, dir)
	if err != nil {
		return nil, err
	}

	var orgs []*Org

	for _, name := range names {
		org, orgErr := newOrg(name, filepath.Join(abs, name), options)
		if orgErr != nil {
			return nil, orgErr
		}

		orgs = append(orgs, org)
	}

	if len(orgs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotArchive, dir)
	}

	slices.SortFunc(orgs, func(a, b *Org) int {
		return strings.Compare(a.Name, b.Name)
	})

	return orgs, nil
}

// localOrgNames returns the names of abs's immediate subdirectories that are
// organization roots.
func localOrgNames(abs, dir string) ([]string, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var names []string

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		ok, err := isOrgRoot(filepath.Join(abs, e.Name()))
		if err != nil {
			return nil, err
		}

		if ok {
			names = append(names, e.Name())
		}
	}

	return names, nil
}

// openWithRemote opens the archive as the union of the local tree and the
// supplied mirror: mirror-only organizations are materialized (directory,
// org.json, marker), local organizations the mirror also holds get its remote
// attached, and purely local organizations open as plain local organizations.
// The mirror is listed once here; each organization's [orgRemote] is seeded
// with its slice of that inventory.
//
// A mirror that cannot be listed degrades rather than failing the open, as
// long as something local exists to browse: every local organization still
// gets the remote attached (its own lazy listing retries, and its failure
// surfaces through [*Org.RemoteWarning]), so an offline machine can keep
// reading the content it has.
func openWithRemote(abs, dir string, options archiveOptions) ([]*Org, error) {
	cfg := *options.remoteCfg

	singleOrg, err := isOrgRoot(abs)
	if err != nil {
		return nil, err
	}

	local := make(map[string]bool)

	if singleOrg {
		local[filepath.Base(abs)] = true
	} else {
		names, namesErr := localOrgNames(abs, dir)
		if namesErr != nil {
			return nil, namesErr
		}

		for _, name := range names {
			local[name] = true
		}
	}

	client, mirror, invErr := listMirrorOrgs(cfg, options)

	if singleOrg {
		// The directory names one organization; every other mirror org is out
		// of scope.
		maps.DeleteFunc(mirror, func(name string, _ map[string]remote.ObjectInfo) bool {
			return name != filepath.Base(abs)
		})
	}

	names := slices.Collect(maps.Keys(local))
	for name := range mirror {
		if !local[name] {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	if len(names) == 0 {
		if invErr != nil {
			return nil, fmt.Errorf("%w: %s holds nothing local and the mirror could not be listed: %w",
				ErrNotArchive, dir, invErr)
		}

		return nil, fmt.Errorf("%w: %s (and the mirror holds no organizations)", ErrNotArchive, dir)
	}

	orgs := make([]*Org, 0, len(names))

	for _, name := range names {
		root := filepath.Join(abs, name)
		if singleOrg {
			root = abs
		}

		org, orgErr := remoteOrg(name, root, cfg, client, mirror[name], invErr != nil, options)
		if orgErr != nil {
			return nil, orgErr
		}

		orgs = append(orgs, org)
	}

	return orgs, nil
}

// listMirrorOrgs builds the shared client and lists the mirror's whole
// inventory once, sliced per organization by the first key segment under the
// configured prefix. An organization counts only when its slice holds an
// org.json, the same marker that identifies a local org root. A client build
// or listing failure comes back as the error alongside a nil map; the caller
// degrades.
func listMirrorOrgs(
	cfg remote.Config, options archiveOptions,
) (*remote.Client, map[string]map[string]remote.ObjectInfo, error) {
	buildCtx, cancel := context.WithTimeout(options.ctx, remoteReadTimeout)
	defer cancel()

	client, err := options.newClient(buildCtx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build remote client: %w", err)
	}

	root := cfg.Key("", "")
	if root != "" {
		root += "/"
	}

	// The listing runs under the browse context alone: a large mirror is many
	// pages, and the client's stall watchdog already bounds a wedged one.
	inventory, err := client.List(options.ctx, root)
	if err != nil {
		return client, nil, fmt.Errorf("list mirror inventory: %w", err)
	}

	byOrg := make(map[string]map[string]remote.ObjectInfo)

	for rel, info := range relativeListing(inventory, root) {
		name, orgRel, ok := strings.Cut(rel, "/")
		if !ok || name == "" || orgRel == "" {
			continue
		}

		if byOrg[name] == nil {
			byOrg[name] = make(map[string]remote.ObjectInfo)
		}

		byOrg[name][orgRel] = info
	}

	maps.DeleteFunc(byOrg, func(_ string, listing map[string]remote.ObjectInfo) bool {
		_, ok := listing[orgFile]

		return !ok
	})

	return client, byOrg, nil
}

// remoteOrg builds one organization handle under a supplied remote: the
// marker conflict check, the mirror-only materialization, and the seeded
// remote all happen here. A local organization the mirror does not hold (and
// whose absence is proven by a successful listing) opens as a plain local
// org, honoring whatever marker it carries.
func remoteOrg(
	name, root string, cfg remote.Config, client *remote.Client,
	listing map[string]remote.ObjectInfo, degraded bool, options archiveOptions,
) (*Org, error) {
	marker, hasMarker, err := remote.ReadMarker(root)
	if err != nil {
		return nil, err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	if hasMarker && marker.Conflicts(cfg.Marker()) {
		return nil, fmt.Errorf(
			"%w: organization %q records its mirror at %q prefix %q, but the supplied remote names %q prefix %q",
			ErrRemoteMismatch, name, marker.URL, marker.Prefix, cfg.URL, cfg.Prefix)
	}

	// A proven miss: the mirror answered and holds no such organization, so
	// this one is purely local and gets no remote it could never use.
	if listing == nil && !degraded {
		return newOrg(name, root, options)
	}

	rem := newSeededOrgRemote(name, cfg, client, listing, options)

	if listing != nil {
		err = materializeOrgRoot(rem, root, cfg, marker, hasMarker)
		if err != nil {
			return nil, fmt.Errorf("materialize organization %q: %w", name, err)
		}
	}

	return &Org{Name: name, root: root, remote: rem, ctx: options.ctx}, nil
}

// materializeOrgRoot lands the two files that make root a self-describing
// organization root: its org.json (fetched if absent) and the remote marker
// that lets later opens find the mirror without a supplied remote. A cleared
// marker (URL "") is the operator's consent to re-record, the same consent
// the archiver honors; a matching one is left exactly as it is.
func materializeOrgRoot(rem *orgRemote, root string, cfg remote.Config, marker remote.Marker, hasMarker bool) error {
	_, statErr := os.Stat(filepath.Join(root, orgFile))
	if errors.Is(statErr, fs.ErrNotExist) {
		err := rem.ensureLocal(root, orgFile)
		if err != nil {
			return err
		}
	}

	if !hasMarker || marker.URL == "" {
		return writeMarker(root, cfg)
	}

	return nil
}

// writeMarker records the supplied remote at the organization's archive root,
// so later opens of this archive need no supplied remote at all. The marker
// carries [remote.Marker.Partial]: the tree beside it is a browse cache the
// mirror must keep standing in for, until a real archive run rewrites the
// marker and the completeness it implies.
func writeMarker(root string, cfg remote.Config) error {
	marker := cfg.Marker()
	marker.Partial = true

	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote marker: %w", err)
	}

	data = append(data, '\n')

	err = atomicfile.WriteFile(filepath.Join(root, remote.MarkerName), data)
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	return nil
}

// newOrg builds one organization handle, loading its remote marker when one
// is present.
func newOrg(name, root string, opts archiveOptions) (*Org, error) {
	rem, err := loadOrgRemote(root, name, opts)
	if err != nil {
		return nil, err
	}

	return &Org{Name: name, root: root, remote: rem, ctx: opts.ctx}, nil
}

// isOrgRoot reports whether dir is one organization's archive root, marked by
// its org.json.
func isOrgRoot(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, orgFile))

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %q: %w", dir, err)
	}
}

// Root returns the absolute path of the organization's archive directory.
func (o *Org) Root() string {
	return o.root
}

// HasRemote reports whether the organization's root carries a remote marker,
// so its evicted objects have somewhere to be fetched from.
//
// It answers what is configured, not what answers: the mirror may be
// unreachable, the credentials absent, the object gone from the bucket. That
// is the useful boundary for a caller predicting a run, since an evicted object
// in an organization with no marker is one an unseal is certain to lose, while
// everything else is only as reachable as the network.
func (o *Org) HasRemote() bool {
	return o.remote != nil
}

// context returns the organization's browse context, defaulting to
// [context.Background] for an Org built without [OpenArchive] (a test
// fixture), so callers never re-derive the fallback themselves.
func (o *Org) context() context.Context {
	if o.ctx != nil {
		return o.ctx
	}

	return context.Background()
}

// AbsPath returns the on-disk absolute path for an archive-relative path.
//
// The result is confined to the root even when relPath carries ".." segments:
// relPath is cleaned as if rooted at "/", collapsing any leading ".." that would
// otherwise escape, before being joined under the root. A clean archive-relative
// path is unchanged.
func (o *Org) AbsPath(relPath string) string {
	clean := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(relPath)), "/")

	return filepath.Join(o.root, filepath.FromSlash(clean))
}

// ReadFile reads a loose file at an archive-relative path. A file absent
// locally is fetched from the organization's mirror when it has one,
// persisting at the same path, so the next read is local.
//
// It suits the organization- and project-level objects, which are never
// sealed; workspace-scoped objects go through [*Workspace.Open], which also
// searches the sealed forms.
func (o *Org) ReadFile(relPath string) ([]byte, error) {
	data, err := os.ReadFile(o.AbsPath(relPath))
	if err == nil {
		return data, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %q: %w", relPath, err)
	}

	return o.readThrough(relPath, fmt.Errorf("read %q: %w", relPath, err))
}

// Projects returns the organization's project directory names, sorted.
func (o *Org) Projects() ([]string, error) {
	return o.subdirs(projectsDir)
}

// Workspaces returns the workspace directory names under a project, sorted.
func (o *Org) Workspaces(project string) ([]string, error) {
	return o.subdirs(path.Join(projectsDir, project, workspacesDir))
}

// Stacks returns the stack directory names under a project, sorted; most
// projects have none.
func (o *Org) Stacks(project string) ([]string, error) {
	return o.subdirs(path.Join(projectsDir, project, "stacks"))
}

// Workspace returns a read handle on one workspace's subtree. Handles are
// memoized per workspace, so every caller shares one lazily built sealed
// index; [Workspace] serializes its own index build, making the shared handle
// safe across goroutines.
func (o *Org) Workspace(project, name string) *Workspace {
	o.mu.Lock()
	defer o.mu.Unlock()

	key := path.Join(project, name)
	if ws, ok := o.workspaces[key]; ok {
		return ws
	}

	ws := &Workspace{
		Project: project,
		Name:    name,
		org:     o,
		dir:     path.Join(projectsDir, project, workspacesDir, name),
	}

	if o.workspaces == nil {
		o.workspaces = make(map[string]*Workspace)
	}

	o.workspaces[key] = ws

	return ws
}

// subdirs returns the sorted immediate subdirectory names of an
// archive-relative directory, tolerating one that does not exist, merged with
// the subdirectories the organization's mirror holds there, so a partly or
// wholly absent local tree still lists whole.
func (o *Org) subdirs(relPath string) ([]string, error) {
	entries, err := o.mergedChildren(relPath)
	if err != nil {
		return nil, err
	}

	var names []string

	for _, e := range entries {
		if e.Dir {
			names = append(names, e.Name)
		}
	}

	return names, nil
}

// looseNames returns the regular-file names directly under an archive-relative
// directory, tolerating one that does not exist, merged with the file names the
// organization's mirror holds there. Callers dedupe and sort.
func (o *Org) looseNames(relPath string) ([]string, error) {
	entries, err := o.mergedChildren(relPath)
	if err != nil {
		return nil, err
	}

	var names []string

	for _, e := range entries {
		if !e.Dir {
			names = append(names, e.Name)
		}
	}

	return names, nil
}

// TreeEntry is one merged child of an archive directory: a subdirectory or a
// loose file, whether it is on disk or known only to the organization's
// mirror.
//
// Instances are produced by [*Org.Entries].
type TreeEntry struct {
	// Name is the child's leaf name; directories carry no trailing separator.
	Name string
	// Size is a file's length in bytes (from the mirror's record when the
	// file is not local); zero for directories.
	Size int64
	// Dir reports a subdirectory.
	Dir bool
	// Remote reports a file the mirror holds that is not on disk yet; reading
	// it fetches and persists it locally.
	Remote bool
}

// Entries returns the merged children of an archive-relative directory,
// directories first, each group sorted by name: the local listing unioned
// with what the organization's mirror holds there, so a partly or wholly
// absent tree browses whole. A directory absent on both sides returns its
// local read error, where the tolerant enumerations ([*Org.subdirs],
// [*Org.looseNames]) answer empty.
func (o *Org) Entries(relDir string) ([]TreeEntry, error) {
	entries, err := o.mergedChildren(relDir)
	if err != nil {
		return nil, err
	}

	if len(entries) == 0 {
		_, statErr := os.Stat(o.AbsPath(relDir))
		if statErr != nil {
			return nil, fmt.Errorf("read %q: %w", relDir, statErr)
		}
	}

	return entries, nil
}

// mergedChildren is the one local-with-mirror merge behind [*Org.Entries],
// [*Org.subdirs], and [*Org.looseNames]: the local directory listing
// (tolerating an absent directory) unioned with the mirror's children, a
// local child winning over its mirror record. Directories come first, each
// group sorted by name.
func (o *Org) mergedChildren(relDir string) ([]TreeEntry, error) {
	dirSet := make(map[string]struct{})
	files := make(map[string]TreeEntry)

	local, err := os.ReadDir(o.AbsPath(relDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %q: %w", relDir, err)
	}

	for _, e := range local {
		if e.IsDir() {
			dirSet[e.Name()] = struct{}{}

			continue
		}

		entry := TreeEntry{Name: e.Name()}

		info, infoErr := e.Info()
		if infoErr == nil {
			entry.Size = info.Size()
		}

		files[e.Name()] = entry
	}

	remoteDirs, remoteFiles := o.remoteChildren(relDir)

	for _, name := range remoteDirs {
		dirSet[name] = struct{}{}
	}

	for name, size := range remoteFiles {
		if _, ok := files[name]; ok {
			continue
		}

		files[name] = TreeEntry{Name: name, Size: size, Remote: true}
	}

	entries := make([]TreeEntry, 0, len(dirSet)+len(files))

	for _, name := range slices.Sorted(maps.Keys(dirSet)) {
		entries = append(entries, TreeEntry{Name: name, Dir: true})
	}

	for _, name := range slices.Sorted(maps.Keys(files)) {
		entries = append(entries, files[name])
	}

	return entries, nil
}

// Archive is a read handle on every organization under one archive directory.
//
// It addresses objects by "<org>/<archive-relative>" paths, the layout an
// unseal reproduces beneath its target, so a listed path names the same file
// before and after recovery. The addressing is invariant whether the archive
// was opened on its root or on a single organization's directory.
//
// Create instances with [NewArchive].
type Archive struct {
	orgs []*Org
}

// NewArchive creates a new [Archive] over the organizations [OpenArchive]
// returned, in the same order.
func NewArchive(orgs []*Org) *Archive {
	return &Archive{orgs: orgs}
}

// Orgs returns the archive's organizations in their opened (name-sorted)
// order.
func (a *Archive) Orgs() []*Org {
	return a.orgs
}

// orgScope pairs one organization with the org-relative remainder of an
// archive path scoped to it.
type orgScope struct {
	org    *Org
	prefix string
}

// scope resolves an org-prefixed archive path onto the organizations it
// covers: an empty path spans every organization, and any other path must
// name a known organization in its first segment or [ErrNoOrg] is returned.
// The match is segment-wise, so "my-o" never matches "my-org".
func (a *Archive) scope(archivePath string) ([]orgScope, error) {
	rel, err := cleanRel(archivePath)
	if err != nil {
		return nil, err
	}

	if rel == "" {
		scopes := make([]orgScope, 0, len(a.orgs))
		for _, org := range a.orgs {
			scopes = append(scopes, orgScope{org: org})
		}

		return scopes, nil
	}

	name, rest, _ := strings.Cut(rel, "/")

	for _, org := range a.orgs {
		if org.Name == name {
			return []orgScope{{org: org, prefix: rest}}, nil
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrNoOrg, name)
}

// List returns the archived objects at or beneath an org-prefixed archive
// path, sorted by [Entry.ArchivePath]. An empty prefix lists every
// organization; a prefix whose first segment names no organization returns
// [ErrNoOrg]. See [*Org.List] for the listing's semantics.
func (a *Archive) List(prefix string) ([]Entry, error) {
	scopes, err := a.scope(prefix)
	if err != nil {
		return nil, err
	}

	var entries []Entry

	for _, sc := range scopes {
		sub, listErr := sc.org.List(sc.prefix)
		if listErr != nil {
			return nil, listErr
		}

		entries = append(entries, sub...)
	}

	// Concatenating name-sorted orgs' path-sorted listings is not already
	// ArchivePath order: the joining "/" compares differently than the byte
	// that follows a shared org-name prefix ("acme-corp/..." sorts before
	// "acme/...").
	slices.SortFunc(entries, func(a, b Entry) int {
		return strings.Compare(a.ArchivePath(), b.ArchivePath())
	})

	return entries, nil
}

// Read returns the bytes at an org-prefixed archive path, whichever physical
// form holds it. See [*Org.Read] for the path's semantics; an empty path (the
// archive itself) is [ErrNotFile].
func (a *Archive) Read(archivePath string) ([]byte, error) {
	scopes, err := a.scope(archivePath)
	if err != nil {
		return nil, err
	}

	if len(scopes) != 1 {
		return nil, fmt.Errorf("%w: %q", ErrNotFile, archivePath)
	}

	return scopes[0].org.Read(scopes[0].prefix)
}

// Unseal extracts every archived object at or beneath an org-prefixed archive
// path into target, reproducing the "<org>/<archive-relative>" layout. An
// empty prefix unseals the whole archive; an empty target returns
// [ErrNoTarget].
//
// Each finished file is handed to progress (which may be nil) with its
// org-prefixed path; a per-file problem is counted in the summary's Errored
// and the run continues. Cancellation stops the run between files, returning
// the partial totals alongside the context's error.
//
//nolint:contextcheck // Index materialization fetches run under the stored browse context (see orgRemote).
func (a *Archive) Unseal(ctx context.Context, target, prefix string, progress UnsealProgress) (UnsealSummary, error) {
	if target == "" {
		return UnsealSummary{}, ErrNoTarget
	}

	scopes, err := a.scope(prefix)
	if err != nil {
		return UnsealSummary{}, err
	}

	var total UnsealSummary

	for _, sc := range scopes {
		jobs, planErr := sc.org.planUnseal(sc.prefix)
		if planErr != nil {
			return total, planErr
		}

		emit := func(ev unsealEvent) bool {
			if progress != nil {
				progress(path.Join(sc.org.Name, ev.Path), ev.Bytes, ev.Err)
			}

			return true
		}

		sum, finished := unsealJobs(ctx, sc.org.Name, target, jobs, emit)
		total.add(sum)

		if !finished {
			return total, fmt.Errorf("unseal stopped: %w", ctx.Err())
		}
	}

	return total, nil
}
