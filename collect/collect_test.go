package collect_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/history"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/serialize"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// fixedClock returns a clock stuck at a fixed instant for deterministic ledger
// timestamps.
func fixedClock() func() time.Time {
	at := time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)

	return func() time.Time { return at }
}

// newEnv builds an [collect.Env] over a real store and ledger rooted in the
// test's temp dir, plus the ledger so a test can inspect and seed it.
func newEnv(t *testing.T, opts ...collect.Option) (*collect.Env, *store.Store, *manifest.Ledger) {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(st.Root(), manifest.WithClock(fixedClock()))
	require.NoError(t, err)

	// A zero confirm delay keeps the 404-confirming re-probe from sleeping in
	// tests; a caller's own options still override it.
	env := collect.NewEnv(nil, st, ledger,
		append([]collect.Option{collect.WithAbsentConfirm(0)}, opts...)...)

	return env, st, ledger
}

// cannedProject is a listed go-tfe object a fetch closure returns.
func cannedProject() *tfe.Project {
	return &tfe.Project{
		ID:                   "prj-abc",
		Name:                 "example",
		DefaultExecutionMode: "remote",
	}
}

// sidecarExists reports whether the object at relPath grew a history sidecar.
func sidecarExists(t *testing.T, st *store.Store, relPath string) bool {
	t.Helper()

	present, err := st.Exists(st.HistoryPath(relPath))
	require.NoError(t, err)

	return present
}

// sidecarRecords parses the object's history sidecar, oldest record first.
func sidecarRecords(t *testing.T, st *store.Store, relPath string) []history.Record {
	t.Helper()

	data, err := os.ReadFile(st.AbsPath(st.HistoryPath(relPath)))
	require.NoError(t, err)

	var out []history.Record

	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec history.Record

		require.NoError(t, json.Unmarshal([]byte(line), &rec))

		out = append(out, rec)
	}

	return out
}

// timeoutError is a net.Error whose timeout classifies as transient.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestEnvConcurrency(t *testing.T) {
	t.Parallel()

	// The fan-out ceiling is fixed: the client's gate bounds real request
	// parallelism, so there is no per-run knob to plumb.
	env, _, _ := newEnv(t)

	assert.Equal(t, collect.DefaultConcurrency, env.Concurrency())
}

func TestEnvObject(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/project.json"

	tests := map[string]struct {
		fetch      func(called *bool) func(context.Context) (any, error)
		seed       func(l *manifest.Ledger)
		wantStatus manifest.Status
		wantFetch  bool
		wantSig    bool
	}{
		"records done and signature on success": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return cannedProject(), nil
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusDone,
			wantSig:    true,
		},
		"records a settled absence on a terminal error": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return nil, tfe.ErrResourceNotFound
				}
			},
			wantFetch: true,
			// The 404 is confirmed by an in-run re-probe (see
			// TestEnvObjectAbsenceConfirmedInRun) and then settles absent.
			wantStatus: manifest.StatusAbsent,
		},
		"records errored on a transient error": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return nil, timeoutError{}
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusErrored,
		},
		"records forbidden on an access denial": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return nil, errors.New("forbidden\n\nTeam and Organization Tokens are not supported")
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusForbidden,
		},
		"skips when the ledger has it settled": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(relPath, manifest.Signature{Hash: "seed", Size: 1})
			},
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return cannedProject(), nil
				}
			},
			wantFetch:  false,
			wantStatus: manifest.StatusDone,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, st, ledger := newEnv(t)

			if tc.seed != nil {
				tc.seed(ledger)
			}

			called := false

			err := env.Object(t.Context(), relPath, tc.fetch(&called))
			require.NoError(t, err)

			assert.Equal(t, tc.wantFetch, called)

			entry, ok := ledger.Entry(relPath)
			require.True(t, ok)
			assert.Equal(t, tc.wantStatus, entry.Status)

			if tc.wantSig {
				require.NotNil(t, entry.Signature)
				assert.NotEmpty(t, entry.Signature.Hash)
				assert.Positive(t, entry.Signature.Size)

				exists, existErr := st.Exists(relPath)
				require.NoError(t, existErr)
				assert.True(t, exists)

				assert.Equal(t, entry.Signature.Size, ledger.Tally().BytesDownloaded)
			}
		})
	}
}

func TestEnvObjectPropagatesCancellation(t *testing.T) {
	t.Parallel()

	env, _, ledger := newEnv(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := env.Object(ctx, "projects/example/project.json", func(ctx context.Context) (any, error) {
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)

	_, ok := ledger.Entry("projects/example/project.json")
	assert.False(t, ok, "a canceled fetch must not record an outcome")
}

func TestEnvMutableAlwaysRefetches(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/project.json"

	env, _, ledger := newEnv(t)
	ledger.RecordDone(relPath, manifest.Signature{Hash: "seed", Size: 1})

	called := false

	err := env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		called = true

		return cannedProject(), nil
	})
	require.NoError(t, err)

	assert.True(t, called, "Mutable must re-fetch even a settled object")

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, entry.Status)
	assert.NotEqual(t, "seed", entry.Signature.Hash, "the refreshed signature replaces the seed")
}

func TestEnvMutableSealedElsewhereSkipsWrite(t *testing.T) {
	t.Parallel()

	// A run.json coalesced into a roll-up: recorded done with this exact
	// content, loose file removed by the seal. An unchanged re-read must not
	// re-materialize the loose file (the next seal would append a duplicate
	// roll-up line) and must not churn the entry.
	const relPath = "projects/example/workspaces/ws/runs/run-1/run.json"

	env, st, ledger := newEnv(t)

	fetch := func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}

	require.NoError(t, env.Mutable(t.Context(), relPath, fetch))

	before, ok := ledger.Entry(relPath)
	require.True(t, ok)

	// Model the seal: the roll-up holds the bytes, the loose source is gone.
	require.NoError(t, os.Remove(st.AbsPath(relPath)))

	require.NoError(t, env.Mutable(t.Context(), relPath, fetch))

	exists, err := st.Exists(relPath)
	require.NoError(t, err)
	assert.False(t, exists, "an unchanged sealed object is not re-materialized")

	after, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, before.Attempts, after.Attempts,
		"the skip leaves the entry untouched rather than churning Attempts")

	assert.False(t, sidecarExists(t, st, relPath),
		"an unchanged skip supersedes nothing and grows no sidecar")
}

func TestEnvMutableSealedElsewhereWritesChangedPayload(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/workspaces/ws/runs/run-1/run.json"

	env, st, ledger := newEnv(t)

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}))
	require.NoError(t, os.Remove(st.AbsPath(relPath)))

	// The payload changed after the seal (a force-canceled run gaining a new
	// status text, say): the loose file must come back so the next seal
	// appends the newer line.
	changed := cannedProject()
	changed.Name = "renamed"

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return changed, nil
	}))

	exists, err := st.Exists(relPath)
	require.NoError(t, err)
	assert.True(t, exists, "a changed payload writes loose again")

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, entry.Status)

	assert.False(t, sidecarExists(t, st, relPath),
		"the sealed prior version lives in the roll-up, not a sidecar")
}

func TestEnvMutableSealedElsewhereFilePresentDedupes(t *testing.T) {
	t.Parallel()

	// With the loose file still on disk, the sealed-elsewhere gate stays out
	// of the way: the write path's own dedup keeps the file and re-records.
	const relPath = "projects/example/workspaces/ws/runs/run-1/run.json"

	env, st, ledger := newEnv(t)

	fetch := func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}

	require.NoError(t, env.Mutable(t.Context(), relPath, fetch))
	require.NoError(t, env.Mutable(t.Context(), relPath, fetch))

	exists, err := st.Exists(relPath)
	require.NoError(t, err)
	assert.True(t, exists)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, 2, entry.Attempts, "a present file re-records normally")

	assert.False(t, sidecarExists(t, st, relPath),
		"an unchanged re-read appends nothing")
}

func TestEnvMutableSealedElsewhereStatErrorFailsOpen(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so no stat error can be provoked")
	}

	// A stat that errors for a reason other than absence (here EACCES from an
	// unsearchable parent) must not be read as "absent and sealed": the gate
	// fails open and the write path proceeds, surfacing the real problem as a
	// recorded write failure instead of silently skipping.
	env, st, ledger := newEnv(t)

	const relPath = "guarded/run.json"

	fetch := func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}

	// First write with a clear path records done with the payload's signature.
	require.NoError(t, env.Mutable(t.Context(), relPath, fetch))

	dir := st.AbsPath("guarded")
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() {
		//nolint:errcheck,gosec // Restore so TempDir cleanup can remove the tree.
		os.Chmod(dir, 0o700)
	})

	require.NoError(t, env.Mutable(t.Context(), relPath, fetch))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status,
		"an unverifiable absence proceeds to the write, whose failure is recorded")
}

func TestEnvMutableRetainsSupersededContent(t *testing.T) {
	t.Parallel()

	// The variable-edit case: a value changed in TFC between runs must leave
	// the prior version recoverable from the history sidecar.
	const relPath = "projects/example/workspaces/ws/variables.json"

	env, st, ledger := newEnv(t)

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}))

	assert.False(t, sidecarExists(t, st, relPath), "a first write grows no sidecar")

	prior, ok := ledger.Entry(relPath)
	require.True(t, ok)

	edited := cannedProject()
	edited.Name = "renamed"

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return edited, nil
	}))

	want, err := serialize.Marshal(cannedProject())
	require.NoError(t, err)

	recs := sidecarRecords(t, st, relPath)
	require.Len(t, recs, 1)
	assert.Equal(t, string(want), recs[0].Content,
		"the sidecar holds the superseded payload byte for byte")
	assert.Equal(t, prior.FetchedAt, recs[0].FetchedAt,
		"stamped with the prior run's recorded fetch time")

	// An unchanged re-read costs only the fetch and appends nothing.
	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return edited, nil
	}))

	assert.Len(t, sidecarRecords(t, st, relPath), 1)
}

func TestEnvObjectKeepsNoHistory(t *testing.T) {
	t.Parallel()

	// The immutable path keeps only its current content: even a changed
	// overwrite (an errored entry re-fetched with different bytes) grows no
	// sidecar.
	const relPath = "projects/example/project.json"

	env, st, ledger := newEnv(t)

	require.NoError(t, env.Object(t.Context(), relPath, func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}))

	ledger.RecordErrored(relPath, errors.New("boom"), true)

	changed := cannedProject()
	changed.Name = "renamed"

	require.NoError(t, env.Object(t.Context(), relPath, func(_ context.Context) (any, error) {
		return changed, nil
	}))

	assert.False(t, sidecarExists(t, st, relPath))
}

func TestEnvMutableAbsentBuriesOnce(t *testing.T) {
	t.Parallel()

	// A mutable object deleted upstream: the last-known content and a single
	// tombstone land in the sidecar, the file stays on disk, and the re-run
	// that re-404s appends nothing more.
	const relPath = "projects/example/workspaces/ws/tags.json"

	env, st, ledger := newEnv(t)

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}))

	notFound := func(_ context.Context) (any, error) {
		return nil, tfe.ErrResourceNotFound
	}

	require.NoError(t, env.Mutable(t.Context(), relPath, notFound))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusAbsent, entry.Status)

	want, err := serialize.Marshal(cannedProject())
	require.NoError(t, err)

	recs := sidecarRecords(t, st, relPath)
	require.Len(t, recs, 2)
	assert.Equal(t, string(want), recs[0].Content, "the last-known content is flushed first")
	assert.True(t, recs[1].Deleted, "the tombstone closes the timeline")

	exists, err := st.Exists(relPath)
	require.NoError(t, err)
	assert.True(t, exists, "the last-known file is never removed")

	// Mutable re-404s every run; only the first observation records.
	require.NoError(t, env.Mutable(t.Context(), relPath, notFound))
	assert.Len(t, sidecarRecords(t, st, relPath), 2)
}

func TestEnvMutableAbsentUnknownObjectCreatesNoSidecar(t *testing.T) {
	t.Parallel()

	// An object that 404s without ever having been archived has nothing to
	// bury: no file, no sidecar, and none may be created.
	const relPath = "projects/example/workspaces/ws/variables.json"

	env, st, ledger := newEnv(t)

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return nil, tfe.ErrResourceNotFound
	}))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusAbsent, entry.Status)

	assert.False(t, sidecarExists(t, st, relPath))
}

func TestEnvMutableReappearsAfterTombstone(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/workspaces/ws/variables.json"

	tests := map[string]struct {
		reappear func() *tfe.Project
	}{
		"with the same content": {
			reappear: cannedProject,
		},
		"with changed content": {
			reappear: func() *tfe.Project {
				p := cannedProject()
				p.Name = "renamed"

				return p
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, st, ledger := newEnv(t)

			require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
				return cannedProject(), nil
			}))

			notFound := func(_ context.Context) (any, error) {
				return nil, tfe.ErrResourceNotFound
			}

			require.NoError(t, env.Mutable(t.Context(), relPath, notFound))

			// The object comes back: the returning content must append over
			// the tombstone so the timeline stays ordered, whether or not
			// the bytes changed.
			returning := tc.reappear()

			require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
				return returning, nil
			}))

			want, err := serialize.Marshal(returning)
			require.NoError(t, err)

			recs := sidecarRecords(t, st, relPath)
			require.Len(t, recs, 3)
			assert.True(t, recs[1].Deleted)
			assert.Equal(t, string(want), recs[2].Content,
				"the returning content closes the deletion")

			got, err := os.ReadFile(st.AbsPath(relPath))
			require.NoError(t, err)
			assert.Equal(t, want, got, "the file holds the returning content")

			entry, ok := ledger.Entry(relPath)
			require.True(t, ok)
			assert.Equal(t, manifest.StatusDone, entry.Status)

			// A second disappearance is a fresh observation and records again.
			require.NoError(t, env.Mutable(t.Context(), relPath, notFound))

			recs = sidecarRecords(t, st, relPath)
			require.Len(t, recs, 4)
			assert.True(t, recs[3].Deleted, "the second disappearance appends its own tombstone")
		})
	}
}

func TestEnvMutableCancellationSkipsBury(t *testing.T) {
	t.Parallel()

	// A wind-down records no outcome, so it must bury nothing either: the
	// object may be fine, and the next run will tell.
	const relPath = "projects/example/workspaces/ws/variables.json"

	env, st, _ := newEnv(t)

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := env.Mutable(ctx, relPath, func(_ context.Context) (any, error) {
		return nil, tfe.ErrResourceNotFound
	})
	require.ErrorIs(t, err, context.Canceled)

	assert.False(t, sidecarExists(t, st, relPath))
}

func TestEnvMutableHistoryFailureRecordsErrored(t *testing.T) {
	t.Parallel()

	// A history append that cannot land fails the whole write: the object
	// records errored so a re-run retries, and the file keeps its old
	// content rather than losing the superseded version.
	const relPath = "projects/example/workspaces/ws/variables.json"

	env, st, ledger := newEnv(t)

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return cannedProject(), nil
	}))

	// A directory squatting on the sidecar path makes the append fail.
	require.NoError(t, os.MkdirAll(st.AbsPath(st.HistoryPath(relPath)), 0o700))

	changed := cannedProject()
	changed.Name = "renamed"

	require.NoError(t, env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		return changed, nil
	}))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status)

	want, err := serialize.Marshal(cannedProject())
	require.NoError(t, err)

	got, err := os.ReadFile(st.AbsPath(relPath))
	require.NoError(t, err)
	assert.Equal(t, want, got, "the file keeps the old content")
}

func TestEnvBytes(t *testing.T) {
	t.Parallel()

	const relPath = "config-versions/cv-1.tar.gz"

	env, st, ledger := newEnv(t)

	err := env.Bytes(t.Context(), relPath, func(_ context.Context) ([]byte, error) {
		return []byte("tarball-bytes"), nil
	})
	require.NoError(t, err)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, entry.Status)
	assert.Equal(t, int64(len("tarball-bytes")), entry.Signature.Size)

	exists, existErr := st.Exists(relPath)
	require.NoError(t, existErr)
	assert.True(t, exists)
}

func TestEnvBytesEmptyIsNotApplicable(t *testing.T) {
	t.Parallel()

	// A 204 No Content answer (an absent structured plan JSON, for one) reaches
	// the fetch as empty bytes with no error; it must be recorded as a settled
	// gap rather than written as a zero-byte, unparseable file.
	const relPath = "projects/example/workspaces/ws/runs/run-1/plan.json"

	env, st, ledger := newEnv(t)

	called := false
	err := env.Bytes(t.Context(), relPath, func(_ context.Context) ([]byte, error) {
		called = true

		return []byte{}, nil
	})
	require.NoError(t, err)
	assert.True(t, called)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusNotApplicable, entry.Status)
	assert.Nil(t, entry.Signature, "an empty payload records no content signature")

	exists, existErr := st.Exists(relPath)
	require.NoError(t, existErr)
	assert.False(t, exists, "no file is written for an empty payload")

	assert.Equal(t, int64(0), ledger.Tally().BytesDownloaded)
}

func TestEnvBlobEmptyIsNotApplicable(t *testing.T) {
	t.Parallel()

	// A 204 No Content answer (a stack step that only planned, for one) reaches
	// the fetch as an empty reader with no error; like the buffered Bytes path it
	// must be recorded as a settled gap rather than streamed to a zero-byte file.
	const relPath = "projects/example/workspaces/ws/runs/run-1/apply.log"

	env, st, ledger := newEnv(t)

	called := false
	err := env.Blob(t.Context(), relPath, func(_ context.Context) (io.Reader, error) {
		called = true

		return strings.NewReader(""), nil
	})
	require.NoError(t, err)
	assert.True(t, called)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusNotApplicable, entry.Status)
	assert.Nil(t, entry.Signature, "an empty payload records no content signature")

	exists, existErr := st.Exists(relPath)
	require.NoError(t, existErr)
	assert.False(t, exists, "no file is written for an empty payload")

	assert.Equal(t, int64(0), ledger.Tally().BytesDownloaded)
}

func TestEnvBlobMidStreamReadErrorRecordsTransient(t *testing.T) {
	t.Parallel()

	// A stream that yields bytes and then fails with a network timeout is a fetch
	// failure, not a local write failure: it must route through the fetch
	// classification and record errored+transient so a re-run retries it and the
	// failure log names the real cause. In-run retrying is disabled so the
	// recorded outcome of a single attempt is what is under test.
	const relPath = "projects/example/workspaces/ws/runs/run-1/apply.log"

	env, st, ledger := newEnv(t, collect.WithBlobRetry(0, 0))

	err := env.Blob(t.Context(), relPath, func(_ context.Context) (io.Reader, error) {
		return io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(timeoutError{})), nil
	})
	require.NoError(t, err)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status)
	assert.True(t, entry.Transient, "a mid-stream network stall is recorded transient")
	assert.Contains(t, entry.LastError, "i/o timeout", "the read error, not the write wrapper, is recorded")

	exists, existErr := st.Exists(relPath)
	require.NoError(t, existErr)
	assert.False(t, exists, "a failed stream commits no file")
}

func TestEnvBlobRetry(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/workspaces/ws/runs/run-1/apply.log"

	fullPayload := strings.Repeat("apply output line\n", 8)

	tests := map[string]struct {
		fetch         func(attempts *int) func(context.Context) (io.Reader, error)
		retries       int
		wantAttempts  int
		wantStatus    manifest.Status
		wantTransient bool
		wantContent   string
	}{
		"a transient fetch error is retried to success": {
			retries: 2,
			fetch: func(attempts *int) func(context.Context) (io.Reader, error) {
				return func(_ context.Context) (io.Reader, error) {
					*attempts++
					if *attempts == 1 {
						return nil, timeoutError{}
					}

					return strings.NewReader(fullPayload), nil
				}
			},
			wantAttempts: 2,
			wantStatus:   manifest.StatusDone,
			wantContent:  fullPayload,
		},
		"a transient mid-stream error is retried to success": {
			retries: 2,
			fetch: func(attempts *int) func(context.Context) (io.Reader, error) {
				return func(_ context.Context) (io.Reader, error) {
					*attempts++
					if *attempts == 1 {
						return io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(timeoutError{})), nil
					}

					return strings.NewReader(fullPayload), nil
				}
			},
			wantAttempts: 2,
			wantStatus:   manifest.StatusDone,
			wantContent:  fullPayload,
		},
		"exhausted retries record errored transient": {
			retries: 2,
			fetch: func(attempts *int) func(context.Context) (io.Reader, error) {
				return func(_ context.Context) (io.Reader, error) {
					*attempts++

					return nil, timeoutError{}
				}
			},
			wantAttempts:  3,
			wantStatus:    manifest.StatusErrored,
			wantTransient: true,
		},
		"an unclassified error is not retried": {
			retries: 2,
			fetch: func(attempts *int) func(context.Context) (io.Reader, error) {
				return func(_ context.Context) (io.Reader, error) {
					*attempts++

					return nil, errors.New("boom")
				}
			},
			wantAttempts: 1,
			wantStatus:   manifest.StatusErrored,
		},
		"a terminal error is confirmed once, never retried further": {
			retries: 2,
			fetch: func(attempts *int) func(context.Context) (io.Reader, error) {
				return func(_ context.Context) (io.Reader, error) {
					*attempts++

					return nil, tfe.ErrResourceNotFound
				}
			},
			// The confirming re-probe is the only second attempt: a repeated 404
			// settles absent without consuming the transient retry budget.
			wantAttempts: 2,
			wantStatus:   manifest.StatusAbsent,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, st, ledger := newEnv(t, collect.WithBlobRetry(tc.retries, 0))

			attempts := 0

			err := env.Blob(t.Context(), relPath, tc.fetch(&attempts))
			require.NoError(t, err)

			assert.Equal(t, tc.wantAttempts, attempts)

			entry, ok := ledger.Entry(relPath)
			require.True(t, ok)
			assert.Equal(t, tc.wantStatus, entry.Status)
			assert.Equal(t, tc.wantTransient, entry.Transient)

			assert.Equal(t, int64(tc.wantAttempts-1), ledger.Tally().Retried,
				"each retry taken is tallied for progress")

			if tc.wantContent != "" {
				got, readErr := os.ReadFile(st.AbsPath(relPath))
				require.NoError(t, readErr)
				assert.Equal(t, tc.wantContent, string(got),
					"the committed file holds the retried full payload, never a partial stream")
			}
		})
	}
}

func TestEnvBlobRetryPropagatesCancellation(t *testing.T) {
	t.Parallel()

	// A cancellation mid-fetch classifies transient, but it must propagate and
	// record nothing rather than burn retries against a dead context.
	env, _, ledger := newEnv(t, collect.WithBlobRetry(2, 0))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	attempts := 0

	err := env.Blob(ctx, "projects/example/workspaces/ws/runs/run-1/apply.log",
		func(ctx context.Context) (io.Reader, error) {
			attempts++

			return nil, ctx.Err()
		})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts, "a canceled context is never retried")

	_, ok := ledger.Entry("projects/example/workspaces/ws/runs/run-1/apply.log")
	assert.False(t, ok, "a canceled fetch must not record an outcome")
}

func TestEnvBlobWriteFailureRecordsNonTransient(t *testing.T) {
	t.Parallel()

	// A write that fails with a healthy stream is a local store failure: it stays
	// errored+non-transient, distinct from the transient fetch class above. A
	// regular file squatting on the parent path makes the write fail without
	// touching the reader.
	env, st, ledger := newEnv(t)

	require.NoError(t, os.WriteFile(st.AbsPath("blocker"), []byte("x"), 0o600))

	const relPath = "blocker/apply.log"

	err := env.Blob(t.Context(), relPath, func(_ context.Context) (io.Reader, error) {
		return strings.NewReader("payload"), nil
	})
	require.NoError(t, err)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status)
	assert.False(t, entry.Transient, "a local write failure is not transient")
}

// walkItem is a listed element the walk describes and archives.
type walkItem struct {
	createdAt time.Time
	relPath   string
	terminal  bool
}

func TestWalkEarlyStopsWhenFullySettled(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A fully settled collection: every element terminal and archived done, the
	// collection walked to its end and recorded settled, with no unsettled child.
	// The walk must still stop at the newest boundary so the incremental
	// optimization is preserved.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	for _, it := range []walkItem{r3, r2, r1} {
		ledger.RecordDone(it.relPath, manifest.Signature{Hash: "prior", Size: 1})
	}

	ledger.Collection("runs").MarkComplete()
	ledger.Collection("runs").SetSettled(true)

	archived := map[string]int{}

	pagesRequested := 0
	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		pagesRequested = page

		if page == 1 {
			return []walkItem{r3, r2, r1}, true, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					archived[it.relPath]++

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe)
	require.NoError(t, err)

	// Only r3, the newest boundary, is refreshed; the walk halts before touching
	// the settled history below it or requesting a second page.
	assert.Equal(t, 1, archived[r3.relPath], "the newest boundary gets a final refresh")
	assert.NotContains(t, archived, r2.relPath, "settled history is not re-touched")
	assert.NotContains(t, archived, r1.relPath, "settled history is not re-touched")

	assert.Equal(t, 1, pagesRequested, "the walk halts before requesting a second page")

	assert.Equal(
		t,
		r3.createdAt,
		ledger.Collection("runs").HighWaterMark(),
		"the mark advances to the newest element seen",
	)

	assert.True(t, ledger.Collection("runs").Settled(),
		"a boundary-only refresh mutates nothing and leaves settlement in place")
}

func TestWalkInterruptedRewalkUnsettles(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A settled collection gains new elements, and the re-walk archiving them
	// aborts before listing them all: page 1's element lands as a frozen done
	// entry, but the older new element is never listed and leaves no ledger
	// record. Were the stale settled flag to survive, the next run's early
	// stop would halt at the page-1 element and strand the unlisted one
	// forever — a silent permanent gap the seal and the remote mirror would
	// then enshrine as complete history.
	r4 := walkItem{relPath: "runs/r4/run.json", createdAt: base.Add(4 * time.Hour), terminal: true}
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	for _, it := range []walkItem{r2, r1} {
		ledger.RecordDone(it.relPath, manifest.Signature{Hash: "prior", Size: 1})
	}

	ledger.Collection("runs").MarkComplete()
	ledger.Collection("runs").SetSettled(true)

	var mu sync.Mutex

	archived := map[string]int{}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					mu.Lock()

					archived[it.relPath]++

					mu.Unlock()

					return cannedProject(), nil
				})
			},
		}
	}

	// The interrupted re-walk: page 1 archives r4, page 2's fetch fails.
	interrupted := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r4}, true, nil
		}

		return nil, false, errors.New("listing failed")
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), interrupted, describe)
	require.Error(t, err)

	assert.Equal(t, 1, archived[r4.relPath], "the interrupted walk archived the page-1 element")
	assert.False(t, ledger.Collection("runs").Settled(),
		"an interrupted mutation withdraws settlement before the next run can early-stop over the gap")

	// The next run must page past r4's frozen boundary and reach the element
	// the interruption stranded.
	full := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r4, r3, r2, r1}, false, nil
		}

		return nil, false, nil
	}

	require.NoError(t, collect.Walk(t.Context(), env, env.Collection("runs"), full, describe))

	assert.Equal(t, 1, archived[r3.relPath], "the stranded element is archived by the next full walk")
	assert.True(t, ledger.Collection("runs").Settled(), "the completed walk re-earns settlement")
}

func TestWalkEarlyStopResettlesAfterDelta(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A settled collection with one new terminal element: the walk withdraws
	// settlement to archive it, stops at the frozen boundary below, and must
	// re-earn the flag — otherwise every delta would force the next run to
	// re-page the whole collection, forfeiting the incremental optimization.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	for _, it := range []walkItem{r2, r1} {
		ledger.RecordDone(it.relPath, manifest.Signature{Hash: "prior", Size: 1})
	}

	ledger.Collection("runs").MarkComplete()
	ledger.Collection("runs").SetSettled(true)

	var mu sync.Mutex

	archived := map[string]int{}

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r3, r2, r1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					mu.Lock()

					archived[it.relPath]++

					mu.Unlock()

					return cannedProject(), nil
				})
			},
		}
	}

	require.NoError(t, collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe))

	assert.Equal(t, 1, archived[r3.relPath], "the new element is archived")
	assert.Equal(t, 1, archived[r2.relPath], "the frozen boundary gets its final refresh")
	assert.NotContains(t, archived, r1.relPath, "settled history below the boundary is not re-touched")

	assert.True(t, ledger.Collection("runs").Settled(),
		"an early stop that archived its delta clean re-earns settlement")
}

func TestWalkRetryAbsentPiercesEarlyStop(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A fully settled collection whose oldest run has an absent child (an
	// expired plan.json settled by a confirmed 404). A normal run early-stops
	// at the newest boundary, so the absence is never re-probed; under
	// retry-absent the absence must re-open the early stop, or the flag is
	// silently inert for the dominant absent population.
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	const absentChild = "runs/r1/plan.json"

	for name, retryAbsent := range map[string]bool{"normal": false, "retry-absent": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			st := store.New(root)

			ledger, err := manifest.Load(root,
				manifest.WithClock(fixedClock()), manifest.WithRetryAbsent(retryAbsent))
			require.NoError(t, err)

			env := collect.NewEnv(nil, st, ledger, collect.WithAbsentConfirm(0))

			for _, it := range []walkItem{r2, r1} {
				ledger.RecordDone(it.relPath, manifest.Signature{Hash: "prior", Size: 1})
			}

			ledger.RecordAbsent(absentChild, errors.New("404"))
			ledger.Collection("runs").MarkComplete()
			ledger.Collection("runs").SetSettled(true)

			reprobes := 0
			pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
				if page == 1 {
					return []walkItem{r2, r1}, false, nil
				}

				return nil, false, nil
			}

			describe := func(it walkItem) collect.Item {
				return collect.Item{
					RelPath:   it.relPath,
					CreatedAt: it.createdAt,
					Terminal:  it.terminal,
					Archive: func(ctx context.Context) error {
						if it.relPath != r1.relPath {
							return nil
						}

						// The absent child re-probes only if the walk reaches
						// r1 at all and ShouldFetch admits the absence.
						return env.Object(ctx, absentChild, func(_ context.Context) (any, error) {
							reprobes++

							return cannedProject(), nil
						})
					},
				}
			}

			require.NoError(t, collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe))

			if retryAbsent {
				assert.Equal(t, 1, reprobes,
					"retry-absent must reach and re-probe an absence below the early-stop boundary")
			} else {
				assert.Zero(t, reprobes,
					"a normal run early-stops at the boundary and leaves the absence settled")
			}
		})
	}
}

func TestWalkPagesPastNonTerminalBoundary(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A newer run finished terminal while an older run is still in flight
	// (recorded done but non-terminal, as a run archives its summary done and
	// defers its children). Old behavior stopped at the newer terminal boundary
	// and stranded the older run's pending children; the fix must page past it.
	newer := walkItem{relPath: "runs/rNew/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	older := walkItem{relPath: "runs/rOld/run.json", createdAt: base.Add(1 * time.Hour), terminal: false}

	env, _, ledger := newEnv(t)

	ledger.RecordDone(newer.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(older.relPath, manifest.Signature{Hash: "prior", Size: 1})

	// The collection completed a prior walk, so the old early-stop would fire at
	// the newer boundary; settled is unset, so the fix keeps paging.
	ledger.Collection("runs").MarkComplete()

	// A page's items archive concurrently, so the recording map needs a lock.
	var mu sync.Mutex

	archived := map[string]int{}

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{newer, older}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					mu.Lock()

					archived[it.relPath]++
					mu.Unlock()

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe)
	require.NoError(t, err)

	// The walk pages past the newer terminal boundary and revisits the older
	// in-flight run, which old behavior would have stranded.
	assert.Equal(t, 1, archived[newer.relPath], "the newer terminal boundary is refreshed")
	assert.Equal(t, 1, archived[older.relPath], "the older in-flight run is reached and refreshed")

	// Seeing a non-terminal element leaves the collection unsettled, so the next
	// run re-pages until it finishes.
	assert.False(t, ledger.Collection("runs").Settled(), "an in-flight run keeps the collection unsettled")
	assert.True(t, ledger.Collection("runs").Complete(), "reaching the final page still marks completion")
}

func TestWalkReachesErroredChildBelowBoundary(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// Both runs are terminal and their summaries archived done, and the
	// collection is marked complete and settled, yet an older run's child log
	// errored in a prior run and was never retried. The start gate's
	// HasUnsettledUnder check must force a full re-walk so the errored child is
	// reached.
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	ledger.RecordDone(r2.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(r1.relPath, manifest.Signature{Hash: "prior", Size: 1})

	const erroredChild = "runs/r1/plan.log"

	ledger.RecordErrored(erroredChild, errors.New("prior boom"), true)

	ledger.Collection("runs").MarkComplete()
	// The steady-state hole: a prior all-terminal walk recorded settled even
	// though a child stayed errored. The start gate closes it.
	ledger.Collection("runs").SetSettled(true)

	archivedChild := 0

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r2, r1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				err := env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					return cannedProject(), nil
				})
				if err != nil {
					return fmt.Errorf("archive run summary: %w", err)
				}

				if it.relPath != r1.relPath {
					return nil
				}

				// The older run retries its errored child once the walk reaches it.
				return env.Object(ctx, erroredChild, func(_ context.Context) (any, error) {
					archivedChild++

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe)
	require.NoError(t, err)

	assert.Equal(t, 1, archivedChild, "the errored child below the done boundary is reached and retried")

	child, ok := ledger.Entry(erroredChild)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, child.Status, "the retried child is now settled done")

	// With the child fixed, the completing walk records the collection settled
	// again, so a later run may early-stop.
	assert.True(t, ledger.Collection("runs").Settled(), "the collection settles once no unsettled child remains")
}

func TestWalkResumesIncompleteCollection(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// An interrupted first run archived the newest elements (r3, r2) but never
	// reached the older tail (r1) and never completed the walk.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	// The prior run recorded the newest elements done but was interrupted before
	// completing the collection, so it is not marked complete.
	ledger.RecordDone(r3.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(r2.relPath, manifest.Signature{Hash: "prior", Size: 1})

	// A page's items archive concurrently, so the recording map needs a lock.
	var mu sync.Mutex

	archived := map[string]int{}

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r3, r2, r1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Object(ctx, it.relPath, func(_ context.Context) (any, error) {
					mu.Lock()

					archived[it.relPath]++
					mu.Unlock()

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe)
	require.NoError(t, err)

	// The walk does not stop at the newest already-done boundary; it pages all
	// the way down and reaches the un-archived older tail.
	assert.Equal(t, 1, archived[r1.relPath], "the un-archived older tail is reached and archived")
	assert.True(t, ledger.Collection("runs").Complete(), "reaching the final page marks the collection complete")
}

// stackConfigArchivePrefix is the archive prefix of the scenario's
// configuration entries, the collection identity the walk opens its ledger
// handle on.
const stackConfigArchivePrefix = "projects/p/stacks/s/configurations"

// erroredChildScenario is a Walk mimicking a stack's configuration walk, with
// an older configuration's child left errored below a newer done boundary.
type erroredChildScenario struct {
	env           *collect.Env
	ledger        *manifest.Ledger
	pager         collect.Pager[walkItem]
	describe      func(walkItem) collect.Item
	erroredChild  string
	archivedChild *int
}

// newErroredChildScenario seeds two terminal configurations archived done in a
// stack shard, marks the collection complete and settled under its synthetic
// cursor, and leaves the older configuration's child errored. The older
// configuration's Archive closure retries that child and bumps a counter.
func newErroredChildScenario(t *testing.T) *erroredChildScenario {
	t.Helper()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	cfg2 := walkItem{
		relPath:   "projects/p/stacks/s/configurations/cfg2/configuration.json",
		createdAt: base.Add(2 * time.Hour),
		terminal:  true,
	}
	cfg1 := walkItem{
		relPath:   "projects/p/stacks/s/configurations/cfg1/configuration.json",
		createdAt: base.Add(1 * time.Hour),
		terminal:  true,
	}

	env, _, ledger := newEnv(t)

	ledger.RecordDone(cfg2.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(cfg1.relPath, manifest.Signature{Hash: "prior", Size: 1})

	const erroredChild = "projects/p/stacks/s/configurations/cfg1/json-schemas.json"

	ledger.RecordErrored(erroredChild, errors.New("prior boom"), true)

	ledger.Collection(stackConfigArchivePrefix).MarkComplete()
	ledger.Collection(stackConfigArchivePrefix).SetSettled(true)

	archivedChild := 0

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{cfg2, cfg1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				err := env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					return cannedProject(), nil
				})
				if err != nil {
					return fmt.Errorf("archive configuration: %w", err)
				}

				if it.relPath != cfg1.relPath {
					return nil
				}

				return env.Object(ctx, erroredChild, func(_ context.Context) (any, error) {
					archivedChild++

					return cannedProject(), nil
				})
			},
		}
	}

	return &erroredChildScenario{
		env:           env,
		ledger:        ledger,
		pager:         pager,
		describe:      describe,
		erroredChild:  erroredChild,
		archivedChild: &archivedChild,
	}
}

func TestWalkGateReachesErroredChildUnderPrefix(t *testing.T) {
	t.Parallel()

	s := newErroredChildScenario(t)

	// The handle's gate scans the collection prefix, the directory that holds
	// the entries, so an errored child below a done boundary suppresses the
	// early stop: the walk re-pages past the boundary and retries it. Before
	// the handle owned the keying, a synthetic cursor could point the gate at
	// the empty org-root shard and strand the child forever.
	err := collect.Walk(t.Context(), s.env, s.env.Collection(stackConfigArchivePrefix), s.pager, s.describe)
	require.NoError(t, err)

	assert.Equal(t, 1, *s.archivedChild, "the gate reaches the errored child under the prefix")

	child, ok := s.ledger.Entry(s.erroredChild)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, child.Status, "the retried child settles done")
	assert.True(t, s.ledger.Collection(stackConfigArchivePrefix).Settled(),
		"the collection settles once no unsettled child remains")
}

func TestWalkFinalPageRecomputesSettledFromArchivePrefix(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A first full walk reaches the final page, where it recomputes the settled
	// flag from the collection's own gate: an errored child under the prefix
	// must record the collection unsettled so the next run re-walks it.
	const (
		archivePrefix = "projects/p/stacks/s2/configurations"
		erroredChild  = archivePrefix + "/cfg1/json-schemas.json"
	)

	cfg2 := walkItem{
		relPath:   archivePrefix + "/cfg2/configuration.json",
		createdAt: base.Add(2 * time.Hour),
		terminal:  true,
	}
	cfg1 := walkItem{
		relPath:   archivePrefix + "/cfg1/configuration.json",
		createdAt: base.Add(1 * time.Hour),
		terminal:  true,
	}

	env, _, ledger := newEnv(t)

	// An errored child left under the prefix that the walk does not retry.
	ledger.RecordErrored(erroredChild, errors.New("prior boom"), true)

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{cfg2, cfg1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, env.Collection(archivePrefix), pager, describe)
	require.NoError(t, err)

	assert.True(t, ledger.Collection(archivePrefix).Complete(), "the final page marks the collection complete")
	assert.False(t, ledger.Collection(archivePrefix).Settled(),
		"an errored child under the archive prefix records the collection unsettled")
}

// walLine is one parsed org-log record, reduced to the fields a placement
// assertion needs.
type walLine struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Shard string `json:"shard"`
}

// walLines reads the org-level ledger log and returns its records per key, so
// a test can assert which shard a flag record was tagged with.
func walLines(t *testing.T, path string) map[string][]walLine {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := map[string][]walLine{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var rec walLine

		require.NoError(t, json.Unmarshal([]byte(line), &rec))

		lines[rec.Key] = append(lines[rec.Key], rec)
	}

	return lines
}

func TestWalkSyntheticCursorFlagsShareTheEntriesShard(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// The crash fence's durability order — the unsettle record ahead of the
	// entries it guards, settlement and completion behind them — holds only
	// within one shard's log append. A synthetic cursor key routes to the
	// org-root shard while the entries live in the stack shard, so flags keyed
	// on the cursor would flush in a separate append from the entries, in no
	// guaranteed order: a crash between the two appends could durably freeze
	// new entries under a stale settled flag, and the next run's early stop
	// would permanently strand elements the interrupted walk never listed. The
	// walk must therefore key the flags on the archive prefix.
	const (
		cursorKey     = "stacks/stk-3/configurations"
		archivePrefix = "projects/p/stacks/s3/configurations"
	)

	cfg2 := walkItem{
		relPath:   archivePrefix + "/cfg2/configuration.json",
		createdAt: base.Add(2 * time.Hour),
		terminal:  true,
	}
	cfg1 := walkItem{
		relPath:   archivePrefix + "/cfg1/configuration.json",
		createdAt: base.Add(1 * time.Hour),
		terminal:  true,
	}

	env, st, ledger := newEnv(t)

	// A page's items archive concurrently, so the recording map needs a lock.
	var mu sync.Mutex

	archived := map[string]int{}

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{cfg2, cfg1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					mu.Lock()

					archived[it.relPath]++
					mu.Unlock()

					return cannedProject(), nil
				})
			},
		}
	}

	require.NoError(t, collect.Walk(t.Context(), env, env.Collection(archivePrefix), pager, describe))

	// Every piece of collection state keys on the prefix the handle was opened
	// on.
	assert.True(t, ledger.Collection(archivePrefix).Complete(), "completion is keyed on the prefix")
	assert.True(t, ledger.Collection(archivePrefix).Settled(), "settlement is keyed on the prefix")
	assert.Equal(t, cfg2.createdAt, ledger.Collection(archivePrefix).HighWaterMark(),
		"the high-water mark is keyed on the prefix")

	// The next walk reads the flags back from the prefix: it early-stops at the
	// frozen cfg2 boundary and never touches cfg1's Archive again.
	require.NoError(t, collect.Walk(t.Context(), env, env.Collection(archivePrefix), pager, describe))

	assert.Equal(t, 2, archived[cfg2.relPath], "the boundary gets its refresh, so the flags were read back")
	assert.Equal(t, 1, archived[cfg1.relPath], "the early stop halts above settled history")

	// After a flush every collection record — flags and high-water mark alike —
	// carries the stack shard's tag, routing it back to the shard that owns the
	// entries it describes.
	require.NoError(t, ledger.Flush())

	orgLog := st.AbsPath(st.Join(manifest.LedgerDirName, manifest.LogFileName))
	lines := walLines(t, orgLog)

	kinds := make([]string, 0, len(lines[archivePrefix]))

	for _, rec := range lines[archivePrefix] {
		kinds = append(kinds, rec.Kind)
		assert.Equal(t, "projects/p/stacks/s3", rec.Shard,
			"the %s record is tagged with the entries' shard", rec.Kind)
	}

	assert.Contains(t, kinds, "completed", "the completion record is logged under the prefix")
	assert.Contains(t, kinds, "settled", "the settled record is logged under the prefix")
	assert.Contains(t, kinds, "watermark", "the high-water mark is logged under the prefix")
}

func TestWalkHistoryLimit(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// Three fresh terminal items newest-first, one per page, so a limit stop is
	// observable both as an unarchived tail and as pages never requested.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	tests := map[string]struct {
		oldest       time.Time
		count        int
		wantArchived []string
		wantSettled  bool
	}{
		"count bound stops before the excluded tail": {
			count:        2,
			wantArchived: []string{r3.relPath, r2.relPath},
		},
		"age bound is inclusive of its boundary instant": {
			oldest:       r2.createdAt,
			wantArchived: []string{r3.relPath, r2.relPath},
		},
		"a wider count bound wins over a narrower age": {
			count:        2,
			oldest:       r3.createdAt,
			wantArchived: []string{r3.relPath, r2.relPath},
		},
		"a wider age bound wins over a narrower count": {
			count:        1,
			oldest:       r2.createdAt,
			wantArchived: []string{r3.relPath, r2.relPath},
		},
		"zero bounds leave the walk unbounded": {
			wantArchived: []string{r3.relPath, r2.relPath, r1.relPath},
			wantSettled:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, _, ledger := newEnv(t)

			pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
				items := []walkItem{r3, r2, r1}
				if page > len(items) {
					return nil, false, nil
				}

				return items[page-1 : page], page < len(items), nil
			}

			archived := map[string]int{}

			describe := func(it walkItem) collect.Item {
				return collect.Item{
					RelPath:   it.relPath,
					CreatedAt: it.createdAt,
					Terminal:  it.terminal,
					Archive: func(ctx context.Context) error {
						return env.Object(ctx, it.relPath, func(_ context.Context) (any, error) {
							archived[it.relPath]++

							return cannedProject(), nil
						})
					},
				}
			}

			err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe,
				collect.WithHistoryLimit(tc.count, tc.oldest))
			require.NoError(t, err)

			for _, relPath := range tc.wantArchived {
				assert.Equal(t, 1, archived[relPath], "%s is within the bounds and archives once", relPath)
			}

			assert.Len(t, archived, len(tc.wantArchived), "nothing beyond the bounds is archived")

			// A fully-walked slice records completion whether bounded or not, so
			// the seal phase can bundle it; only a walk that reached the true end
			// also records settlement, which keeps the early stop disabled for a
			// bounded walk so a later wider limit still pages down.
			assert.True(t, ledger.Collection("runs").Complete(),
				"a fully-walked slice records completion whether or not it is bounded")
			assert.Equal(t, tc.wantSettled, ledger.Collection("runs").Settled(),
				"only a walk that reaches the collection's true end records settlement")
		})
	}
}

func TestWalkPropagatesPageError(t *testing.T) {
	t.Parallel()

	env, _, _ := newEnv(t)

	wantErr := tfe.ErrResourceNotFound
	pager := func(_ context.Context, _ int) ([]walkItem, bool, error) {
		return nil, false, wantErr
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, func(walkItem) collect.Item {
		return collect.Item{}
	})
	require.ErrorIs(t, err, wantErr)
}

func TestWalkArchivesPageItemsConcurrently(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// Two fresh items on one page. Each Archive closure blocks until the other
	// has started, so the walk completes only if the page's items run
	// concurrently; a sequential walk would park the first closure until its
	// timeout and fail the test with its error.
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, _ := newEnv(t)

	started := make(chan string, 2)
	release := make(chan struct{})

	go func() {
		<-started
		<-started
		close(release)
	}()

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r2, r1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				started <- it.relPath

				select {
				case <-release:
				case <-time.After(5 * time.Second):
					return errors.New("page sibling never started: items archived sequentially")
				}

				return env.Object(ctx, it.relPath, func(_ context.Context) (any, error) {
					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, env.Collection("runs"), pager, describe)
	require.NoError(t, err)
}

func TestEnvObjectAbsenceConfirmedInRun(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/project.json"

	env, _, ledger := newEnv(t)

	fetches := 0
	fetch := func(_ context.Context) (any, error) {
		fetches++

		return nil, tfe.ErrResourceNotFound
	}

	// A first 404 is confirmed by one in-run re-probe; the repeated 404 settles
	// the object absent.
	ledger.StartRun()
	require.NoError(t, env.Object(t.Context(), relPath, fetch))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusAbsent, entry.Status)
	assert.Equal(t, 2, fetches, "the confirming re-probe is the only second attempt")

	// Settled: no further probes, this run or the next.
	require.NoError(t, env.Object(t.Context(), relPath, fetch))
	assert.Equal(t, 2, fetches, "a settled absence is never re-probed")
}

func TestEnvObjectAbsenceBlipRecovers(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/project.json"

	env, _, ledger := newEnv(t)

	// A 404 answered out of eventual consistency succeeds on the confirming
	// re-probe, so the blip lands on the success path instead of settling a gap.
	fetches := 0
	fetch := func(_ context.Context) (any, error) {
		fetches++
		if fetches == 1 {
			return nil, tfe.ErrResourceNotFound
		}

		return cannedProject(), nil
	}

	ledger.StartRun()
	require.NoError(t, env.Object(t.Context(), relPath, fetch))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, entry.Status)
	assert.Equal(t, int64(1), ledger.Tally().Retried, "the confirming re-probe tallies as a retry")
}
