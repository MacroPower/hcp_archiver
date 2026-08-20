package export

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"text/template"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

var (
	// ErrNoTarget indicates an export was run with an empty target directory.
	ErrNoTarget = errors.New("export target directory is required")

	// ErrTargetNotDir indicates the export target exists and is not a
	// directory, so nothing can be rendered beneath it.
	ErrTargetNotDir = errors.New("export target is not a directory")

	// ErrTargetNotEmpty indicates the export target directory already holds
	// content the export will not silently mix with or replace; [WithForce]
	// opts into replacing it.
	ErrTargetNotEmpty = errors.New("export target directory is not empty")

	// ErrTargetOverlapsArchive indicates the export target and an
	// organization's archive directory contain one another in either
	// direction, where writing the site (or clearing the target under
	// [WithForce]) would touch the archive itself.
	ErrTargetOverlapsArchive = errors.New("export target overlaps the archive directory")
)

// Site file permissions. The exported tree holds metadata only, curated for
// sharing, so unlike the archive's owner-only files it is written
// world-readable the way a site source is expected to be.
const (
	siteFileMode fs.FileMode = 0o644
	siteDirMode  fs.FileMode = 0o755
)

// indexFile is the leaf every generated directory carries, the name
// directory-based site generators treat as a section index.
const indexFile = "index.md"

// renderConcurrency bounds how many workspaces and stacks render at once.
// Rendering one is a long series of small reads whose cost is latency rather
// than computation, so the bound sits well above the core count: it exists to
// cap open files and in-flight pages, not to match the CPU. It matches the
// archiver's own fixed bound, and like it is deliberately not configurable.
const renderConcurrency = 16

// Summary reports what one export run produced.
type Summary struct {
	// Pages counts the markdown files written, the copied workspace readmes
	// included.
	Pages int
	// Orgs counts the organizations rendered.
	Orgs int
}

// Exporter renders an archive's metadata as a markdown tree beneath a target
// directory.
//
// Create instances with [New].
type Exporter struct {
	arc          *view.Archive
	tmpl         *template.Template
	progress     Progress
	target       string
	templatesDir string
	pages        atomic.Int64
	force        bool
}

// Option configures an [Exporter].
//
// Options of this type:
//   - [WithForce]
//   - [WithProgress]
//   - [WithTemplatesDir]
type Option func(*Exporter)

// WithForce lets the export replace a non-empty target directory's contents
// instead of refusing with [ErrTargetNotEmpty]. It returns an [Option].
func WithForce() Option {
	return func(e *Exporter) {
		e.force = true
	}
}

// WithTemplatesDir names a directory of *.md.tmpl files overriding the
// export's built-in page templates by filename; pages without an override
// keep their default. An empty dir leaves every default in place. It returns
// an [Option].
func WithTemplatesDir(dir string) Option {
	return func(e *Exporter) {
		e.templatesDir = dir
	}
}

// New creates a new [Exporter] rendering arc's metadata beneath target.
func New(arc *view.Archive, target string, opts ...Option) *Exporter {
	e := &Exporter{arc: arc, target: target, progress: nopProgress{}}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Run renders the archive and returns what it produced. The target directory
// is created when absent; see [ErrNoTarget], [ErrTargetNotDir],
// [ErrTargetNotEmpty], and [ErrTargetOverlapsArchive] for the shapes it
// refuses. Organizations render in turn, the workspaces and stacks within a
// project concurrently. Cancellation stops the run between those items,
// returning the context's error with the partial totals.
func (e *Exporter) Run(ctx context.Context) (Summary, error) {
	e.pages.Store(0)

	if e.target == "" {
		return Summary{}, ErrNoTarget
	}

	// Templates load before the target is touched, so a broken override never
	// costs a cleared directory under [WithForce].
	tmpl, err := loadTemplates(e.templatesDir)
	if err != nil {
		return Summary{}, err
	}

	e.tmpl = tmpl

	err = e.prepareTarget()
	if err != nil {
		return Summary{}, err
	}

	orgs := e.arc.Orgs()

	err = e.writeArchiveIndex(orgs)
	if err != nil {
		return Summary{}, err
	}

	for rendered, org := range orgs {
		err = e.renderOrg(ctx, org)
		if err != nil {
			return Summary{Pages: int(e.pages.Load()), Orgs: rendered}, err
		}
	}

	return Summary{Pages: int(e.pages.Load()), Orgs: len(orgs)}, nil
}

// prepareTarget brings the target directory to an empty, writable state:
// disjoint from every organization's archive root, created when absent,
// refused when it is a file or holds content without force, and cleared of
// its contents (never removed itself) under force.
func (e *Exporter) prepareTarget() error {
	err := e.checkTargetDisjoint()
	if err != nil {
		return err
	}

	info, err := os.Stat(e.target)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		mkErr := atomicfile.MkdirAll(e.target, siteDirMode)
		if mkErr != nil {
			return fmt.Errorf("create target: %w", mkErr)
		}

		return nil

	case err != nil:
		return fmt.Errorf("stat target: %w", err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s", ErrTargetNotDir, e.target)
	}

	entries, err := os.ReadDir(e.target)
	if err != nil {
		return fmt.Errorf("read target: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	if !e.force {
		return fmt.Errorf("%w: %s", ErrTargetNotEmpty, e.target)
	}

	// Clearing a previous export walks that whole tree, so it is worth naming:
	// its cost tracks the old site's size, which nothing here has counted, so
	// the phase runs without a total rather than paying for a walk to size a
	// bar with.
	e.progress.SetPhase(PhaseClear)
	e.progress.SetTarget(e.target)

	for _, entry := range entries {
		err = os.RemoveAll(filepath.Join(e.target, entry.Name()))
		if err != nil {
			return fmt.Errorf("clear target: %w", err)
		}
	}

	return nil
}

// checkTargetDisjoint refuses a target that contains, equals, or sits inside
// any organization's archive root: writing the site into the archive would
// pollute it, and a forced clear of a containing target would delete it. The
// guard lives here, beside the destructive clear, so no caller can reach the
// wipe without it. Symlinks are not resolved, so a symlinked path can dodge
// the check, accepted rather than pulling physical identity resolution in.
func (e *Exporter) checkTargetDisjoint() error {
	absTarget, err := filepath.Abs(e.target)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", e.target, err)
	}

	for _, org := range e.arc.Orgs() {
		if pathkit.Overlaps(absTarget, org.Root()) {
			return fmt.Errorf("%w: %s and %s", ErrTargetOverlapsArchive, e.target, org.Root())
		}
	}

	return nil
}

// write stores one generated file at a target-relative forward-slash path,
// creating parents.
func (e *Exporter) write(rel string, data []byte) error {
	abs := filepath.Join(e.target, filepath.FromSlash(rel))

	err := atomicfile.WriteFile(abs, data,
		atomicfile.WithFileMode(siteFileMode), atomicfile.WithDirMode(siteDirMode))
	if err != nil {
		return fmt.Errorf("write %q: %w", rel, err)
	}

	e.pages.Add(1)

	return nil
}

// writeArchiveIndex renders the site home: one row per organization.
func (e *Exporter) writeArchiveIndex(orgs []*view.Org) error {
	pageCtx := ArchivePage{Orgs: make([]ArchiveOrg, 0, len(orgs))}
	for _, org := range orgs {
		pageCtx.Orgs = append(pageCtx.Orgs, ArchiveOrg{Name: org.Name})
	}

	data, err := e.renderPage("archive.md.tmpl", pageCtx)
	if err != nil {
		return err
	}

	return e.write(indexFile, data)
}
