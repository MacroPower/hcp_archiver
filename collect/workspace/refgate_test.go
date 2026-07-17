package workspace_test

import (
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// serveEmptyRunChildren registers valid, empty responses for every run-child
// endpoint the run walk touches beyond the one under test, so a strand+recover
// test can isolate the reference gate as the sole unsettled child: an unrelated
// errored child would independently keep HasUnsettledUnder true and pass the
// assertions for the wrong reason. Each id gets its list children (comments, run
// events, policy checks, task stages) and its own run read (no native policy
// evaluations), leaving no gate of their own.
func serveEmptyRunChildren(t *testing.T, mux *http.ServeMux, ids ...string) {
	t.Helper()

	emptyList := func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, listPayload(0))
	}

	for _, id := range ids {
		mux.HandleFunc("/api/v2/runs/"+id+"/comments", emptyList)
		mux.HandleFunc("/api/v2/runs/"+id+"/run-events", emptyList)
		mux.HandleFunc("/api/v2/runs/"+id+"/policy-checks", emptyList)
		mux.HandleFunc("/api/v2/runs/"+id+"/task-stages", emptyList)
		mux.HandleFunc("/api/v2/runs/"+id, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.Run{ID: id}))
		})
	}
}

func TestArchiveRunEventsReReadsActorsWhileGateOpen(t *testing.T) {
	t.Parallel()

	// A single hydrated actor whose users/<id>.json write failed on an earlier
	// pass: run-events.json is already settled, but the actor lives only in the
	// list include, so the union gate must force the read again to re-derive it.
	events := []*tfe.RunEvent{
		{ID: "re-1", Action: "created", Actor: &tfe.User{ID: "user-1", Username: "alice"}},
	}

	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1/run-events", func(w http.ResponseWriter, _ *http.Request) {
		hits++

		writeJSONAPI(t, w, marshalJSONAPI(t, events))
	})

	f := newWSFixture(t, mux)
	st := f.store

	eventsPath := st.RunFile("proj", "ws", "run-1", "run-events.json")
	actorsGate := st.RunFile("proj", "ws", "run-1", "run-events-actors.ref")

	f.preSettle(eventsPath)
	f.ledger.MirrorReference(actorsGate, false)
	require.Equal(t, manifest.StatusPending, f.status(actorsGate))

	require.NoError(t, f.collector.ArchiveRunEvents(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	assert.Equal(t, 1, hits, "a pending gate forces the read even when run-events.json is settled")

	alice := f.attrs(t, st.User("user-1"), "users", "user-1")
	assert.Equal(t, "alice", alice["username"])

	assert.Equal(t, manifest.StatusReferenceCleared, f.status(actorsGate),
		"the gate clears once the actor is captured")
	assert.Equal(t, manifest.StatusDone, f.status(eventsPath),
		"the settled events file is not regressed by the pure re-read")
}

func TestArchiveRunEventsCleanRunLeavesNoGate(t *testing.T) {
	t.Parallel()

	// On a clean run every actor write succeeds, so mirroring a settled reference
	// against an absent gate is a no-op: no *.ref entry is created.
	events := []*tfe.RunEvent{
		{ID: "re-1", Action: "created", Actor: &tfe.User{ID: "user-1", Username: "alice"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1/run-events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, events))
	})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveRunEvents(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	assert.Equal(t, manifest.StatusDone, f.status(st.User("user-1")))

	_, ok := f.ledger.Entry(st.RunFile("proj", "ws", "run-1", "run-events-actors.ref"))
	assert.False(t, ok, "a clean run leaves no actors reference gate")
}

func TestArchivePolicyChecksReReadsLogsWhileGateOpen(t *testing.T) {
	t.Parallel()

	// A single policy check whose log did not settle on an earlier pass. The
	// checks file is already settled Done -- logBlob records a log failure and
	// returns nil, so the list settles regardless -- yet the log must still be
	// retried. The union gate forces the metered list again so the log re-fetches,
	// where ShouldFetch(policy-checks.json) alone would have stranded it.
	var listHits, logHits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1/policy-checks", func(w http.ResponseWriter, _ *http.Request) {
		listHits++

		writeJSONAPI(t, w, marshalJSONAPI(t, []*tfe.PolicyCheck{{ID: "pc1", Status: tfe.PolicyPasses}}))
	})
	// PolicyChecks.Logs re-reads the check for its status before fetching output.
	mux.HandleFunc("/api/v2/policy-checks/pc1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.PolicyCheck{ID: "pc1", Status: tfe.PolicyPasses}))
	})
	mux.HandleFunc("/api/v2/policy-checks/pc1/output", func(w http.ResponseWriter, _ *http.Request) {
		logHits++

		_, werr := io.WriteString(w, "policy log bytes")
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	checksPath := st.RunFile("proj", "ws", "run-1", "policy-checks.json")
	logsGate := st.RunFile("proj", "ws", "run-1", "policy-check-logs.ref")
	logPath := st.RunFile("proj", "ws", "run-1", "policy-check-pc1.log")

	f.preSettle(checksPath)
	f.ledger.MirrorReference(logsGate, false)
	require.Equal(t, manifest.StatusPending, f.status(logsGate))

	require.NoError(t, f.collector.ArchivePolicyChecks(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	assert.Equal(t, 1, listHits, "a pending gate forces the list even when policy-checks.json is settled")
	assert.Equal(t, 1, logHits, "the stranded log is re-fetched")
	assert.Equal(t, manifest.StatusDone, f.status(logPath), "the recovered log is captured")
	assert.Equal(t, manifest.StatusReferenceCleared, f.status(logsGate), "the gate clears once the log settles")
	assert.Equal(t, manifest.StatusDone, f.status(checksPath), "the settled checks file is not regressed")
}

func TestArchivePolicyChecksCleanRunLeavesNoGate(t *testing.T) {
	t.Parallel()

	// On a clean run the check's log write succeeds, so mirroring a settled
	// reference against an absent gate is a no-op: no *.ref entry is created, and a
	// re-run skips the metered list rather than re-listing forever.
	var listHits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1/policy-checks", func(w http.ResponseWriter, _ *http.Request) {
		listHits++

		writeJSONAPI(t, w, marshalJSONAPI(t, []*tfe.PolicyCheck{{ID: "pc1", Status: tfe.PolicyPasses}}))
	})
	mux.HandleFunc("/api/v2/policy-checks/pc1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.PolicyCheck{ID: "pc1", Status: tfe.PolicyPasses}))
	})
	mux.HandleFunc("/api/v2/policy-checks/pc1/output", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, "policy log bytes")
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store
	run := &tfe.Run{ID: "run-1"}

	require.NoError(t, f.collector.ArchivePolicyChecks(t.Context(), "proj", "ws", run))

	assert.Equal(t, manifest.StatusDone, f.status(st.RunFile("proj", "ws", "run-1", "policy-check-pc1.log")))

	_, ok := f.ledger.Entry(st.RunFile("proj", "ws", "run-1", "policy-check-logs.ref"))
	assert.False(t, ok, "a clean run leaves no policy-check logs reference gate")

	require.NoError(t, f.collector.ArchivePolicyChecks(t.Context(), "proj", "ws", run))
	assert.Equal(t, 1, listHits, "a settled run with no open gate is not re-listed")
}

func TestArchiveConfigurationVersionTarballStrandsAndRecovers(t *testing.T) {
	t.Parallel()

	// The tarball is deduped org-wide under config-versions/, outside the run's
	// subtree. A regular file squatting on that directory makes the tarball write
	// fail; its errored entry is invisible to the runs walk's retry gate, so the
	// run-scoped reference gate is what keeps the failure retryable.
	const tarball = "\x1f\x8b\x08 fake tar.gz bytes \x00\x01\x02"

	cv := &tfe.ConfigurationVersion{ID: "cv-1", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})
	mux.HandleFunc("/api/v2/configuration-versions/cv-1/download", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, tarball)
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	tarballPath := st.ConfigVersionTarball("cv-1")
	gate := st.RunFile("proj", "ws", "run-1", "config-version-tarball.ref")
	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}

	// Pass 1: the squat makes the tarball write fail, opening the gate.
	require.NoError(t, os.WriteFile(st.AbsPath("config-versions"), []byte("x"), 0o600))
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, manifest.StatusErrored, f.status(tarballPath), "the foreign tarball write failed")
	assert.Equal(t, manifest.StatusPending, f.status(gate), "the reference gate mirrors the failed write")

	// Pass 2: clear the obstacle; the tarball streams and the gate clears.
	require.NoError(t, os.Remove(st.AbsPath("config-versions")))
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, manifest.StatusDone, f.status(tarballPath), "the tarball is captured on recovery")
	assert.Equal(t, manifest.StatusReferenceCleared, f.status(gate), "the gate clears once the tarball settles")
}

func TestArchiveRunChildrenCreatedByStrandsAndRecovers(t *testing.T) {
	t.Parallel()

	// The created-by user lands in the org-root users/ shard, outside the run's
	// subtree. Squatting users/ makes that write fail; the gate under the run's own
	// prefix is what makes the failure visible to HasUnsettledUnder.
	mux := http.NewServeMux()
	serveEmptyRunChildren(t, mux, "run-1")

	f := newWSFixture(t, mux)
	st := f.store

	const runsKey = "projects/proj/workspaces/ws/runs"

	userPath := st.User("user-1")
	gate := st.RunFile("proj", "ws", "run-1", "created-by.ref")
	run := &tfe.Run{ID: "run-1", CreatedBy: &tfe.User{ID: "user-1", Username: "alice"}}

	// Pass 1: the squat strands the created-by user behind an errored write.
	require.NoError(t, os.WriteFile(st.AbsPath("users"), []byte("x"), 0o600))
	require.NoError(t, f.collector.ArchiveRunChildren(t.Context(), "proj", "ws", run))

	assert.Equal(t, manifest.StatusErrored, f.status(userPath), "the foreign user write failed")
	assert.Equal(t, manifest.StatusPending, f.status(gate), "the reference gate mirrors the failed write")
	assert.True(t, f.ledger.Collection(runsKey).HasUnsettled(),
		"the gate makes the stranded user visible to the runs walk's retry gate")

	// Pass 2: clear the obstacle; the user is captured and the gate clears.
	require.NoError(t, os.Remove(st.AbsPath("users")))
	require.NoError(t, f.collector.ArchiveRunChildren(t.Context(), "proj", "ws", run))

	alice := f.attrs(t, userPath, "users", "user-1")
	assert.Equal(t, "alice", alice["username"])
	assert.Equal(t, manifest.StatusReferenceCleared, f.status(gate), "the gate clears once the user settles")
	assert.False(t, f.ledger.Collection(runsKey).HasUnsettled(), "the run has no outstanding work once captured")
}

func TestCollectRunsStrandsAndRecoversCreatedByAcrossWalk(t *testing.T) {
	t.Parallel()

	// Two terminal runs, both with a hydrated created-by. The runs walk always
	// re-archives the newest terminal run as its boundary, so the older run is the
	// one at risk of stranding: its gate must flip the collection unsettled and
	// disable the early stop until the foreign write recovers.
	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	runs := []*tfe.Run{
		{
			ID: "run-2", Status: tfe.RunApplied, CreatedAt: base.Add(2 * time.Hour),
			CreatedBy: &tfe.User{ID: "user-2", Username: "bob"},
		},
		{
			ID: "run-1", Status: tfe.RunApplied, CreatedAt: base.Add(1 * time.Hour),
			CreatedBy: &tfe.User{ID: "user-1", Username: "alice"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, runs))
	})
	serveEmptyRunChildren(t, mux, "run-1", "run-2")

	f := newWSFixture(t, mux)
	st := f.store
	ws := &tfe.Workspace{ID: "ws-1", Name: "ws"}

	const runsKey = "projects/proj/workspaces/ws/runs"

	olderGate := st.RunFile("proj", "ws", "run-1", "created-by.ref")

	// Pass 1: the squat strands both runs' created-by writes.
	require.NoError(t, os.WriteFile(st.AbsPath("users"), []byte("x"), 0o600))
	require.NoError(t, f.collector.CollectRuns(t.Context(), "proj", ws, nil))

	assert.Equal(t, manifest.StatusPending, f.status(olderGate),
		"the older, non-boundary run's gate is left open")
	assert.True(t, f.ledger.Collection(runsKey).HasUnsettled(), "the open gate keeps the collection with work to do")
	assert.False(t, f.ledger.Collection(runsKey).Settled(),
		"a pending gate withholds settlement, so the next walk re-pages past the boundary")

	// Pass 2: clear the obstacle; the re-walk reaches the older run and recovers it.
	require.NoError(t, os.Remove(st.AbsPath("users")))
	require.NoError(t, f.collector.CollectRuns(t.Context(), "proj", ws, nil))

	assert.Equal(t, manifest.StatusDone, f.status(st.User("user-1")), "the older run's user is captured")
	assert.Equal(t, manifest.StatusReferenceCleared, f.status(olderGate), "its gate clears")
	assert.False(t, f.ledger.Collection(runsKey).HasUnsettled(), "no run has outstanding work")
	assert.True(t, f.ledger.Collection(runsKey).Settled(), "the collection settles once every gate clears")
}
