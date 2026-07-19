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

	"go.jacobcolvin.com/hcp_archiver/remote"
)

// ErrNotArchive indicates the given path holds no archive: neither it nor any
// of its immediate subdirectories carries an org.json.
var ErrNotArchive = errors.New("not an archive directory")

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

	// Name is the organization's directory name, which the archiver keys on the
	// organization name.
	Name string

	root string
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
	return o.subdirs("projects")
}

// Workspaces returns the workspace directory names under a project, sorted.
func (o *Org) Workspaces(project string) ([]string, error) {
	return o.subdirs(path.Join("projects", project, "workspaces"))
}

// Stacks returns the stack directory names under a project, sorted; most
// projects have none.
func (o *Org) Stacks(project string) ([]string, error) {
	return o.subdirs(path.Join("projects", project, "stacks"))
}

// Workspace returns a read handle on one workspace's subtree.
func (o *Org) Workspace(project, name string) *Workspace {
	return &Workspace{
		Project: project,
		Name:    name,
		org:     o,
		dir:     path.Join("projects", project, "workspaces", name),
	}
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
