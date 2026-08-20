package view_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// The benchmark fixture's shape, chosen to resemble a long-lived workspace
// rather than the small correctness fixtures: most runs and state versions are
// sealed (the steady state, once the sealer has swept), a few of the newest
// are still loose (in flight), and every run carries the artifact spread a
// real run leaves behind.
const (
	benchRuns          = 50
	benchLooseRuns     = 4
	benchStateVersions = 20
	benchLooseStates   = 2
	benchWorkspaces    = 10

	// The heavy leaf every sealed fixture run has bundled.
	benchBundledArtifact = "plan.log"
)

// benchRolledArtifacts are the metadata leaves each sealed fixture run has
// rolled up.
var benchRolledArtifacts = []string{"config-version.json", "comments.json", "run-events.json"}

// benchWrite writes content at an archive-relative path under root, creating
// parents, and returns the absolute path. It is [writeFile] for a benchmark,
// which has no *testing.T to hand.
func benchWrite(tb testing.TB, root, rel, content string) string {
	tb.Helper()

	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(tb, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(tb, os.WriteFile(abs, []byte(content), 0o600))

	return abs
}

// buildBenchArchive lays out an organization of sealed workspaces under a
// fresh temporary directory and returns the archive root. Sealing runs here,
// outside every timed loop, so the benchmarks measure reads over the shape the
// sealer leaves rather than the sealing itself.
func buildBenchArchive(tb testing.TB, workspaces int) string {
	tb.Helper()

	root := tb.TempDir()
	org := filepath.Join(root, "my-org")

	benchWrite(tb, org, "org.json",
		`{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`)
	benchWrite(tb, org, "projects/default/project.json",
		`{"data":{"id":"prj-1","type":"projects","attributes":{"name":"default"}}}`)

	for i := range workspaces {
		buildBenchWorkspace(tb, org, fmt.Sprintf("ws-%03d", i))
	}

	return root
}

// buildBenchWorkspace lays out one workspace's runs and state versions, then
// folds all but the newest few into a roll-up and a bundle, the mix a swept
// workspace holds.
func buildBenchWorkspace(tb testing.TB, org, name string) {
	tb.Helper()

	dir := "projects/default/workspaces/" + name

	benchWrite(tb, org, dir+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{"name":"`+name+`"}}}`)

	var (
		rolled, bundled []seal.Member
		emptied         []string
	)

	for i := range benchRuns {
		id := fmt.Sprintf("run-%04d", i)
		created := fmt.Sprintf("2024-01-01T%02d:00:00Z", i%24)
		runDir := dir + "/runs/" + id

		runAbs := benchWrite(tb, org, runDir+"/run.json", runJSON(id, "applied", created))
		logAbs := benchWrite(tb, org, runDir+"/"+benchBundledArtifact, "plan output\n")

		var artifacts []seal.Member

		for _, leaf := range benchRolledArtifacts {
			abs := benchWrite(tb, org, runDir+"/"+leaf, `{"data":[]}`)
			artifacts = append(artifacts, seal.Member{Name: runDir + "/" + leaf, Source: abs})
		}

		// The newest runs stay wholly loose: in flight, nothing sealed yet.
		if i >= benchRuns-benchLooseRuns {
			continue
		}

		rolled = append(rolled, seal.Member{Name: runDir + "/run.json", Source: runAbs})
		rolled = append(rolled, artifacts...)
		bundled = append(bundled,
			seal.Member{Name: runDir + "/" + benchBundledArtifact, Source: logAbs, Compress: true})
		emptied = append(emptied, filepath.Join(org, filepath.FromSlash(runDir)))
	}

	stateRolled, stateBundled := buildBenchStates(tb, org, dir)

	sealBench(tb, org, dir, "runs", rolled, bundled)
	sealBench(tb, org, dir, "state-versions", stateRolled, stateBundled)

	// The sealer removes a run directory it has emptied, so a swept workspace
	// holds no directory for a fully sealed run. Reproducing that is what makes
	// the fixture's read profile the real one: a listing that names those runs
	// anyway would keep every probe the reader spends on them.
	for _, runDir := range emptied {
		require.NoError(tb, os.Remove(runDir))
	}
}

// buildBenchStates lays out one workspace's state versions loose and returns
// the members to fold: each older version's meta into a roll-up and its raw
// blob into a bundle.
func buildBenchStates(tb testing.TB, org, dir string) ([]seal.Member, []seal.Member) {
	tb.Helper()

	var rolled, bundled []seal.Member

	for i := range benchStateVersions {
		stem := fmt.Sprintf("2024010%dT0000%02dZ-sv-%d", i%10, i, i)
		rel := dir + "/state-versions/" + stem

		metaAbs := benchWrite(tb, org, rel+".meta.json",
			metaJSON(fmt.Sprintf("sv-%d", i), fmt.Sprint(i), "2024-01-01T00:00:00Z"))
		blobAbs := benchWrite(tb, org, rel+".tfstate.json", `{"serial":1}`)

		if i >= benchStateVersions-benchLooseStates {
			continue
		}

		rolled = append(rolled, seal.Member{Name: rel + ".meta.json", Source: metaAbs})
		bundled = append(bundled, seal.Member{Name: rel + ".tfstate.json", Source: blobAbs})
	}

	return rolled, bundled
}

// sealBench folds one generation's members into the workspace's roll-up and
// bundle, named after the surface they came from.
func sealBench(tb testing.TB, org, dir, surface string, rolled, bundled []seal.Member) {
	tb.Helper()

	wsAbs := filepath.Join(org, filepath.FromSlash(dir))

	require.NoError(tb, seal.Rollup(filepath.Join(wsAbs, "rollups", surface+".ndjson"), rolled))

	_, err := seal.Seal(filepath.Join(wsAbs, "bundles", surface+".gen0001.zip"), bundled)
	require.NoError(tb, err)
}

// benchOrg opens the fixture archive, returning an organization whose
// workspace handles (and so their sealed indexes) are unbuilt.
func benchOrg(tb testing.TB, root string) *view.Org {
	tb.Helper()

	orgs, err := view.OpenArchive(root)
	require.NoError(tb, err)
	require.Len(tb, orgs, 1)

	return orgs[0]
}

// benchWorkspace opens the fixture archive and returns a workspace handle with
// a cold sealed index. Handles are memoized per organization and their indexes
// are built once and kept, so a benchmark that means to measure the build must
// reopen the archive rather than reuse a handle.
func benchWorkspace(tb testing.TB, root string) *view.Workspace {
	tb.Helper()

	return benchOrg(tb, root).Workspace("default", "ws-000")
}

// benchColdWarm runs one read over both index states. Cold reopens the archive
// every iteration, so it times the open and the sealed index build alongside
// the read: what the first descent into a workspace costs, and what an export
// pays once per workspace. Warm reuses the built index, what every later
// screen push costs.
func benchColdWarm(b *testing.B, root string, read func(b *testing.B, ws *view.Workspace)) {
	b.Helper()

	b.Run("cold", func(b *testing.B) {
		for b.Loop() {
			read(b, benchWorkspace(b, root))
		}
	})

	b.Run("warm", func(b *testing.B) {
		ws := benchWorkspace(b, root)
		read(b, ws)

		for b.Loop() {
			read(b, ws)
		}
	})
}

func BenchmarkWorkspaceRuns(b *testing.B) {
	root := buildBenchArchive(b, 1)

	benchColdWarm(b, root, func(b *testing.B, ws *view.Workspace) {
		b.Helper()

		_, err := ws.Runs()
		require.NoError(b, err)
	})
}

func BenchmarkWorkspaceStateVersions(b *testing.B) {
	root := buildBenchArchive(b, 1)

	benchColdWarm(b, root, func(b *testing.B, ws *view.Workspace) {
		b.Helper()

		_, err := ws.StateVersions()
		require.NoError(b, err)
	})
}

func BenchmarkRunArtifactsPerRun(b *testing.B) {
	root := buildBenchArchive(b, 1)

	benchColdWarm(b, root, func(b *testing.B, ws *view.Workspace) {
		b.Helper()

		runs, err := ws.Runs()
		require.NoError(b, err)

		for _, run := range runs {
			_, err = ws.RunArtifacts(run.ID)
			require.NoError(b, err)
		}
	})
}

func BenchmarkAllRunArtifacts(b *testing.B) {
	root := buildBenchArchive(b, 1)

	benchColdWarm(b, root, func(b *testing.B, ws *view.Workspace) {
		b.Helper()

		_, err := ws.Runs()
		require.NoError(b, err)

		_, err = ws.AllRunArtifacts()
		require.NoError(b, err)
	})
}

func BenchmarkOrgList(b *testing.B) {
	root := buildBenchArchive(b, benchWorkspaces)

	b.Run("cold", func(b *testing.B) {
		for b.Loop() {
			_, err := benchOrg(b, root).List("")
			require.NoError(b, err)
		}
	})

	b.Run("warm", func(b *testing.B) {
		org := benchOrg(b, root)

		_, err := org.List("")
		require.NoError(b, err)

		for b.Loop() {
			_, err = org.List("")
			require.NoError(b, err)
		}
	})
}
