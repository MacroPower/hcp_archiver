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
	// Name is the organization's directory name, which the archiver keys on the
	// organization name.
	Name string

	root string
}

// OpenArchive opens the archive at dir and returns its organizations, sorted by
// name.
//
// The path may name the archive root (whose subdirectories are organizations)
// or a single organization's directory; either way each returned [*Org] reads
// one organization tree. A path holding neither shape returns [ErrNotArchive].
func OpenArchive(dir string) ([]*Org, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", dir, err)
	}

	ok, err := isOrgRoot(abs)
	if err != nil {
		return nil, err
	}

	if ok {
		return []*Org{{Name: filepath.Base(abs), root: abs}}, nil
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
			orgs = append(orgs, &Org{Name: e.Name(), root: sub})
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

// AbsPath returns the on-disk absolute path for an archive-relative path.
func (o *Org) AbsPath(relPath string) string {
	return filepath.Join(o.root, filepath.FromSlash(relPath))
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
	entries, err := os.ReadDir(o.AbsPath(relPath))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", relPath, err)
	}

	var names []string

	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}

	slices.Sort(names)

	return names, nil
}
