package serialize_test

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/serialize"
)

// TestMarshalIsByteFaithful pins the package's fidelity contract: values the
// API returned (sensitive variable values, token fields, signed URLs) are
// stored exactly as received, never redacted or stripped. The archive is a
// full-fidelity record, and its at-rest sensitivity is handled by file
// permissions, not by rewriting payloads.
func TestMarshalIsByteFaithful(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input        any
		wantContains []string
	}{
		"sensitive variable keeps its value and kebab keys": {
			input: &tfe.Variable{
				ID:        "var-1",
				Key:       "db_password",
				Value:     "hunter2",
				VersionID: "vv-1",
				Sensitive: true,
			},
			wantContains: []string{"hunter2", `"version-id"`, `"vars"`},
		},
		"agent token keeps its secret": {
			input: &tfe.AgentToken{
				ID:    "at-1",
				Token: "aabbcc.atlasv1.deadbeef",
			},
			wantContains: []string{"aabbcc.atlasv1.deadbeef"},
		},
		"oauth client keeps its secret": {
			input: &tfe.OAuthClient{
				ID:     "oc-1",
				Secret: "shhh",
			},
			wantContains: []string{`"shhh"`},
		},
		"run task keeps its hmac key": {
			input: &tfe.RunTask{
				ID:      "task-1",
				Name:    "sentinel",
				HMACKey: new("raw-hmac"),
			},
			wantContains: []string{"raw-hmac", `"hmac-key"`},
		},
		"notification configuration keeps its token": {
			input: &tfe.NotificationConfiguration{
				ID:    "nc-1",
				Token: "notif-token",
			},
			wantContains: []string{"notif-token"},
		},
		"state version keeps its signed urls": {
			input: &tfe.StateVersion{
				ID:          "sv-1",
				DownloadURL: "https://archivist.example/download?token=sig",
				UploadURL:   "https://archivist.example/upload?token=sig",
			},
			wantContains: []string{"https://archivist.example/download?token=sig"},
		},
		"signed url keeps ampersands and angle brackets verbatim": {
			// The jsonapi path once ran through an HTML-escaping encoder, which
			// rewrote &, <, and > in a multi-parameter signed URL into \u escapes a
			// raw search would then miss. Pin that they survive byte for byte.
			input: &tfe.StateVersion{
				ID:          "sv-2",
				DownloadURL: "https://archivist.example/d?token=a&sig=b&x=<y>",
			},
			wantContains: []string{"?token=a&sig=b&x=<y>"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := serialize.Marshal(tc.input)
			require.NoError(t, err)

			out := string(got)

			for _, want := range tc.wantContains {
				assert.Contains(t, out, want)
			}

			assert.NotContains(t, out, "REDACTED", "nothing is rewritten on the way to disk")
			require.True(t, json.Valid(got), "output must be valid JSON")
		})
	}
}

// TestMarshalSubStructKeepsAttributes locks the behavior the hydrated-relation
// archiving depends on: a sub-object that carries its own jsonapi primary tag,
// marshaled directly as the primary object, renders its full attributes rather
// than the bare {type, id} reference it collapses to when nested on a parent.
func TestMarshalSubStructKeepsAttributes(t *testing.T) {
	t.Parallel()

	version := &tfe.PolicySetVersion{
		ID:     "psv-1",
		Source: "tfe-api",
		Status: "ready",
	}

	got, err := serialize.Marshal(version)
	require.NoError(t, err)

	var doc struct {
		Data struct {
			Type       string         `json:"type"`
			ID         string         `json:"id"`
			Attributes map[string]any `json:"attributes"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal(got, &doc))
	assert.Equal(t, "policy-set-versions", doc.Data.Type)
	assert.Equal(t, "psv-1", doc.Data.ID)
	assert.Equal(t, "tfe-api", doc.Data.Attributes["source"], "the attributes survive, not a bare id ref")
	assert.Equal(t, "ready", doc.Data.Attributes["status"])
}

func TestMarshalHydratedRelationAsIDRef(t *testing.T) {
	t.Parallel()

	v := &tfe.Variable{
		ID:        "var-1",
		Key:       "region",
		Value:     "us-east-1",
		Workspace: &tfe.Workspace{ID: "ws-99", Name: "production-workspace"},
	}

	got, err := serialize.Marshal(v)
	require.NoError(t, err)

	var payload struct {
		Data struct {
			Relationships map[string]struct {
				Data struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"data"`
			} `json:"relationships"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal(got, &payload))

	rel, ok := payload.Data.Relationships["configurable"]
	require.True(t, ok, "expected a configurable relationship")
	assert.Equal(t, "ws-99", rel.Data.ID)
	assert.Equal(t, "workspaces", rel.Data.Type)

	// The relation is an id ref, not a nested object, so the workspace name and
	// the sideloaded "included" array must be absent.
	assert.NotContains(t, string(got), "production-workspace")
	assert.NotContains(t, string(got), `"included"`)
}

func TestMarshalPlainTypeUsesEncodingJSON(t *testing.T) {
	t.Parallel()

	at := &tfe.AuditTrail{
		ID:      "at-1",
		Version: "1",
		Type:    "Resource",
	}

	got, err := serialize.Marshal(at)
	require.NoError(t, err)

	out := string(got)

	// Plain types honor the json tags and produce no jsonapi "data" wrapper.
	assert.Contains(t, out, `"id": "at-1"`)
	assert.Contains(t, out, `"version": "1"`)
	assert.NotContains(t, out, `"data"`)

	// Indented output carries newlines and two-space indentation.
	assert.Contains(t, out, "\n  ")
}

func TestMarshalByValueModel(t *testing.T) {
	t.Parallel()

	// A model passed by value still marshals through the jsonapi encoder, which
	// requires a pointer; Marshal takes an addressable copy.
	got, err := serialize.Marshal(tfe.Variable{ID: "var-1", Key: "a", Value: "one"})
	require.NoError(t, err)

	assert.Contains(t, string(got), `"one"`)
	require.True(t, json.Valid(got))
}

func TestMarshalSliceOfModels(t *testing.T) {
	t.Parallel()

	vars := []*tfe.Variable{
		{ID: "var-1", Key: "a", Value: "one", Sensitive: true},
		{ID: "var-2", Key: "b", Value: "two", Sensitive: false},
	}

	got, err := serialize.Marshal(vars)
	require.NoError(t, err)

	out := string(got)

	assert.Contains(t, out, `"one"`, "a sensitive value is stored as returned")
	assert.Contains(t, out, "two")
	require.True(t, json.Valid(got))
}
