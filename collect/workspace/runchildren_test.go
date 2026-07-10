package workspace_test

import (
	"net/http"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

func TestArchiveConfigurationVersionWithIngress(t *testing.T) {
	t.Parallel()

	cv := &tfe.ConfigurationVersion{
		ID:     "cv-1",
		Status: tfe.ConfigurationUploaded,
		IngressAttributes: &tfe.IngressAttributes{
			ID:        "ia-1",
			Branch:    "main",
			CommitSHA: "abc123",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	// The tarball is deduped org-wide and downloaded separately; settle it so the
	// test isolates on the record read and its split into two files.
	f.preSettle(st.ConfigVersionTarball("cv-1"))

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	f.attrs(t, st.RunFile("proj", "ws", "run-1", "config-version.json"), "configuration-versions", "cv-1")

	ingPath := st.RunFile("proj", "ws", "run-1", "config-version-ingress.json")
	ing := f.attrs(t, ingPath, "ingress-attributes", "ia-1")
	assert.Equal(t, "abc123", ing["commit-sha"])
	assert.Equal(t, "main", ing["branch"])
}

func TestArchiveConfigurationVersionVCSLess(t *testing.T) {
	t.Parallel()

	// A VCS-less upload has no ingress attributes: the record is archived and the
	// ingress path settles a not-applicable gap rather than a null file.
	cv := &tfe.ConfigurationVersion{ID: "cv-2", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.ConfigVersionTarball("cv-2"))

	run := &tfe.Run{ID: "run-2", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-2"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	f.attrs(t, st.RunFile("proj", "ws", "run-2", "config-version.json"), "configuration-versions", "cv-2")

	ingPath := st.RunFile("proj", "ws", "run-2", "config-version-ingress.json")
	assert.Equal(t, manifest.StatusNotApplicable, f.status(ingPath))

	exists, err := st.Exists(ingPath)
	require.NoError(t, err)
	assert.False(t, exists, "a VCS-less config version writes no ingress file")
}

func TestArchiveConfigurationVersionRetryGate(t *testing.T) {
	t.Parallel()

	cv := &tfe.ConfigurationVersion{
		ID:                "cv-3",
		IngressAttributes: &tfe.IngressAttributes{ID: "ia-3", CommitSHA: "def456"},
	}

	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-3", func(w http.ResponseWriter, _ *http.Request) {
		hits++

		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	cvPath := st.RunFile("proj", "ws", "run-3", "config-version.json")
	ingPath := st.RunFile("proj", "ws", "run-3", "config-version-ingress.json")

	// The record settled on an earlier pass but its ingress write did not: the
	// read must still happen because the gate opens on either derived file, and
	// the already-settled record must not regress.
	f.preSettle(cvPath)
	f.preSettle(st.ConfigVersionTarball("cv-3"))

	run := &tfe.Run{ID: "run-3", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-3"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, 1, hits, "the record is re-read while the ingress gap remains")
	f.attrs(t, ingPath, "ingress-attributes", "ia-3")
	assert.Equal(t, manifest.StatusDone, f.status(cvPath), "the settled record is left untouched")
}
