package workspace_test

import (
	"net/http"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveRunEventsArchivesActors(t *testing.T) {
	t.Parallel()

	events := []*tfe.RunEvent{
		{ID: "re-1", Action: "created", Actor: &tfe.User{ID: "user-1", Username: "alice"}},
		{ID: "re-2", Action: "applied", Actor: &tfe.User{ID: "user-2", Username: "bob"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1/run-events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, events))
	})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveRunEvents(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	// The event timeline is archived, and each hydrated actor becomes its own
	// users/<id>.json rather than a permanently-opaque id ref.
	assert.Equal(t, []string{"re-1", "re-2"},
		f.dataIDs(t, st.RunFile("proj", "ws", "run-1", "run-events.json")))

	alice := f.attrs(t, st.User("user-1"), "users", "user-1")
	assert.Equal(t, "alice", alice["username"])

	bob := f.attrs(t, st.User("user-2"), "users", "user-2")
	assert.Equal(t, "bob", bob["username"])
}
