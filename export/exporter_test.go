package export_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/export"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// wsDir is the fixture workspace's archive-relative directory.
const wsDir = "projects/default/workspaces/app"

// secretMarker is the value every sensitive fixture variable stores. The
// archive may hold such a value (legacy data, a non-blanking API); the export
// must never emit it.
const secretMarker = "SECRET-MARKER-VALUE"

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
func runJSON(id, status, message, createdAt string) string {
	return `{"data":{"id":"` + id + `","type":"runs","attributes":{` +
		`"status":"` + status + `","message":"` + message + `",` +
		`"source":"tfe-api","terraform-version":"1.9.0",` +
		`"created-at":"` + createdAt + `","has-changes":true,"is-destroy":false}}}`
}

// metaJSON renders a minimal archived state-version metadata sidecar.
func metaJSON(id, serial, createdAt string) string {
	return `{"data":{"id":"` + id + `","type":"state-versions","attributes":{` +
		`"serial":` + serial + `,"size":123,"status":"finalized",` +
		`"created-at":"` + createdAt + `"}}}`
}

// readmeContent is the fixture workspace's user-authored readme, copied
// verbatim by the export.
const readmeContent = "# App\n\nUser-authored | readme <em>kept verbatim</em>.\n"

// buildArchive lays out one organization exercising every page the export
// renders, with one run's artifacts and the older state version sealed so the
// tables read through roll-ups and bundles. It returns the archive root.
func buildArchive(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "my-org")

	writeFile(t, org, "org.json",
		`{"data":{"id":"org-1","type":"organizations","attributes":{`+
			`"name":"my-org","email":"admin@example.com","created-at":"2023-06-01T00:00:00Z"}}}`)
	writeFile(t, org, "memberships.json",
		`{"data":[{"id":"ou-1","type":"organization-memberships","attributes":{}},`+
			`{"id":"ou-2","type":"organization-memberships","attributes":{}}]}`)
	writeFile(t, org, "run-tasks.json",
		`{"data":[{"id":"task-1","type":"tasks","attributes":{"name":"scan"}}]}`)
	writeFile(t, org, "users/user-1.json",
		`{"data":{"id":"user-1","type":"users","attributes":{"username":"jane"}}}`)
	writeFile(t, org, "agent-pools/apool-1.json",
		`{"data":{"id":"apool-1","type":"agent-pools","attributes":{"name":"default"}}}`)

	writeFile(t, org, "teams/team-1/team.json",
		`{"data":{"id":"team-1","type":"teams","attributes":{`+
			`"name":"owners","visibility":"secret","users-count":2}}}`)

	writeFile(t, org, "variable-sets/vs-1/variable-set.json",
		`{"data":{"id":"vs-1","type":"varsets","attributes":{`+
			`"name":"shared-set","description":"org-wide values","global":true}}}`)
	writeFile(t, org, "variable-sets/vs-1/variables.json",
		`{"data":[`+
			`{"id":"var-10","type":"vars","attributes":{"key":"proxy_url","value":"http://proxy","category":"env"}},`+
			`{"id":"var-11","type":"vars","attributes":{"key":"api_token","value":"`+secretMarker+`","category":"env","sensitive":true}}`+
			`]}`)

	writeFile(t, org, "policy-sets/ps-1/policy-set.json",
		`{"data":{"id":"ps-1","type":"policy-sets","attributes":{`+
			`"name":"security","kind":"sentinel","global":false,"policy-count":2}}}`)
	writeFile(t, org, "policy-sets/ps-1/parameters.json",
		`{"data":[`+
			`{"id":"var-20","type":"vars","attributes":{"key":"license_key","value":"`+secretMarker+`","category":"policy-set","sensitive":true}}`+
			`]}`)

	writeFile(t, org, "projects/default/project.json",
		`{"data":{"id":"prj-1","type":"projects","attributes":{`+
			`"name":"default","description":"the default project","created-at":"2023-06-02T00:00:00Z"}}}`)

	writeFile(t, org, wsDir+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{`+
			`"name":"app","description":"the app","terraform-version":"1.9.0","execution-mode":"remote",`+
			`"auto-apply":false,"resource-count":7,"created-at":"2024-01-01T00:00:00Z",`+
			`"vcs-repo":{"identifier":"acme/app"}}}}`)
	writeFile(t, org, wsDir+"/variables.json",
		`{"data":[`+
			`{"id":"var-1","type":"vars","attributes":{"key":"region","value":"us-east-1","category":"terraform","hcl":false}},`+
			`{"id":"var-2","type":"vars","attributes":{"key":"token","value":"`+secretMarker+`","category":"env","sensitive":true}}`+
			`]}`)
	writeFile(t, org, wsDir+"/"+"readme.md", readmeContent)

	// run-new: settled, its heavy log bundled and its config version rolled
	// up; its message carries markdown-hostile characters the page must
	// neutralize.
	writeFile(t, org, wsDir+"/runs/run-new/run.json",
		runJSON("run-new", "applied", `deploy |pipes| & <b>markup</b>`, "2024-02-01T10:00:00Z"))

	planAbs := writeFile(t, org, wsDir+"/runs/run-new/plan.log", "plan output line\n")
	cvAbs := writeFile(t, org, wsDir+"/runs/run-new/config-version.json",
		`{"data":{"id":"cv-1","type":"configuration-versions","attributes":{"source":"tfe-api"}}}`)

	// run-old: still fully loose.
	writeFile(t, org, wsDir+"/runs/run-old/run.json",
		runJSON("run-old", "errored", "first attempt", "2024-01-01T10:00:00Z"))
	writeFile(t, org, wsDir+"/runs/run-old/apply.log", "apply output\n")

	// sv-2: newest, fully loose.
	writeFile(t, org, wsDir+"/state-versions/20240102T030405Z-sv-2.meta.json",
		metaJSON("sv-2", "3", "2024-01-02T03:04:05Z"))
	writeFile(t, org, wsDir+"/state-versions/20240102T030405Z-sv-2.tfstate.json",
		`{"serial":3,"secret":"`+secretMarker+`"}`)

	// sv-1: older, its meta rolled up and its raw blob bundled.
	svMetaAbs := writeFile(t, org, wsDir+"/state-versions/20240101T000000Z-sv-1.meta.json",
		metaJSON("sv-1", "2", "2024-01-01T00:00:00Z"))
	svBlobAbs := writeFile(t, org, wsDir+"/state-versions/20240101T000000Z-sv-1.tfstate.json",
		`{"serial":2,"secret":"`+secretMarker+`"}`)

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

	writeFile(t, org, "projects/default/stacks/net/stack.json",
		`{"data":{"id":"st-1","type":"stacks","attributes":{`+
			`"name":"net","description":"network stack","created-at":"2024-03-01T00:00:00Z",`+
			`"updated-at":"2024-03-02T00:00:00Z"}}}`)
	writeFile(t, org, "projects/default/stacks/net/deployments/production/deployment.json",
		`{"data":{"id":"stdg-1","type":"stack-deployment-groups","attributes":{`+
			`"name":"production","created-at":"2024-03-01T12:00:00Z"}}}`)

	return root
}

// openFixture opens the fixture archive as a [*view.Archive].
func openFixture(t *testing.T, root string) *view.Archive {
	t.Helper()

	orgs, err := view.OpenArchive(root)
	require.NoError(t, err)

	return view.NewArchive(orgs)
}

// runExport exports the fixture archive into a fresh target and returns the
// target and the run's summary.
func runExport(t *testing.T) (string, export.Summary) {
	t.Helper()

	root := buildArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	sum, err := export.New(openFixture(t, root), target).Run(t.Context())
	require.NoError(t, err)

	return target, sum
}

// readTree returns every generated file's target-relative forward-slash path
// mapped to its content.
func readTree(t *testing.T, target string) map[string]string {
	t.Helper()

	files := make(map[string]string)

	err := filepath.WalkDir(target, func(abs string, d fs.DirEntry, err error) error {
		require.NoError(t, err)

		if d.IsDir() {
			return nil
		}

		data, readErr := os.ReadFile(abs)
		require.NoError(t, readErr)

		rel, relErr := filepath.Rel(target, abs)
		require.NoError(t, relErr)

		files[filepath.ToSlash(rel)] = string(data)

		return nil
	})
	require.NoError(t, err)

	return files
}

func TestExportTree(t *testing.T) {
	t.Parallel()

	target, sum := runExport(t)
	files := readTree(t, target)

	want := []string{
		"index.md",
		"my-org/index.md",
		"my-org/policy-sets/index.md",
		"my-org/projects/default/index.md",
		"my-org/projects/default/stacks/net/index.md",
		"my-org/projects/default/workspaces/app/index.md",
		"my-org/projects/default/workspaces/app/readme.md",
		"my-org/projects/index.md",
		"my-org/teams/index.md",
		"my-org/variable-sets/index.md",
	}

	got := make([]string, 0, len(files))
	for rel := range files {
		got = append(got, rel)
	}

	assert.ElementsMatch(t, want, got)
	assert.Equal(t, export.Summary{Pages: len(want), Orgs: 1}, sum)
}

func TestExportWithholdsSensitiveValues(t *testing.T) {
	t.Parallel()

	target, _ := runExport(t)
	files := readTree(t, target)

	for rel, content := range files {
		assert.NotContains(t, content, secretMarker,
			"%s must not carry a sensitive fixture value", rel)
	}

	ws := files["my-org/projects/default/workspaces/app/index.md"]
	assert.Contains(t, ws, "us-east-1", "a plain variable's value renders")
	assert.Contains(t, ws, "token", "a sensitive variable's key renders")
	assert.Contains(t, ws, "(sensitive)", "a sensitive variable's value is marked withheld")

	assert.Contains(t, files["my-org/variable-sets/index.md"], "(sensitive)")
	assert.Contains(t, files["my-org/policy-sets/index.md"], "(sensitive)")
}

// TestExportPointsAtCLI asserts the withheld-content sections carry runnable
// retrieval snippets, independent of golden churn: a show of one concrete
// archived object and an extract of the section directory.
func TestExportPointsAtCLI(t *testing.T) {
	t.Parallel()

	target, _ := runExport(t)
	files := readTree(t, target)

	ws := files["my-org/projects/default/workspaces/app/index.md"]
	wsDir := "my-org/projects/default/workspaces/app"

	tests := map[string]struct {
		want string
	}{
		"run artifact show": {
			want: "hcp_archiver show <archive-dir> '" + wsDir + "/runs/run-new/plan.log'",
		},
		"runs extract": {
			want: "hcp_archiver extract <archive-dir> '" + wsDir + "/runs' --target <output-dir>",
		},
		"state version show": {
			want: "hcp_archiver show <archive-dir> '" + wsDir + "/state-versions/20240102T030405Z-sv-2.tfstate.json'",
		},
		"state versions extract": {
			want: "hcp_archiver extract <archive-dir> '" + wsDir + "/state-versions' --target <output-dir>",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Contains(t, ws, tc.want)
		})
	}
}

func TestExportCopiesReadmeVerbatim(t *testing.T) {
	t.Parallel()

	target, _ := runExport(t)
	files := readTree(t, target)

	assert.Equal(t, readmeContent, files["my-org/projects/default/workspaces/app/readme.md"])
	assert.Contains(t, files["my-org/projects/default/workspaces/app/index.md"], "readme.md")
}

// TestExportPages pins every kind of generated page against a golden file.
// The workspace page reads through the sealed forms: run-new's plan.log lists
// from its bundle sidecar and sv-1's row parses from the roll-up, alongside
// the loose artifacts. The readme copy is not golden-pinned; its verbatim
// equality is asserted directly by [TestExportCopiesReadmeVerbatim].
func TestExportPages(t *testing.T) {
	t.Parallel()

	target, _ := runExport(t)
	files := readTree(t, target)

	tests := map[string]struct {
		path string
	}{
		"archive index":  {path: "index.md"},
		"org index":      {path: "my-org/index.md"},
		"teams":          {path: "my-org/teams/index.md"},
		"variable sets":  {path: "my-org/variable-sets/index.md"},
		"policy sets":    {path: "my-org/policy-sets/index.md"},
		"projects index": {path: "my-org/projects/index.md"},
		"project":        {path: "my-org/projects/default/index.md"},
		"workspace":      {path: "my-org/projects/default/workspaces/app/index.md"},
		"stack":          {path: "my-org/projects/default/stacks/net/index.md"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			content, ok := files[tc.path]
			require.True(t, ok, "the export must generate %s", tc.path)

			golden.RequireEqual(t, []byte(content))
		})
	}
}

func TestExportTargetRefusals(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		target func(t *testing.T, root string) string
		force  bool
		err    error
	}{
		"a regular file target is refused": {
			target: func(t *testing.T, _ string) string {
				t.Helper()

				return writeFile(t, t.TempDir(), "site", "not a directory")
			},
			err: export.ErrTargetNotDir,
		},
		"a non-empty target is refused without force": {
			target: func(t *testing.T, _ string) string {
				t.Helper()

				dir := t.TempDir()
				writeFile(t, dir, "stale.md", "old content")

				return dir
			},
			err: export.ErrTargetNotEmpty,
		},
		"a forced target containing the archive is refused": {
			target: func(t *testing.T, root string) string {
				t.Helper()

				return root
			},
			force: true,
			err:   export.ErrTargetOverlapsArchive,
		},
		"a target inside the archive is refused": {
			target: func(t *testing.T, root string) string {
				t.Helper()

				return filepath.Join(root, "my-org", "site")
			},
			err: export.ErrTargetOverlapsArchive,
		},
		"an empty target is refused": {
			target: func(t *testing.T, _ string) string {
				t.Helper()

				return ""
			},
			err: export.ErrNoTarget,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildArchive(t)

			var opts []export.Option

			if tc.force {
				opts = append(opts, export.WithForce())
			}

			_, err := export.New(openFixture(t, root), tc.target(t, root), opts...).Run(t.Context())
			require.ErrorIs(t, err, tc.err)
		})
	}
}

func TestExportForceReplacesContents(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	target := t.TempDir()
	stale := writeFile(t, target, "stale/old.md", "old content")

	_, err := export.New(openFixture(t, root), target, export.WithForce()).Run(t.Context())
	require.NoError(t, err)

	assert.NoFileExists(t, stale, "force clears the target's previous contents")
	assert.FileExists(t, filepath.Join(target, "index.md"))
}

func TestExportStopsOnCancel(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := export.New(openFixture(t, root), target).Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestExportEscapesRunMessage(t *testing.T) {
	t.Parallel()

	target, _ := runExport(t)
	files := readTree(t, target)

	ws := files["my-org/projects/default/workspaces/app/index.md"]
	assert.NotContains(t, ws, "<b>markup</b>", "run messages cannot smuggle inline HTML")
	assert.Contains(t, ws, `deploy \|pipes\| &amp; &lt;b&gt;markup&lt;/b&gt;`)
	assert.Contains(t, ws, "plan.log", "run-new's bundled plan.log must list among the archived artifacts")
}
