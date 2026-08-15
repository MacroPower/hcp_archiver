package workspace_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectWorkspaceSkipsOnIdentityMismatch(t *testing.T) {
	t.Parallel()

	// The directory projects/proj/workspaces/app archives a workspace that has
	// since been deleted; a new workspace reused the name. Collecting the new
	// one must not touch the directory: no endpoint is registered on the mux,
	// so any fetch attempt would fail the test through an unexpected request.
	f := newWSFixture(t, http.NewServeMux())
	st := f.store

	dir := filepath.Join(st.Root(), "projects", "proj", "workspaces", "app")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".identity.json"),
		[]byte(`{"id":"ws-deleted","firstSeen":"2026-01-01T00:00:00Z"}`),
		0o600,
	))

	ws := &tfe.Workspace{ID: "ws-reused", Name: "app"}

	err := f.collector.CollectWorkspace(t.Context(), "proj", ws, nil)
	require.NoError(t, err, "the skip is best-effort, not a collector error")

	_, ok := f.ledger.Entry(st.WorkspaceFile("proj", "app", "workspace.json"))
	assert.False(t, ok, "nothing was archived into the contested directory")

	assert.Equal(t, 1, f.ledger.Tally().SurfacesDropped,
		"the skipped workspace marks the run incomplete until the operator intervenes")
}
