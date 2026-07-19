package view

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/fsid"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// ErrNoTarget indicates an unseal was confirmed with an empty target
// directory.
var ErrNoTarget = errors.New("unseal target directory is required")

// unsealJob is one file of an unseal: the archive-relative path to reproduce
// under the target and the reader that resolves its bytes, [Org.ReadFile] for
// a loose file or [Workspace.Open] for a sealed member.
type unsealJob struct {
	read func() ([]byte, error)
	rel  string
}

// UnsealSummary totals one finished unseal run.
type UnsealSummary struct {
	// Bytes is the total content size written.
	Bytes int64
	// Files is the number of files written.
	Files int
	// Errored is the number of files that errored and were not written.
	Errored int
}

// unsealEvent is the tea.Msg [runUnseal] emits: one per file (Err non-nil on
// a per-file failure), then a terminal event carrying Summary.
type unsealEvent struct {
	Err     error
	Summary *UnsealSummary
	Path    string
	Bytes   int64
}

// planWorkspaceUnseal enumerates one workspace's archived objects as unseal
// jobs: its loose files first, then its sealed members, deduplicated by path
// with the loose copy winning — the same precedence [Workspace.Open] applies.
// The newest roll-up line per path wins for free through the workspace's
// sealed index. Jobs are sorted so progress is deterministic.
func (o *Org) planWorkspaceUnseal(ws *Workspace) ([]unsealJob, error) {
	jobs, seen, err := o.looseJobs(ws.Dir())
	if err != nil {
		return nil, err
	}

	jobs, err = appendSealedJobs(jobs, seen, ws)
	if err != nil {
		return nil, err
	}

	sortJobs(jobs)

	return jobs, nil
}

// planProjectUnseal enumerates a whole project's archived objects as unseal
// jobs: the project's loose subtree (project.json, its stacks, and every
// workspace's loose files), then each workspace's sealed members, deduplicated
// with the loose copy winning.
func (o *Org) planProjectUnseal(project string) ([]unsealJob, error) {
	jobs, seen, err := o.looseJobs(path.Join("projects", project))
	if err != nil {
		return nil, err
	}

	workspaces, err := o.Workspaces(project)
	if err != nil {
		return nil, err
	}

	for _, name := range workspaces {
		jobs, err = appendSealedJobs(jobs, seen, o.Workspace(project, name))
		if err != nil {
			return nil, err
		}
	}

	sortJobs(jobs)

	return jobs, nil
}

// looseJobs builds the loose-file jobs under an archive-relative directory and
// the set of paths they claim, which the sealed pass dedups against.
func (o *Org) looseJobs(relDir string) ([]unsealJob, map[string]struct{}, error) {
	loose, err := o.looseFiles(relDir)
	if err != nil {
		return nil, nil, err
	}

	jobs := make([]unsealJob, 0, len(loose))
	seen := make(map[string]struct{}, len(loose))

	for _, rel := range loose {
		seen[rel] = struct{}{}
		jobs = append(jobs, unsealJob{rel: rel, read: func() ([]byte, error) { return o.ReadFile(rel) }})
	}

	return jobs, seen, nil
}

// appendSealedJobs appends jobs for the workspace's sealed members not already
// claimed by a loose file.
func appendSealedJobs(jobs []unsealJob, seen map[string]struct{}, ws *Workspace) ([]unsealJob, error) {
	sealed, err := ws.sealedNames(ws.Dir())
	if err != nil {
		return nil, err
	}

	for _, rel := range sealed {
		if _, ok := seen[rel]; ok {
			continue
		}

		seen[rel] = struct{}{}
		jobs = append(jobs, unsealJob{rel: rel, read: func() ([]byte, error) { return ws.Open(rel) }})
	}

	return jobs, nil
}

// sortJobs orders jobs by path so an unseal's progress and its target-tree
// writes are deterministic.
func sortJobs(jobs []unsealJob) {
	slices.SortFunc(jobs, func(a, b unsealJob) int {
		return strings.Compare(a.rel, b.rel)
	})
}

// looseFiles returns the archive-relative (slash-separated) paths of the loose
// files under an archive-relative directory, so they dedup exactly against the
// sealed-index keys. The sealed forms themselves (bundles/ zips with their
// sidecars, rollups/) and the [manifest.LedgerDirName] bookkeeping shards are
// skipped — an unseal reproduces archived content, not the archive's own
// machinery — as are the atomic writer's staging leftovers. The walk goes
// through [fsid.WalkFiles], which owns the archive's symlink-aliasing rules.
//
// The stored browse context is the intended parent: planning runs inside
// tea.Cmds, which carry no context of their own.
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
		if inSealedForm(slashed) || atomicfile.IsTemp(path.Base(slashed)) {
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

// inSealedForm reports whether a slash-separated archive-relative path sits
// inside a sealed-form directory ([store.BundlesDirName],
// [store.RollupsDirName]) or a [manifest.LedgerDirName] bookkeeping
// directory.
func inSealedForm(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		switch seg {
		case store.BundlesDirName, store.RollupsDirName, manifest.LedgerDirName:
			return true
		}
	}

	return false
}

// runUnseal extracts jobs into target sequentially, emitting one event per
// file and then a terminal event carrying the summary before closing events.
//
// A per-file problem — an unreadable member, an evicted bundle with no remote
// configured, a member above the [maxMemberSize] read bound (which errors out
// rather than silently truncates), an unsafe recorded name — increments
// Errored and the run continues; ctx cancellation stops the loop between
// files. Every send is guarded on ctx so a receiver that stopped draining can
// never strand this goroutine.
func runUnseal(ctx context.Context, org *Org, target string, jobs []unsealJob, events chan<- unsealEvent) {
	defer close(events)

	var sum UnsealSummary

	send := func(ev unsealEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}

		data, err := job.read()
		if err == nil {
			err = writeUnsealed(target, org.Name, job.rel, data)
		}

		if err != nil {
			sum.Errored++

			if !send(unsealEvent{Path: job.rel, Err: err}) {
				return
			}

			continue
		}

		sum.Files++
		sum.Bytes += int64(len(data))

		if !send(unsealEvent{Path: job.rel, Bytes: int64(len(data))}) {
			return
		}
	}

	send(unsealEvent{Summary: &sum})
}

// writeUnsealed writes one member's bytes at its archive-relative path under
// target/<org>, overwriting an existing file so a re-run refreshes the tree.
//
// The path is untrusted — it comes from a sidecar or roll-up record — so it is
// validated with [seal.ValidName] before any join: cleaning it the way
// [Org.AbsPath] does would silently collapse a traversal instead of refusing
// it. The write is atomic and creates parents, both with the owner-only
// default modes, matching the archive's secret-at-rest stance for raw state.
func writeUnsealed(target, org, rel string, data []byte) error {
	err := seal.ValidName(rel)
	if err != nil {
		return fmt.Errorf("refuse unsafe member name: %w", err)
	}

	err = atomicfile.WriteFile(filepath.Join(target, org, filepath.FromSlash(rel)), data)
	if err != nil {
		return fmt.Errorf("write %q: %w", rel, err)
	}

	return nil
}
