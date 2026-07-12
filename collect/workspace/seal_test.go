package workspace_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
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
	f.ledger.MarkCollectionComplete(f.store.Join(f.store.WorkspaceDir(project, ws), "runs"))
	f.ledger.MarkCollectionComplete(f.store.StateVersionDir(project, ws))
}

// rollupContent reads back the byte-for-byte content a roll-up recorded for an
// archive-relative path.
func rollupContent(t *testing.T, st *store.Store, project, ws, rollup, relPath string) []byte {
	t.Helper()

	data, err := os.ReadFile(st.AbsPath(st.Join(st.RollupDir(project, ws), rollup)))
	require.NoError(t, err)

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
			return []byte(entry.Content)
		}
	}

	t.Fatalf("path %q not found in roll-up %s", relPath, rollup)

	return nil
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
