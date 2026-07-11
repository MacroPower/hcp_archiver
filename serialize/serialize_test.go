package serialize_test

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/serialize"
)

// plainSecret is a non-jsonapi struct carrying a sensitive Value, so it takes
// the encoding/json fallback rather than the jsonapi encoder and can exercise
// redaction of a secret reached only through an unaddressable reflected value.
type plainSecret struct {
	Value     string
	Sensitive bool
}

// ifaceHolder nests a value behind an interface field, where the concrete struct
// the interface holds is not addressable.
type ifaceHolder struct {
	Inner any
}

func TestMarshal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input           any
		wantContains    []string
		wantNotContains []string
	}{
		"sensitive variable redacts value and keeps kebab keys": {
			input: &tfe.Variable{
				ID:        "var-1",
				Key:       "db_password",
				Value:     "hunter2",
				VersionID: "vv-1",
				Sensitive: true,
			},
			wantContains:    []string{serialize.Redacted, `"version-id"`, `"vars"`},
			wantNotContains: []string{"hunter2"},
		},
		"non-sensitive variable keeps its value": {
			input: &tfe.Variable{
				ID:        "var-2",
				Key:       "region",
				Value:     "us-east-1",
				Sensitive: false,
			},
			wantContains:    []string{"us-east-1"},
			wantNotContains: []string{serialize.Redacted},
		},
		"sensitive variable-set variable redacts value": {
			input: &tfe.VariableSetVariable{
				ID:        "var-3",
				Key:       "api_key",
				Value:     "abc123",
				Sensitive: true,
			},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{"abc123"},
		},
		"sensitive policy-set parameter redacts value": {
			input: &tfe.PolicySetParameter{
				ID:        "var-4",
				Key:       "token",
				Value:     "topsecret",
				Sensitive: true,
			},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{"topsecret"},
		},
		"agent token secret is redacted": {
			input: &tfe.AgentToken{
				ID:    "at-1",
				Token: "aabbcc.atlasv1.deadbeef",
			},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{"deadbeef"},
		},
		"oauth client secret is redacted": {
			input: &tfe.OAuthClient{
				ID:     "oc-1",
				Secret: "shhh",
			},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{`"shhh"`},
		},
		"run task hmac key is redacted": {
			input: &tfe.RunTask{
				ID:      "task-1",
				Name:    "sentinel",
				HMACKey: new("raw-hmac"),
			},
			wantContains:    []string{serialize.Redacted, `"hmac-key"`},
			wantNotContains: []string{"raw-hmac"},
		},
		"notification configuration token is redacted": {
			input: &tfe.NotificationConfiguration{
				ID:    "nc-1",
				Token: "notif-token",
			},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{"notif-token"},
		},
		"state version strips download and upload urls": {
			input: &tfe.StateVersion{
				ID:                      "sv-1",
				DownloadURL:             "https://archivist.example/download?token=secret",
				UploadURL:               "https://archivist.example/upload?token=secret",
				JSONUploadURL:           "https://archivist.example/json?token=secret",
				SanitizedStateUploadURL: new("https://archivist.example/sanitized?token=secret"),
			},
			wantNotContains: []string{"archivist.example", "token=secret"},
		},
		"sensitive value in a by-value array is redacted": {
			input:           [1]plainSecret{{Value: "hunter2", Sensitive: true}},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{"hunter2"},
		},
		"sensitive value behind an interface field is redacted": {
			input:           &ifaceHolder{Inner: plainSecret{Value: "hunter2", Sensitive: true}},
			wantContains:    []string{serialize.Redacted},
			wantNotContains: []string{"hunter2"},
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

			for _, notWant := range tc.wantNotContains {
				assert.NotContains(t, out, notWant)
			}

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

func TestMarshalRedactsNonStringValue(t *testing.T) {
	t.Parallel()

	// A sensitive Value whose kind is interface{} rather than a plain string must
	// still be redacted: the redactor fails closed instead of emitting cleartext.
	type sensitiveAnyValue struct {
		Key       string `json:"key"`
		Value     any    `json:"value"`
		Sensitive bool   `json:"sensitive"`
	}

	got, err := serialize.Marshal(&sensitiveAnyValue{
		Key:       "db_password",
		Value:     "hunter2-SECRET",
		Sensitive: true,
	})
	require.NoError(t, err)

	out := string(got)
	assert.NotContains(t, out, "hunter2-SECRET", "a non-string sensitive value must not leak cleartext")
	assert.Contains(t, out, serialize.Redacted)
	require.True(t, json.Valid(got), "output must be valid JSON")
}

func TestMarshalRedactsNestedSecret(t *testing.T) {
	t.Parallel()

	// A secret carried below the top-level struct must still be redacted: the
	// safety pass descends into nested structs, pointers, and slice elements
	// rather than stopping at the outermost struct.
	type inner struct {
		Token string `json:"token"`
	}

	type policy struct {
		Secret string `json:"secret"`
	}

	type outer struct {
		Name    string   `json:"name"`
		Nested  inner    `json:"nested"`
		Deep    *inner   `json:"deep"`
		Members []policy `json:"members"`
	}

	got, err := serialize.Marshal(&outer{
		Name:    "config",
		Nested:  inner{Token: "nested-TOKEN-SECRET"},
		Deep:    &inner{Token: "pointer-TOKEN-SECRET"},
		Members: []policy{{Secret: "slice-SECRET"}},
	})
	require.NoError(t, err)

	out := string(got)
	assert.NotContains(t, out, "nested-TOKEN-SECRET", "a secret in a nested struct must not leak")
	assert.NotContains(t, out, "pointer-TOKEN-SECRET", "a secret behind a pointer must not leak")
	assert.NotContains(t, out, "slice-SECRET", "a secret in a slice element must not leak")
	assert.Contains(t, out, serialize.Redacted)
	require.True(t, json.Valid(got), "output must be valid JSON")
}

func TestMarshalRedactsSecretInMap(t *testing.T) {
	t.Parallel()

	// A secret reachable only through a Go map must still be redacted: the safety
	// pass descends into map values, both a struct stored by value and a pointer
	// to one, rather than stopping at the map.
	type member struct {
		Secret string `json:"secret"`
	}

	type org struct {
		Name    string             `json:"name"`
		Members map[string]member  `json:"members"`
		Admins  map[string]*member `json:"admins"`
	}

	got, err := serialize.Marshal(&org{
		Name:    "acme",
		Members: map[string]member{"a": {Secret: "map-value-SECRET"}},
		Admins:  map[string]*member{"b": {Secret: "map-pointer-SECRET"}},
	})
	require.NoError(t, err)

	out := string(got)
	assert.NotContains(t, out, "map-value-SECRET", "a secret in a map struct value must not leak")
	assert.NotContains(t, out, "map-pointer-SECRET", "a secret behind a map pointer value must not leak")
	assert.Contains(t, out, serialize.Redacted)
	require.True(t, json.Valid(got), "output must be valid JSON")
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

func TestMarshalSliceOfModels(t *testing.T) {
	t.Parallel()

	vars := []*tfe.Variable{
		{ID: "var-1", Key: "a", Value: "one", Sensitive: true},
		{ID: "var-2", Key: "b", Value: "two", Sensitive: false},
	}

	got, err := serialize.Marshal(vars)
	require.NoError(t, err)

	out := string(got)

	assert.Contains(t, out, serialize.Redacted)
	assert.NotContains(t, out, `"one"`)
	assert.Contains(t, out, "two")
	require.True(t, json.Valid(got))
}
