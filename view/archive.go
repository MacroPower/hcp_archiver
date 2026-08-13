package view

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

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
//   - [WithRemoteFactory]
type ArchiveOption func(*archiveOptions)

// archiveOptions carries the resolved [OpenArchive] settings.
type archiveOptions struct {
	ctx       context.Context //nolint:containedctx // Plumbed into readers whose interfaces take none.
	newClient remoteClientFactory
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
// marker every read is local and no client is ever constructed.
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

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var orgs []*Org

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		sub := filepath.Join(abs, e.Name())

		ok, err = isOrgRoot(sub)
		if err != nil {
			return nil, err
		}

		if ok {
			org, orgErr := newOrg(e.Name(), sub, options)
			if orgErr != nil {
				return nil, orgErr
			}

			orgs = append(orgs, org)
		}
	}

	if len(orgs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotArchive, dir)
	}

	slices.SortFunc(orgs, func(a, b *Org) int {
		return strings.Compare(a.Name, b.Name)
	})

	return orgs, nil
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

// ReadFile reads a loose file at an archive-relative path.
//
// It suits the organization- and project-level objects, which are never
// sealed; workspace-scoped objects go through [*Workspace.Open], which also
// searches the sealed forms.
func (o *Org) ReadFile(relPath string) ([]byte, error) {
	data, err := os.ReadFile(o.AbsPath(relPath))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", relPath, err)
	}

	return data, nil
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
// archive-relative directory, tolerating one that does not exist.
func (o *Org) subdirs(relPath string) ([]string, error) {
	names, err := subdirNames(o.AbsPath(relPath))
	if err != nil {
		return nil, err
	}

	slices.Sort(names)

	return names, nil
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
