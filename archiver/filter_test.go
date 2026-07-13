package archiver_test

import (
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/archiver"
)

func TestFilterProjects(t *testing.T) {
	t.Parallel()

	projects := []*tfe.Project{
		{ID: "prj-1", Name: "networking"},
		{ID: "prj-2", Name: "platform"},
		{ID: "prj-3", Name: "sandbox"},
	}

	tests := map[string]struct {
		filter []string
		want   []string
	}{
		"empty filter admits every project": {
			filter: nil,
			want:   []string{"networking", "platform", "sandbox"},
		},
		"named projects keep the listing order": {
			filter: []string{"sandbox", "networking"},
			want:   []string{"networking", "sandbox"},
		},
		"unknown name admits nothing": {
			filter: []string{"nope"},
			want:   []string{},
		},
		"matching is exact and case-sensitive": {
			filter: []string{"Networking"},
			want:   []string{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := archiver.FilterProjects(tc.filter, projects)

			names := make([]string, 0, len(got))
			for _, p := range got {
				names = append(names, p.Name)
			}

			assert.Equal(t, tc.want, names)
		})
	}
}

func TestFilterWorkspaces(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		"prj-1": "networking",
		"prj-2": "platform",
	}

	workspaces := []*tfe.Workspace{
		{Name: "vpc", Project: &tfe.Project{ID: "prj-1"}},
		{Name: "dns", Project: &tfe.Project{ID: "prj-1"}},
		{Name: "cluster", Project: &tfe.Project{ID: "prj-2"}},
		{Name: "orphan", Project: nil},
	}

	tests := map[string]struct {
		projectFilter   []string
		workspaceFilter []string
		want            []string
	}{
		"no filters admit every workspace": {
			want: []string{"vpc", "dns", "cluster", "orphan"},
		},
		"project filter admits its workspaces": {
			projectFilter: []string{"networking"},
			want:          []string{"vpc", "dns"},
		},
		"workspace filter admits the named workspaces": {
			workspaceFilter: []string{"dns", "cluster"},
			want:            []string{"dns", "cluster"},
		},
		"both filters intersect": {
			projectFilter:   []string{"networking"},
			workspaceFilter: []string{"dns", "cluster"},
			want:            []string{"dns"},
		},
		"default project name admits an unresolved workspace": {
			projectFilter: []string{archiver.DefaultProjectName},
			want:          []string{"orphan"},
		},
		"unknown workspace name admits nothing": {
			workspaceFilter: []string{"nope"},
			want:            []string{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := archiver.FilterWorkspaces(tc.projectFilter, tc.workspaceFilter, names, workspaces)

			wsNames := make([]string, 0, len(got))
			for _, ws := range got {
				wsNames = append(wsNames, ws.Name)
			}

			assert.Equal(t, tc.want, wsNames)
		})
	}
}

func TestUnmatchedFilter(t *testing.T) {
	t.Parallel()

	identity := func(s string) string { return s }

	tests := map[string]struct {
		filter []string
		listed []string
		want   []string
	}{
		"empty filter never reports unmatched": {
			filter: nil,
			listed: []string{"a"},
			want:   nil,
		},
		"fully matched filter reports nothing": {
			filter: []string{"a", "b"},
			listed: []string{"b", "a", "c"},
			want:   nil,
		},
		"unmatched entries keep the filter order": {
			filter: []string{"z", "a", "y"},
			listed: []string{"a"},
			want:   []string{"z", "y"},
		},
		"empty listing leaves the whole filter unmatched": {
			filter: []string{"a"},
			listed: nil,
			want:   []string{"a"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := archiver.UnmatchedFilter(tc.filter, tc.listed, identity)
			assert.Equal(t, tc.want, got)
		})
	}
}
