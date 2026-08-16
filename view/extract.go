package view

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/seal"
)

// ErrNoTarget indicates an extract was confirmed with an empty target
// directory.
var ErrNoTarget = errors.New("extract target directory is required")

// extractJob is one file of an extract: the archive-relative path to reproduce
// under the target and the writer that streams its bytes through
// [*Org.writeObject], whichever physical form holds them and wherever they
// live.
//
// The job streams rather than returning bytes because the surfaces it covers
// have no common bound: a bundle member is capped by [maxMemberSize], but a
// configuration tarball fetched from the mirror is whatever size it was
// archived at, and buffering one whole would put an archive's largest object
// into memory to write it straight back out.
//
// The path is a parameter of write rather than something write closes over, so
// a plan is a list of paths beside one shared function value instead of one
// closure per object. A whole-organization plan holds every job at once, and a
// per-object closure would pin whatever the loop that built it had in scope.
type extractJob struct {
	write func(ctx context.Context, rel string, w io.Writer) (int64, error)
	rel   string
}

// ExtractSummary totals one finished extract run.
type ExtractSummary struct {
	// Bytes is the total content size written.
	Bytes int64
	// Files is the number of files written.
	Files int
	// Errored is the number of files that errored and were not written.
	Errored int
}

// add folds another run's totals into s, so a multi-organization extract
// accumulates one summary.
func (s *ExtractSummary) add(other ExtractSummary) {
	s.Bytes += other.Bytes
	s.Files += other.Files
	s.Errored += other.Errored
}

// ExtractProgress observes one file of an extract run: the org-prefixed archive
// path just processed, its written byte count, and its error. A per-file
// failure does not stop the run; it is counted in [ExtractSummary].Errored.
type ExtractProgress func(archivePath string, bytes int64, err error)

// extractEvent is the tea.Msg [runExtract] emits: one per file (Err non-nil on
// a per-file failure), then a terminal event carrying Summary.
type extractEvent struct {
	Err     error
	Summary *ExtractSummary
	Path    string
	Bytes   int64
}

// planExtract enumerates the archived objects at or beneath an archive-relative
// prefix as extract jobs, in listing order. The jobs carry exactly [*Org.List]'s
// entries, with the same dedup, the same machinery filter, and the same order,
// so a listing is a faithful dry run of the extract it plans.
//
// That faithfulness is about what the run attempts, not about what it
// recovers: a listed object can still fail its read, and one kind fails
// predictably. An object the eviction moved to the mirror is fetched back in an
// organization that records where its mirror is (see [*Org.HasRemote]) and lost
// in one that does not, so a caller summarizing the plan can count the second
// kind against it up front.
func (o *Org) planExtract(prefix string) ([]extractJob, error) {
	entries, err := o.List(prefix)
	if err != nil {
		return nil, err
	}

	jobs := make([]extractJob, 0, len(entries))
	write := o.writeObject

	for _, e := range entries {
		jobs = append(jobs, extractJob{rel: e.Path, write: write})
	}

	return jobs, nil
}

// planWorkspaceExtract enumerates one workspace's archived objects as extract
// jobs.
func (o *Org) planWorkspaceExtract(ws *Workspace) ([]extractJob, error) {
	return o.planExtract(ws.Dir())
}

// planProjectExtract enumerates a whole project's archived objects as extract
// jobs.
func (o *Org) planProjectExtract(project string) ([]extractJob, error) {
	return o.planExtract(path.Join(projectsDir, project))
}

// extractJobs writes jobs into target under org sequentially, handing each
// outcome to emit, and returns the totals plus whether the run finished.
//
// A per-file problem increments Errored and the run continues: an unreadable
// member, an evicted tarball in an organization that records no mirror
// ([ErrRemoteOnly]), a member of an evicted bundle in one ([ErrObjectNotFound],
// since a sidecar entry is not something bytes can be read out of), a mirrored
// object whose fetch fails or comes back contradicting the proof its eviction
// recorded, a member above the [maxMemberSize] read bound (which errors out
// rather than silently truncates), an unsafe recorded name. The loop stops
// between files once ctx ends or emit returns false, reporting an unfinished
// run.
//
// A cancellation is not a per-file problem, even though it surfaces as one: a
// SIGINT lands inside a long fetch as that file's error, and counting it would
// report a failed object where the operator stopped the run. A failed write
// under an ended context therefore ends the run unfinished, which is what the
// callers turn into an interrupted-run message rather than a failure.
func extractJobs(
	ctx context.Context,
	org, target string,
	jobs []extractJob,
	emit func(extractEvent) bool,
) (ExtractSummary, bool) {
	var sum ExtractSummary

	for _, job := range jobs {
		if ctx.Err() != nil {
			return sum, false
		}

		n, err := writeExtracted(ctx, target, org, job.rel, job.write)
		if err != nil {
			if ctx.Err() != nil {
				return sum, false
			}

			sum.Errored++

			if !emit(extractEvent{Path: job.rel, Err: err}) {
				return sum, false
			}

			continue
		}

		sum.Files++
		sum.Bytes += n

		if !emit(extractEvent{Path: job.rel, Bytes: n}) {
			return sum, false
		}
	}

	return sum, true
}

// runExtract extracts jobs into target sequentially, emitting one event per
// file and then a terminal event carrying the summary before closing events.
// Every send is guarded on ctx so a receiver that stopped draining can never
// strand this goroutine; see [extractJobs] for the loop's semantics.
func runExtract(ctx context.Context, org *Org, target string, jobs []extractJob, events chan<- extractEvent) {
	defer close(events)

	send := func(ev extractEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	sum, finished := extractJobs(ctx, org.Name, target, jobs, send)
	if finished {
		send(extractEvent{Summary: &sum})
	}
}

// writeExtracted streams one object at its archive-relative path under
// target/<org>, overwriting an existing file so a re-run refreshes the tree,
// and returns how many bytes write produced.
//
// The path is untrusted, coming from a sidecar or roll-up record, so it is
// validated with [seal.ValidName] before any join and before anything is
// created. Cleaning it the way [*Org.AbsPath] does would silently collapse a
// traversal instead of refusing it. The write is atomic and creates parents,
// both with the owner-only default modes, matching the archive's secret-at-rest
// stance for raw state.
//
// Staging the stream is what makes a verified fetch safe, because
// [atomicfile.Write] discards the staging file when write fails, so an object
// whose bytes come back from the mirror contradicting the digest its eviction
// recorded leaves no file at all rather than a plausible-looking one. The cost
// is a directory, since the parents are created before the first byte is asked
// for, so a file that never arrives can leave an empty directory behind it.
//
// A failure of write is returned as it stands, so the reason an object could
// not be resolved reads plainly; only a failure of the write path itself is
// wrapped with the path it was writing.
func writeExtracted(
	ctx context.Context,
	target, org, rel string,
	write func(ctx context.Context, rel string, w io.Writer) (int64, error),
) (int64, error) {
	err := seal.ValidName(rel)
	if err != nil {
		return 0, fmt.Errorf("refuse unsafe member name: %w", err)
	}

	var (
		n        int64
		writeErr error
	)

	err = atomicfile.Write(filepath.Join(target, org, filepath.FromSlash(rel)), func(w io.Writer) error {
		n, writeErr = write(ctx, rel, w)

		return writeErr
	})

	switch {
	case writeErr != nil:
		return n, writeErr
	case err != nil:
		return n, fmt.Errorf("write %q: %w", rel, err)
	}

	return n, nil
}
