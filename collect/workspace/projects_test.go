package workspace_test

import (
	"net/http"
	"testing"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectProjectArchivesEffectiveTagBindings(t *testing.T) {
	t.Parallel()

	hydrated := &tfe.Project{
		ID:   "prj-1",
		Name: "platform",
		EffectiveTagBindings: []*tfe.EffectiveTagBinding{
			{ID: "etb-1", Key: "env", Value: "prod"},
			{ID: "etb-2", Key: "team", Value: "platform"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/projects/prj-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, hydrated))
	})
	// The project's notification configs and team access are listed separately;
	// answer them empty so the collect completes.
	mux.HandleFunc("/api/v2/projects/prj-1/notification-configurations",
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSONAPI(t, w, listPayload(0))
		})
	mux.HandleFunc("/api/v2/team-projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, listPayload(0))
	})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.CollectProject(t.Context(), &tfe.Project{ID: "prj-1", Name: "platform"}))

	// The project record keeps its own attributes.
	attrs := f.attrs(t, st.ProjectFile("platform", "project.json"), "projects", "prj-1")
	assert.Equal(t, "platform", attrs["name"])

	// The hydrated effective tag bindings are archived as their own file rather
	// than dropped as bare id refs on project.json.
	tagsPath := st.ProjectFile("platform", "effective-tag-bindings.json")
	assert.Equal(t, []string{"etb-1", "etb-2"}, f.dataIDs(t, tagsPath))
}
