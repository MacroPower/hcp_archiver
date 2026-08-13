package view

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/seal"
)

// ErrNoTarget indicates an unseal was confirmed with an empty target
// directory.
var ErrNoTarget = errors.New("unseal target directory is required")

// unsealJob is one file of an unseal: the archive-relative path to reproduce
// under the target and the reader that resolves its bytes through [*Org.Read],
// whichever physical form holds them.
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

// add folds another run's totals into s, so a multi-organization unseal
// accumulates one summary.
func (s *UnsealSummary) add(other UnsealSummary) {
	s.Bytes += other.Bytes
	s.Files += other.Files
	s.Errored += other.Errored
}

// UnsealProgress observes one file of an unseal run: the org-prefixed archive
// path just processed, its written byte count, and its error. A per-file
// failure does not stop the run; it is counted in [UnsealSummary].Errored.
type UnsealProgress func(archivePath string, bytes int64, err error)

// unsealEvent is the tea.Msg [runUnseal] emits: one per file (Err non-nil on
// a per-file failure), then a terminal event carrying Summary.
type unsealEvent struct {
	Err     error
	Summary *UnsealSummary
	Path    string
	Bytes   int64
}

// planUnseal enumerates the archived objects at or beneath an archive-relative
// prefix as unseal jobs, in listing order. The jobs carry exactly [*Org.List]'s
// entries — same dedup, same machinery filter, same order — so a listing is a
// faithful dry run of the unseal it plans.
func (o *Org) planUnseal(prefix string) ([]unsealJob, error) {
	entries, err := o.List(prefix)
	if err != nil {
		return nil, err
	}

	jobs := make([]unsealJob, 0, len(entries))

	for _, e := range entries {
		jobs = append(jobs, unsealJob{rel: e.Path, read: func() ([]byte, error) { return o.Read(e.Path) }})
	}

	return jobs, nil
}

// planWorkspaceUnseal enumerates one workspace's archived objects as unseal
// jobs.
func (o *Org) planWorkspaceUnseal(ws *Workspace) ([]unsealJob, error) {
	return o.planUnseal(ws.Dir())
}

// planProjectUnseal enumerates a whole project's archived objects as unseal
// jobs.
func (o *Org) planProjectUnseal(project string) ([]unsealJob, error) {
	return o.planUnseal(path.Join(projectsDir, project))
}

// unsealJobs writes jobs into target under org sequentially, handing each
// outcome to emit, and returns the totals plus whether the run finished.
//
// A per-file problem — an unreadable member, an evicted bundle with no remote
// configured, a member above the [maxMemberSize] read bound (which errors out
// rather than silently truncates), an unsafe recorded name — increments
// Errored and the run continues. The loop stops between files once ctx ends
// or emit returns false, reporting an unfinished run.
func unsealJobs(
	ctx context.Context,
	org, target string,
	jobs []unsealJob,
	emit func(unsealEvent) bool,
) (UnsealSummary, bool) {
	var sum UnsealSummary

	for _, job := range jobs {
		if ctx.Err() != nil {
			return sum, false
		}

		data, err := job.read()
		if err == nil {
			err = writeUnsealed(target, org, job.rel, data)
		}

		if err != nil {
			sum.Errored++

			if !emit(unsealEvent{Path: job.rel, Err: err}) {
				return sum, false
			}

			continue
		}

		sum.Files++
		sum.Bytes += int64(len(data))

		if !emit(unsealEvent{Path: job.rel, Bytes: int64(len(data))}) {
			return sum, false
		}
	}

	return sum, true
}

// runUnseal extracts jobs into target sequentially, emitting one event per
// file and then a terminal event carrying the summary before closing events.
// Every send is guarded on ctx so a receiver that stopped draining can never
// strand this goroutine; see [unsealJobs] for the loop's semantics.
func runUnseal(ctx context.Context, org *Org, target string, jobs []unsealJob, events chan<- unsealEvent) {
	defer close(events)

	send := func(ev unsealEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	sum, finished := unsealJobs(ctx, org.Name, target, jobs, send)
	if finished {
		send(unsealEvent{Summary: &sum})
	}
}

// writeUnsealed writes one member's bytes at its archive-relative path under
// target/<org>, overwriting an existing file so a re-run refreshes the tree.
//
// The path is untrusted — it comes from a sidecar or roll-up record — so it is
// validated with [seal.ValidName] before any join: cleaning it the way
// [*Org.AbsPath] does would silently collapse a traversal instead of refusing
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
