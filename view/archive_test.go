package view_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// wsDir is the fixture workspace's archive-relative directory.
const wsDir = "projects/default/workspaces/app"

// writeFile writes content at an archive-relative path under root, creating
// parents, and returns the absolute path.
func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()

	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))

	return abs
}

// runJSON renders a minimal archived run document.
func runJSON(id, status, createdAt string) string {
	return `{"data":{"id":"` + id + `","type":"runs","attributes":{` +
		`"status":"` + status + `","message":"deploy things",` +
		`"source":"tfe-api","terraform-version":"1.9.0",` +
		`"created-at":"` + createdAt + `","has-changes":true,"is-destroy":false}}}`
}

// metaJSON renders a minimal archived state-version metadata sidecar.
func metaJSON(id, serial, createdAt string) string {
	return `{"data":{"id":"` + id + `","type":"state-versions","attributes":{` +
		`"serial":` + serial + `,"size":123,"status":"finalized",` +
		`"created-at":"` + createdAt + `"}}}`
}

// buildArchive lays out one organization with a workspace whose artifacts span
// all three physical forms: loose files, NDJSON roll-ups, and zip bundles. It
// returns the archive root (the directory holding the org directory).
func buildArchive(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "my-org")

	writeFile(t, org, "org.json",
		`{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`)
	writeFile(t, org, "projects/default/project.json",
		`{"data":{"id":"prj-1","type":"projects","attributes":{"name":"default"}}}`)
	writeFile(t, org, wsDir+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{`+
			`"name":"app","terraform-version":"1.9.0","execution-mode":"remote",`+
			`"auto-apply":false,"resource-count":7,"created-at":"2024-01-01T00:00:00Z"}}}`)
	writeFile(t, org, wsDir+"/variables.json",
		`{"data":[`+
			`{"id":"var-1","type":"vars","attributes":{"key":"region","value":"us-east-1","category":"terraform"}},`+
			`{"id":"var-2","type":"vars","attributes":{"key":"token","value":"[REDACTED]","category":"env","sensitive":true}}`+
			`]}`)

	// run-new: settled, its heavy log bundled and its config version rolled up.
	writeFile(t, org, wsDir+"/runs/run-new/run.json",
		runJSON("run-new", "applied", "2024-02-01T10:00:00Z"))

	planAbs := writeFile(t, org, wsDir+"/runs/run-new/plan.log", "plan output line\n")
	cvAbs := writeFile(t, org, wsDir+"/runs/run-new/config-version.json",
		`{"data":{"id":"cv-1","type":"configuration-versions","attributes":{"source":"tfe-api"}}}`)

	// run-old: still fully loose.
	writeFile(t, org, wsDir+"/runs/run-old/run.json",
		runJSON("run-old", "errored", "2024-01-01T10:00:00Z"))
	writeFile(t, org, wsDir+"/runs/run-old/comments.json", `{"data":[]}`)

	// sv-2: newest, fully loose.
	writeFile(t, org, wsDir+"/state-versions/20240102T030405Z-sv-2.meta.json",
		metaJSON("sv-2", "3", "2024-01-02T03:04:05Z"))
	writeFile(t, org, wsDir+"/state-versions/20240102T030405Z-sv-2.tfstate.json",
		`{"serial":3}`)

	// sv-1: older, its meta rolled up and its raw blob bundled.
	svMetaAbs := writeFile(t, org, wsDir+"/state-versions/20240101T000000Z-sv-1.meta.json",
		metaJSON("sv-1", "2", "2024-01-01T00:00:00Z"))
	svBlobAbs := writeFile(t, org, wsDir+"/state-versions/20240101T000000Z-sv-1.tfstate.json",
		`{"serial":2}`)

	_, err := seal.Seal(filepath.Join(org, filepath.FromSlash(wsDir), "bundles", "logs.gen0001.zip"),
		[]seal.Member{{Name: wsDir + "/runs/run-new/plan.log", Source: planAbs, Compress: true}})
	require.NoError(t, err)

	_, err = seal.Seal(filepath.Join(org, filepath.FromSlash(wsDir), "bundles", "state.gen0001.zip"),
		[]seal.Member{{Name: wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json", Source: svBlobAbs}})
	require.NoError(t, err)

	err = seal.Rollup(filepath.Join(org, filepath.FromSlash(wsDir), "rollups", "config-versions.ndjson"),
		[]seal.Member{{Name: wsDir + "/runs/run-new/config-version.json", Source: cvAbs}})
	require.NoError(t, err)

	err = seal.Rollup(filepath.Join(org, filepath.FromSlash(wsDir), "rollups", "state-versions.ndjson"),
		[]seal.Member{{Name: wsDir + "/state-versions/20240101T000000Z-sv-1.meta.json", Source: svMetaAbs}})
	require.NoError(t, err)

	return root
}

func TestOpenArchiveToleratesOversizedSidecarLine(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)

	// Corrupt a bundle sidecar with a merged/garbage run far larger than a
	// size-capped scanner's line limit (bit rot, a partial restore). The index
	// must skip it, not dead-end the whole workspace on bufio.ErrTooLong.
	sidecars, err := filepath.Glob(
		filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "bundles", "*.sidecar.ndjson"))
	require.NoError(t, err)
	require.NotEmpty(t, sidecars, "the fixture bundles carry sidecars")

	f, err := os.OpenFile(sidecars[0], os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(strings.Repeat("x", (1<<20)+1) + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	orgs, err := view.OpenArchive(root)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	ws := orgs[0].Workspace("default", "app")

	// The valid sidecar entry indexed alongside the garbage still resolves.
	data, err := ws.Open(wsDir + "/runs/run-new/plan.log")
	require.NoError(t, err, "an oversized sidecar line must not fail the whole index")
	assert.Equal(t, "plan output line\n", string(data))
}

// openWorkspace opens the fixture archive and returns its one workspace.
func openWorkspace(t *testing.T) *view.Workspace {
	t.Helper()

	orgs, err := view.OpenArchive(buildArchive(t))
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	return orgs[0].Workspace("default", "app")
}

func TestOpenArchive(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dir  func(t *testing.T) string
		want []string
		err  error
	}{
		"archive root": {
			dir:  buildArchive,
			want: []string{"my-org"},
		},
		"org directory": {
			dir: func(t *testing.T) string {
				t.Helper()

				return filepath.Join(buildArchive(t), "my-org")
			},
			want: []string{"my-org"},
		},
		"not an archive": {
			dir: func(t *testing.T) string {
				t.Helper()

				return t.TempDir()
			},
			err: view.ErrNotArchive,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			orgs, err := view.OpenArchive(tt.dir(t))
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)

			names := make([]string, 0, len(orgs))
			for _, org := range orgs {
				names = append(names, org.Name)
			}

			require.Equal(t, tt.want, names)
		})
	}
}

func TestOrgListings(t *testing.T) {
	t.Parallel()

	orgs, err := view.OpenArchive(buildArchive(t))
	require.NoError(t, err)

	org := orgs[0]

	projects, err := org.Projects()
	require.NoError(t, err)
	require.Equal(t, []string{"default"}, projects)

	workspaces, err := org.Workspaces("default")
	require.NoError(t, err)
	require.Equal(t, []string{"app"}, workspaces)

	stacks, err := org.Stacks("default")
	require.NoError(t, err)
	require.Empty(t, stacks)
}

func TestWorkspaceOpen(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    string
		err     error
	}{
		"loose file": {
			relPath: wsDir + "/runs/run-old/comments.json",
			want:    `{"data":[]}`,
		},
		"rolled-up member": {
			relPath: wsDir + "/runs/run-new/config-version.json",
			want:    `{"data":{"id":"cv-1","type":"configuration-versions","attributes":{"source":"tfe-api"}}}`,
		},
		"bundled member": {
			relPath: wsDir + "/runs/run-new/plan.log",
			want:    "plan output line\n",
		},
		"bundled state blob": {
			relPath: wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json",
			want:    `{"serial":2}`,
		},
		"missing object": {
			relPath: wsDir + "/runs/run-new/apply.log",
			err:     view.ErrObjectNotFound,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ws := openWorkspace(t)

			data, err := ws.Open(tt.relPath)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, string(data))
		})
	}
}

func TestWorkspaceRuns(t *testing.T) {
	t.Parallel()

	ws := openWorkspace(t)

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 2)

	require.Equal(t, "run-new", runs[0].ID)
	require.Equal(t, "applied", runs[0].Status)
	require.Equal(t, "deploy things", runs[0].Message)
	require.Equal(t, "tfe-api", runs[0].Source)
	require.Equal(t, "1.9.0", runs[0].TerraformVersion)
	require.True(t, runs[0].HasChanges)
	require.False(t, runs[0].IsDestroy)

	require.Equal(t, "run-old", runs[1].ID)
	require.Equal(t, "errored", runs[1].Status)
}

func TestRunArtifacts(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		runID string
		want  []string
	}{
		"sealed artifacts list beside loose": {
			runID: "run-new",
			want: []string{
				wsDir + "/runs/run-new/plan.log",
				wsDir + "/runs/run-new/config-version.json",
			},
		},
		"run.json is excluded": {
			runID: "run-old",
			want: []string{
				wsDir + "/runs/run-old/comments.json",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ws := openWorkspace(t)

			artifacts, err := ws.RunArtifacts(tt.runID)
			require.NoError(t, err)
			require.Equal(t, tt.want, artifacts)
		})
	}
}

func TestStateVersions(t *testing.T) {
	t.Parallel()

	ws := openWorkspace(t)

	versions, err := ws.StateVersions()
	require.NoError(t, err)
	require.Len(t, versions, 2)

	require.Equal(t, "sv-2", versions[0].ID)
	require.EqualValues(t, 3, versions[0].Serial)
	require.True(t, versions[0].HasRaw)
	require.False(t, versions[0].HasJSON)

	// Sv-1's meta is rolled up and its raw blob is bundled; both still resolve.
	require.Equal(t, "sv-1", versions[1].ID)
	require.EqualValues(t, 2, versions[1].Serial)
	require.Equal(t, "finalized", versions[1].Status)
	require.True(t, versions[1].HasRaw)
	require.False(t, versions[1].HasJSON)
}
