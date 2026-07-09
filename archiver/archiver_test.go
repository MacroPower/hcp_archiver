package archiver_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MacroPower/tfc_archiver/archiver"
	"github.com/MacroPower/tfc_archiver/config"
)

func TestNew(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Token:                "secret",
		Address:              config.DefaultAddress,
		OutputDir:            t.TempDir(),
		ProgressMode:         config.ProgressModeQuiet,
		WorkspaceConcurrency: config.DefaultWorkspaceConcurrency,
	}

	buf := &bytes.Buffer{}

	a := archiver.New(cfg, archiver.WithWriter(buf))
	require.NotNil(t, a)
}

func TestRunInvalidConfig(t *testing.T) {
	t.Parallel()

	// A config missing its output directory fails validation before any I/O.
	cfg := &config.Config{
		Token:                "secret",
		Address:              config.DefaultAddress,
		ProgressMode:         config.ProgressModeQuiet,
		WorkspaceConcurrency: config.DefaultWorkspaceConcurrency,
	}

	a := archiver.New(cfg, archiver.WithWriter(&bytes.Buffer{}))

	err := a.Run(t.Context())
	require.ErrorIs(t, err, config.ErrMissingOutputDir)
}

func TestResolveOrgs(t *testing.T) {
	t.Parallel()

	errList := errors.New("list called")

	tests := map[string]struct {
		org       string
		listOrgs  []string
		listErr   error
		want      []string
		wantErr   error
		wantCalls int
	}{
		"single named org skips listing": {
			org:       "acme",
			listErr:   errList,
			want:      []string{"acme"},
			wantCalls: 0,
		},
		"empty org lists every visible org": {
			org:       "",
			listOrgs:  []string{"one", "two"},
			want:      []string{"one", "two"},
			wantCalls: 1,
		},
		"list error propagates": {
			org:       "",
			listErr:   errList,
			wantErr:   errList,
			wantCalls: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			list := func(_ context.Context) ([]string, error) {
				calls++

				return tc.listOrgs, tc.listErr
			}

			got, err := archiver.ResolveOrgs(t.Context(), tc.org, list)

			assert.Equal(t, tc.wantCalls, calls)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestProjectNameFor(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		"prj-1": "networking",
		"prj-2": "",
	}

	tests := map[string]struct {
		ws   *tfe.Workspace
		want string
	}{
		"resolves from the map by id": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-1"}},
			want: "networking",
		},
		"nil project falls back to default": {
			ws:   &tfe.Workspace{Project: nil},
			want: archiver.DefaultProjectName,
		},
		"unknown id falls back to the relation name": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-9", Name: "hydrated"}},
			want: "hydrated",
		},
		"blank map entry falls back to the relation name": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-2", Name: "from-relation"}},
			want: "from-relation",
		},
		"unknown id and no relation name falls back to default": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-9"}},
			want: archiver.DefaultProjectName,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := archiver.ProjectNameFor(names, tc.ws)
			assert.Equal(t, tc.want, got)
		})
	}
}
