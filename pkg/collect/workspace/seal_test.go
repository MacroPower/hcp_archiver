package workspace_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/pkg/logtest"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// sealFixture builds a workspace collector over a temp-dir store and ledger, with
// no client, so the seal phase can be exercised without a network.
type sealFixture struct {
	collector *workspace.Collector
	store     *store.Store
	ledger    *manifest.Ledger
}

func newSealFixture(t *testing.T) sealFixture {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	return sealFixture{
		collector: workspace.New(collect.NewEnv(nil, st, ledger), "org"),
		store:     st,
		ledger:    ledger,
	}
}

// newSealFixtureLogged is [newSealFixture] with the environment's logger
// captured by rec, for tests that assert on what the seal phase announces.
func newSealFixtureLogged(t *testing.T, rec *logtest.Recorder) sealFixture {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	env := collect.NewEnv(nil, st, ledger, collect.WithLogger(rec.Logger()))

	return sealFixture{
		collector: workspace.New(env, "org"),
		store:     st,
		ledger:    ledger,
	}
}

// writeDone commits a loose file at relPath and records it done in the ledger, so
// the seal phase sees a settled artifact.
func (f sealFixture) writeDone(t *testing.T, relPath string, data []byte) {
	t.Helper()

	_, err := f.store.WriteBytes(relPath, data)
	require.NoError(t, err)

	f.ledger.RecordDone(relPath, manifest.SignatureOf(data))
}

func (f sealFixture) exists(relPath string) bool {
	_, err := os.Stat(f.store.AbsPath(relPath))

	return err == nil
}

func (f sealFixture) markComplete(project, ws string) {
	f.ledger.Collection(f.store.Join(f.store.WorkspaceDir(project, ws), "runs")).MarkComplete()
	f.ledger.Collection(f.store.StateVersionDir(project, ws)).MarkComplete()
}

// rollupContent reads back the byte-for-byte content of the oldest line a
// roll-up recorded for an archive-relative path.
func rollupContent(t *testing.T, st *store.Store, project, ws, rollup, relPath string) []byte {
	t.Helper()

	lines := rollupLines(t, st, project, ws, rollup, relPath)
	require.NotEmpty(t, lines, "path %q not found in roll-up %s", relPath, rollup)

	return []byte(lines[0])
}

func TestSealWorkspace_BundlesFrozenArtifacts(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	// A frozen run: heavy children, immutable small children, and run.json.
	f.writeDone(t, st.RunFile(project, ws, "run-1", "run.json"), []byte(`{"id":"run-1"}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.json"), []byte(`{"plan":true}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "apply.log"), []byte("apply output"))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "policy-check-pc1.log"), []byte("policy log"))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "config-version.json"), []byte(`{"cv":"cv-1"}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "config-version-ingress.json"), []byte(`{"sha":"abc"}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan-summary.json"), []byte(`{"plan":"plan-1"}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "apply-summary.json"), []byte(`{"apply":"apply-1"}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "tf-policy-evaluations.json"), []byte(`{"eval":"eval-1"}`))
	f.writeDone(t, st.RunFile(project, ws, "run-1", "run-events.json"), []byte("[\n  {}\n]"))

	// A state version: raw and JSON blobs plus the meta sidecar.
	sv := st.StateVersionDir(project, ws)
	f.writeDone(t, st.Join(sv, "20260101T000000Z-sv-1.tfstate.json"), []byte(`{"serial":1}`))
	f.writeDone(t, st.Join(sv, "20260101T000000Z-sv-1.json"), []byte(`{"format":"json"}`))
	f.writeDone(t, st.Join(sv, "20260101T000000Z-sv-1.meta.json"), []byte(`{"serial":1}`))

	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// The heavy artifacts are bundled and the immutable small metadata is
	// coalesced; both remove their loose originals.
	for _, gone := range []string{
		st.RunFile(project, ws, "run-1", "plan.log"),
		st.RunFile(project, ws, "run-1", "plan.json"),
		st.RunFile(project, ws, "run-1", "apply.log"),
		st.RunFile(project, ws, "run-1", "policy-check-pc1.log"),
		st.RunFile(project, ws, "run-1", "config-version.json"),
		st.RunFile(project, ws, "run-1", "config-version-ingress.json"),
		st.RunFile(project, ws, "run-1", "plan-summary.json"),
		st.RunFile(project, ws, "run-1", "apply-summary.json"),
		st.RunFile(project, ws, "run-1", "tf-policy-evaluations.json"),
		st.RunFile(project, ws, "run-1", "run-events.json"),
		st.Join(sv, "20260101T000000Z-sv-1.tfstate.json"),
		st.Join(sv, "20260101T000000Z-sv-1.json"),
		st.Join(sv, "20260101T000000Z-sv-1.meta.json"),
	} {
		assert.False(t, f.exists(gone), "%s should be sealed or coalesced away", gone)
	}

	// Only run.json, the mutable summary, stays loose.
	assert.True(t, f.exists(st.RunFile(project, ws, "run-1", "run.json")), "run.json stays loose")

	// The bundles, their sidecars, and the roll-ups exist.
	for _, out := range []string{
		st.Join(st.BundleDir(project, ws), "logs.gen0001.zip"),
		st.Join(st.BundleDir(project, ws), "logs.gen0001.zip.sidecar.ndjson"),
		st.Join(st.BundleDir(project, ws), "state.gen0001.zip"),
		st.Join(st.BundleDir(project, ws), "state.gen0001.zip.sidecar.ndjson"),
		st.Join(st.RollupDir(project, ws), "config-versions.ndjson"),
		st.Join(st.RollupDir(project, ws), "plan-summaries.ndjson"),
		st.Join(st.RollupDir(project, ws), "apply-summaries.ndjson"),
		st.Join(st.RollupDir(project, ws), "tf-policy-evaluations.ndjson"),
		st.Join(st.RollupDir(project, ws), "run-events.ndjson"),
		st.Join(st.RollupDir(project, ws), "state-versions.ndjson"),
	} {
		assert.True(t, f.exists(out), "%s should exist", out)
	}

	// A coalesced line carries the original bytes verbatim: a multi-line indented
	// JSON round-trips through the roll-up's escaped content field.
	original := []byte("[\n  {}\n]")
	assert.Equal(t, original, rollupContent(t, f.store, project, ws, "run-events.ndjson",
		st.RunFile(project, ws, "run-1", "run-events.json")))
}

func TestSealWorkspace_FlushesLedgerBeforeRemovingSources(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	planLog := st.RunFile(project, ws, "run-1", "plan.log")
	f.writeDone(t, planLog, []byte("plan output"))
	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
	require.False(t, f.exists(planLog), "the loose source is sealed away")

	// Close releases the lock without flushing, so the reload sees exactly
	// what a process hard-killed right after the seal would leave behind.
	require.NoError(t, f.ledger.Close())

	reloaded, err := manifest.Load(st.Root())
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, reloaded.Close()) })

	// The seal destroyed the loose plan.log, so the records that authorized
	// it must already be durable: a reload missing them would re-fetch the
	// sealed path and could settle an expired artifact absent while the
	// bundle holds its bytes.
	entry, ok := reloaded.Entry(planLog)
	require.True(t, ok, "the sealed artifact's done entry is durable before its loose source is removed")
	assert.Equal(t, manifest.StatusDone, entry.Status)

	runsKey := st.Join(st.WorkspaceDir(project, ws), "runs")
	assert.True(t, reloaded.Collection(runsKey).Complete(),
		"the completion flag that authorized the seal is durable")
}

func TestSealWorkspace_SkipsIncompleteCollections(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))

	// The runs collection is never marked complete, so a still-walking archive is
	// not sealed prematurely.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(st.RunFile(project, ws, "run-1", "plan.log")),
		"an incomplete collection is left loose")
	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"no bundle is written for an incomplete collection")
}

func TestSealWorkspace_SkipsUnsettledArtifacts(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	// A heavy file present on disk but recorded errored, not done, is left loose:
	// only settled artifacts seal.
	relPath := st.RunFile(project, ws, "run-1", "plan.log")
	_, err := st.WriteBytes(relPath, []byte("partial"))
	require.NoError(t, err)
	f.ledger.RecordErrored(relPath, errors.New("boom"), false)

	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(relPath), "an unsettled artifact is not sealed")
	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"no bundle is written when nothing is frozen")
}

func TestSealWorkspace_IgnoresReferenceGates(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "stg", "web"

	// A frozen run whose actor write stranded: run-events.json is on disk and done,
	// while a ledger-only pending reference gate has no on-disk file. Sealing must
	// coalesce the events file and neither seal, materialize, nor disturb the gate.
	gate := st.RunFile(project, ws, "run-1", "run-events-actors.ref")
	f.writeDone(t, st.RunFile(project, ws, "run-1", "run-events.json"), []byte("[]"))
	f.ledger.MirrorReference(gate, false)

	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(st.RunFile(project, ws, "run-1", "run-events.json")),
		"the events file coalesces into its roll-up")
	assert.True(t, f.exists(st.Join(st.RollupDir(project, ws), "run-events.ndjson")),
		"the run-events roll-up is written")
	assert.False(t, f.exists(gate), "a reference gate is never materialized on disk")

	// The gate entry is inert to sealing: it stays pending, so a later run still
	// retries the stranded actor.
	entry, ok := f.ledger.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusPending, entry.Status)
}

func TestSealWorkspace_GenerationsAppend(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("first"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// A later run freezes a new artifact; sealing again writes the next generation
	// and leaves the first untouched.
	f.writeDone(t, st.RunFile(project, ws, "run-2", "plan.log"), []byte("second"))
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"the first generation is never rewritten")
	assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0002.zip")),
		"a new generation holds the newly frozen artifact")
	assert.False(t, f.exists(st.RunFile(project, ws, "run-2", "plan.log")),
		"the newly frozen artifact is sealed")
}

func TestSealWorkspace_ResealAfterStrandedSourcesIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	planLog := st.RunFile(project, ws, "run-1", "plan.log")
	stateBlob := st.Join(st.StateVersionDir(project, ws), "20260101T000000Z-sv-1.tfstate.json")

	f.writeDone(t, planLog, []byte("plan output"))
	f.writeDone(t, stateBlob, []byte(`{"serial":1}`))
	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
	require.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")))
	require.True(t, f.exists(st.Join(st.BundleDir(project, ws), "state.gen0001.zip")))
	require.False(t, f.exists(planLog), "the first seal removed the loose source")

	// Model a crash between the sidecar commit and the source removal: the bundle
	// and its sidecar are durable, yet the identical loose sources are still on
	// disk and still recorded done.
	f.writeDone(t, planLog, []byte("plan output"))
	f.writeDone(t, stateBlob, []byte(`{"serial":1}`))

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0002.zip")),
		"a stranded source already in a verified bundle is not re-sealed into a new generation")
	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "state.gen0002.zip")),
		"the same holds for the state bundle")
	assert.False(t, f.exists(planLog), "the reconciled stranded source is removed")
	assert.False(t, f.exists(stateBlob), "the reconciled stranded state blob is removed")
}

func TestSealWorkspace_StrandedSourceWarningCarriesRemovalCause(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test refuses the removal with")
	}

	rec := logtest.NewRecorder()
	f := newSealFixtureLogged(t, rec)
	st := f.store
	project, ws := "prod", "api"

	planLog := st.RunFile(project, ws, "run-1", "plan.log")

	f.writeDone(t, planLog, []byte("plan output"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// Model the strand the reconcile exists for, with the removal that clears it
	// refused: the bundle and sidecar are durable, the identical loose source is
	// back on disk, and its parent directory allows the digest's read but not the
	// unlink.
	f.writeDone(t, planLog, []byte("plan output"))

	runDir := st.AbsPath(st.RunDir(project, ws, "run-1"))
	require.NoError(t, os.Chmod(runDir, 0o500))

	t.Cleanup(func() {
		//nolint:errcheck // Restores the mode so the temp dir can be torn down.
		_ = os.Chmod(runDir, 0o700)
	})

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	events := rec.Events("seal_reconcile_stranded_source")
	require.Len(t, events, 1, "the stranded source is reported once")
	assert.Equal(t, planLog, events[0].Attrs["name"])
	assert.Contains(t, events[0].Attrs["remove_error"], "permission denied",
		"a refused removal names its cause, so a warning that recurs every run is diagnosable")
	assert.True(t, f.exists(planLog), "the refused removal leaves the source for the next run")
}

func TestSealWorkspace_RefusedSourceRemovalIsReported(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test refuses the removal with")
	}

	rec := logtest.NewRecorder()
	f := newSealFixtureLogged(t, rec)
	st := f.store
	project, ws := "prod", "api"

	planLog := st.RunFile(project, ws, "run-1", "plan.log")
	planSummary := st.RunFile(project, ws, "run-1", "plan-summary.json")

	f.writeDone(t, planLog, []byte("plan output"))
	f.writeDone(t, planSummary, []byte(`{"plan":"plan-1"}`))
	f.markComplete(project, ws)

	// The run directory allows the seal's reads but not the unlink, so both the
	// bundle's and the roll-up's best-effort source removals are refused.
	runDir := st.AbsPath(st.RunDir(project, ws, "run-1"))
	require.NoError(t, os.Chmod(runDir, 0o500))

	t.Cleanup(func() {
		//nolint:errcheck // Restores the mode so the temp dir can be torn down.
		_ = os.Chmod(runDir, 0o700)
	})

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// Each refusal is reported the moment it happens, named under the sealed
	// form that met it, rather than a run later by the reconcile.
	events := rec.Events("seal_remove_source_error")
	require.Len(t, events, 2, "one report per refused removal")

	assert.Equal(t, "logs", events[0].Attrs["prefix"])
	assert.Equal(t, planLog, events[0].Attrs["name"])
	assert.Contains(t, events[0].Attrs["error"], "permission denied")

	assert.Equal(t, "plan-summaries.ndjson", events[1].Attrs["rollup"])
	assert.Equal(t, planSummary, events[1].Attrs["name"])
	assert.Contains(t, events[1].Attrs["error"], "permission denied")

	assert.True(t, f.exists(planLog), "the refused removal leaves the source for the next run")
}

func TestSealWorkspace_ResealWithChangedContentSealsNewGeneration(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	planLog := st.RunFile(project, ws, "run-1", "plan.log")

	f.writeDone(t, planLog, []byte("first output"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
	require.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")))

	// A stranded source whose bytes diverge from what the sidecar sealed must
	// never be dropped as already-sealed: seal it as a new generation so the
	// changed content survives.
	f.writeDone(t, planLog, []byte("second output, different bytes"))
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0002.zip")),
		"divergent content is sealed into a new generation, not dropped")
	assert.False(t, f.exists(planLog), "the newly sealed source is removed")
}

// runJSON renders a minimal archived run document recording status, the shape
// terminalRunFile reads.
func runJSON(status string) []byte {
	return []byte(`{"data":{"attributes":{"status":"` + status + `"}}}`)
}

// rollupLines returns every line a roll-up recorded for an archive-relative
// path, oldest first.
func rollupLines(t *testing.T, st *store.Store, project, ws, rollup, relPath string) []string {
	t.Helper()

	data, err := os.ReadFile(st.AbsPath(st.Join(st.RollupDir(project, ws), rollup)))
	require.NoError(t, err)

	var out []string

	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var entry struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}

		require.NoError(t, json.Unmarshal(line, &entry))

		if entry.Path == relPath {
			out = append(out, entry.Content)
		}
	}

	return out
}

func TestSealWorkspace_CoalescesTerminalRunJSON(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	relPath := st.RunFile(project, ws, "run-1", "run.json")
	f.writeDone(t, relPath, runJSON("applied"))
	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(relPath), "a settled terminal run.json coalesces away")
	assert.Equal(t, runJSON("applied"),
		rollupContent(t, f.store, project, ws, "runs.ndjson", relPath),
		"the roll-up carries the summary byte for byte")

	// The run directory held only run.json, so the seal empties it; both it and
	// the now-empty runs/ parent are pruned.
	assert.False(t, f.exists(st.RunDir(project, ws, "run-1")), "the emptied run dir is pruned")
	assert.False(t, f.exists(st.Join(st.WorkspaceDir(project, ws), "runs")),
		"the emptied runs dir is pruned")
}

func TestSealWorkspace_LeavesRunHistorySidecarLoose(t *testing.T) {
	t.Parallel()

	// A run observed in-flight across two archive runs grows a history
	// sidecar beside its run.json. The seal coalesces the terminal summary
	// but must leave the sidecar loose: it is the status timeline nothing
	// else keeps, so it is never bundled, never rolled up, and never
	// removed, and the run dir it keeps non-empty survives as accepted
	// residue.
	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	relPath := st.RunFile(project, ws, "run-1", "run.json")
	f.writeDone(t, relPath, runJSON("applied"))

	sidecar := st.HistoryPath(relPath)
	sidecarLine := `{"fetchedAt":"2026-08-11T09:00:00Z","sha256":"ab","content":"{}"}` + "\n"

	_, err := st.WriteBytes(sidecar, []byte(sidecarLine))
	require.NoError(t, err)

	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(relPath), "the terminal summary coalesces away")
	assert.Equal(t, runJSON("applied"),
		rollupContent(t, f.store, project, ws, "runs.ndjson", relPath))

	assert.True(t, f.exists(sidecar), "the run's history sidecar stays loose")
	assert.Empty(t, rollupLines(t, f.store, project, ws, "runs.ndjson", sidecar),
		"the sidecar is never rolled up")
	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"the sidecar is never bundled")
	assert.True(t, f.exists(st.RunDir(project, ws, "run-1")),
		"a run dir still holding its sidecar is not pruned")
}

func TestSealWorkspace_RunJSONStaysLoose(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		content  []byte
		unsettle bool
	}{
		"an in-flight run": {
			content: runJSON("planning"),
		},
		"an unrecognized status": {
			content: runJSON("some_future_state"),
		},
		"an unparseable document": {
			content: []byte("not json"),
		},
		"an unsettled summary": {
			content:  runJSON("applied"),
			unsettle: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSealFixture(t)
			st := f.store
			project, ws := "prod", "api"

			relPath := st.RunFile(project, ws, "run-1", "run.json")

			if tc.unsettle {
				_, err := st.WriteBytes(relPath, tc.content)
				require.NoError(t, err)
				f.ledger.RecordErrored(relPath, errors.New("boom"), false)
			} else {
				f.writeDone(t, relPath, tc.content)
			}

			f.markComplete(project, ws)

			require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

			assert.True(t, f.exists(relPath), "the summary stays loose")
			assert.False(t, f.exists(st.Join(st.RollupDir(project, ws), "runs.ndjson")),
				"nothing coalesces into the runs roll-up")
			assert.True(t, f.exists(st.RunDir(project, ws, "run-1")),
				"a run dir still holding its summary is not pruned")
		})
	}
}

func TestSealWorkspace_RunJSONIncompleteCollectionStaysLoose(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	relPath := st.RunFile(project, ws, "run-1", "run.json")
	f.writeDone(t, relPath, runJSON("applied"))

	// The runs collection was never walked to its end, so even a terminal
	// summary is not frozen.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(relPath))
	assert.False(t, f.exists(st.Join(st.RollupDir(project, ws), "runs.ndjson")))
}

func TestSealWorkspace_ReconcilesRematerializedRollupSource(t *testing.T) {
	t.Parallel()

	rec := logtest.NewRecorder()
	f := newSealFixtureLogged(t, rec)
	st := f.store
	project, ws := "prod", "api"

	runPath := st.RunFile(project, ws, "run-1", "run.json")
	summaryPath := st.RunFile(project, ws, "run-1", "plan-summary.json")

	f.writeDone(t, runPath, runJSON("applied"))
	f.writeDone(t, summaryPath, []byte(`{"plan":"plan-1"}`))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// The identical bytes come back settled (a reader re-materialized them from
	// a stale mirror key, or a refused removal left them): the next seal must
	// reconcile them away, not append a duplicate line on every pass forever.
	f.writeDone(t, runPath, runJSON("applied"))
	f.writeDone(t, summaryPath, []byte(`{"plan":"plan-1"}`))
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.Len(t, rollupLines(t, f.store, project, ws, "runs.ndjson", runPath), 1,
		"an already-folded run.json gains no duplicate line")
	assert.Len(t, rollupLines(t, f.store, project, ws, "plan-summaries.ndjson", summaryPath), 1,
		"an already-folded immutable child gains no duplicate line")
	assert.False(t, f.exists(runPath), "the reconciled stranded source is removed")
	assert.False(t, f.exists(summaryPath))

	// Each stranded source is reported under the roll-up that already records
	// it, mirroring the bundle reconcile's warning.
	events := rec.Events("seal_reconcile_stranded_source")
	require.Len(t, events, 2)
	assert.Equal(t, "plan-summaries.ndjson", events[0].Attrs["rollup"])
	assert.Equal(t, summaryPath, events[0].Attrs["name"])
	assert.Equal(t, "runs.ndjson", events[1].Attrs["rollup"])
	assert.Equal(t, runPath, events[1].Attrs["name"])
}

func TestSealWorkspace_RunJSONResealAppendsNewerLine(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	relPath := st.RunFile(project, ws, "run-1", "run.json")
	f.writeDone(t, relPath, runJSON("canceled"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// The run's summary legitimately changed after the first seal (a canceled
	// run force-canceled, say): the refreshed loose copy re-freezes and the
	// roll-up gains a newer, different line under the same path.
	f.writeDone(t, relPath, runJSON("force_canceled"))
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	lines := rollupLines(t, f.store, project, ws, "runs.ndjson", relPath)
	require.Len(t, lines, 2, "each seal appends its own line")
	assert.Equal(t, string(runJSON("canceled")), lines[0])
	assert.Equal(t, string(runJSON("force_canceled")), lines[1],
		"the newest line carries the updated content")
	assert.False(t, f.exists(relPath))
}

// writeOrphanZip crafts a valid zip holding members (name to content) at an
// archive-relative path with no sidecar beside it, modeling the residue of a
// seal a crash cut short.
func writeOrphanZip(t *testing.T, st *store.Store, relPath string, members map[string]string) {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for name, content := range members {
		w, err := zw.Create(name)
		require.NoError(t, err)

		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())

	_, err := st.WriteBytes(relPath, buf.Bytes())
	require.NoError(t, err)
}

func TestSealWorkspace_SweepsOrphanCoveredBySidecars(t *testing.T) {
	t.Parallel()

	rec := logtest.NewRecorder()
	f := newSealFixtureLogged(t, rec)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// A crash orphan whose loose sources later re-sealed: its every member is
	// recorded, digest for digest, by the surviving gen0001 sidecar, so its
	// bytes are held twice and the zip is safe to reclaim. Model it by copying
	// the sealed zip to a sidecar-less generation.
	sealedZip := st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")
	orphanZip := st.Join(st.BundleDir(project, ws), "logs.gen0002.zip")

	data, err := os.ReadFile(st.AbsPath(sealedZip))
	require.NoError(t, err)

	_, err = st.WriteBytes(orphanZip, data)
	require.NoError(t, err)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(orphanZip), "an orphan proven redundant is reclaimed")
	assert.True(t, f.exists(sealedZip), "the sidecar-backed generation is untouched")
	assert.True(t, f.exists(sealedZip+seal.SidecarSuffix))

	events := rec.Events("seal_orphan_bundle_swept")
	require.Len(t, events, 1)
	assert.Equal(t, orphanZip, events[0].Attrs["path"])
}

func TestSealWorkspace_KeepsOrphanItCannotProve(t *testing.T) {
	t.Parallel()

	planLog := "projects/prod/workspaces/api/runs/run-1/plan.log"

	tests := map[string]struct {
		sealFirst bool              // seal a settled plan.log before planting the orphan
		members   map[string]string // the orphan zip's contents
		reason    string
	}{
		"a member no surviving sidecar records": {
			sealFirst: false,
			members:   map[string]string{planLog: "plan output"},
			reason:    "recorded by no surviving sidecar",
		},
		"a member diverging from the recorded digest": {
			sealFirst: true,
			members:   map[string]string{planLog: "different bytes"},
			reason:    "diverges from the recorded digest",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := logtest.NewRecorder()
			f := newSealFixtureLogged(t, rec)
			st := f.store
			project, ws := "prod", "api"

			if tc.sealFirst {
				f.writeDone(t, planLog, []byte("plan output"))
				f.markComplete(project, ws)
				require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
			}

			// A sidecar-less zip whose bytes are not proven to live in any
			// sealed generation may be the archive's only copy (a verified
			// bundle whose sidecar was lost): it must survive the sweep.
			orphanZip := st.Join(st.BundleDir(project, ws), "logs.gen0009.zip")
			writeOrphanZip(t, st, orphanZip, tc.members)

			require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

			assert.True(t, f.exists(orphanZip), "an unproven orphan is never removed")

			events := rec.Events("seal_orphan_bundle_kept")
			require.Len(t, events, 1, "the kept orphan is reported")
			assert.Equal(t, orphanZip, events[0].Attrs["path"])
			assert.Contains(t, events[0].Attrs["reason"], tc.reason)
		})
	}
}

// writeSidecar commits the sidecar index of one sealed generation of prefix,
// recording name at digest. It is the marker sealedDigests reads, so writing it
// alone models a generation whose zip has since been evicted to a remote store.
func (f sealFixture) writeSidecar(t *testing.T, project, ws, prefix string, gen int, name, digest string) {
	t.Helper()

	bundle := fmt.Sprintf("%s.gen%04d.zip", prefix, gen)

	line, err := json.Marshal(seal.Entry{Name: name, Bundle: bundle, SHA256: digest})
	require.NoError(t, err)

	_, err = f.store.WriteBytes(
		f.store.Join(f.store.BundleDir(project, ws), bundle+seal.SidecarSuffix),
		append(line, '\n'),
	)
	require.NoError(t, err)
}

// digestOf returns the lowercase hex SHA-256 of data, the form a sidecar entry
// records.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// genDigest is a stand-in digest that identifies the generation that recorded
// it, for tests that care only which sidecar won a name.
func genDigest(gen int) string {
	return fmt.Sprintf("digest-of-gen%d", gen)
}

func TestSealedDigestsReadsInGenerationOrder(t *testing.T) {
	t.Parallel()

	// The generation field is zero-padded to four digits and widens past them, so
	// a lexical read order would put gen10000 before gen9999 and hand a recurring
	// name the older generation's digest.
	tests := map[string]struct {
		gens []int
		want int
	}{
		"one generation":                       {gens: []int{1}, want: 1},
		"ascending generations":                {gens: []int{1, 2, 3}, want: 3},
		"widths that sort the same either way": {gens: []int{998, 999}, want: 999},
		"across the padding boundary":          {gens: []int{9999, 10000}, want: 10000},
		"past the padding boundary":            {gens: []int{9999, 10000, 10001}, want: 10001},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSealFixture(t)
			st := f.store
			project, ws := "prod", "api"
			member := st.RunFile(project, ws, "run-1", "plan.log")

			for _, gen := range tc.gens {
				f.writeSidecar(t, project, ws, "logs", gen, member, genDigest(gen))
			}

			sealed, err := f.collector.SealedDigests(st.AbsPath(st.BundleDir(project, ws)), "logs")
			require.NoError(t, err)

			assert.Equal(t, genDigest(tc.want), sealed[member],
				"the highest generation's entry wins for a recurring name")
		})
	}
}

func TestSealWorkspace_ReconcilesPastThePaddingBoundary(t *testing.T) {
	t.Parallel()

	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	planLog := st.RunFile(project, ws, "run-1", "plan.log")
	superseded := []byte("first output")
	current := []byte("second output, different bytes")

	// Two generations either side of the padding boundary sealed the same name,
	// the newer holding the bytes a failed removal stranded on disk. Read in
	// generation order the survivor is proven sealed and dropped; read lexically
	// gen9999's stale digest wins, the survivor looks divergent, and its name is
	// duplicated into a third bundle.
	f.writeSidecar(t, project, ws, "logs", 9999, planLog, digestOf(superseded))
	f.writeSidecar(t, project, ws, "logs", 10000, planLog, digestOf(current))

	f.writeDone(t, planLog, current)
	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen10001.zip")),
		"a survivor the newest generation already sealed is not sealed again")
	assert.False(t, f.exists(planLog), "the reconciled stranded source is removed")
}
