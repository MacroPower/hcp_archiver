package main_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	main "go.jacobcolvin.com/hcp_archiver/cmd/hcp_archiver"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// Fixture contents and paths for the mini archive, addressed the way the
// commands print them: org-prefixed.
const (
	miniWs = "projects/p1/workspaces/w1"

	miniOrgContent  = `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"mini-org"}}}`
	miniPlanContent = "mini plan output\n"
	miniCVContent   = `{"data":{"id":"cv-1","type":"configuration-versions"}}`

	miniPlanPath = "mini-org/" + miniWs + "/runs/r1/plan.log"
	miniCVPath   = "mini-org/" + miniWs + "/runs/r1/config-version.json"
)

// writeMini writes content at an archive-relative path under root, creating
// parents, and returns the absolute path.
func writeMini(t *testing.T, root, rel, content string) string {
	t.Helper()

	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))

	return abs
}

// buildMiniArchive lays out one organization spanning all three physical
// forms: loose files, one rolled-up member, and one bundled member. It
// returns the archive root.
func buildMiniArchive(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "mini-org")

	writeMini(t, org, "org.json", miniOrgContent)
	writeMini(t, org, "projects/p1/project.json",
		`{"data":{"id":"prj-1","type":"projects","attributes":{"name":"p1"}}}`)
	writeMini(t, org, miniWs+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{"name":"w1"}}}`)
	writeMini(t, org, miniWs+"/runs/r1/run.json",
		`{"data":{"id":"r1","type":"runs","attributes":{"status":"applied"}}}`)

	planAbs := writeMini(t, org, miniWs+"/runs/r1/plan.log", miniPlanContent)
	cvAbs := writeMini(t, org, miniWs+"/runs/r1/config-version.json", miniCVContent)

	_, err := seal.Seal(filepath.Join(org, filepath.FromSlash(miniWs), "bundles", "logs.gen0001.zip"),
		[]seal.Member{{Name: miniWs + "/runs/r1/plan.log", Source: planAbs, Compress: true}})
	require.NoError(t, err)

	err = seal.Rollup(filepath.Join(org, filepath.FromSlash(miniWs), "rollups", "config-versions.ndjson"),
		[]seal.Member{{Name: miniWs + "/runs/r1/config-version.json", Source: cvAbs}})
	require.NoError(t, err)

	return root
}

// runCmd executes the root command with args, returning stdout, stderr, and
// the execution error.
func runCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	cmd := main.NewRootCmd()

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)

	err := cmd.Execute()

	return out.String(), errOut.String(), err
}

func TestListCmd_Text(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	out, _, err := runCmd(t, "list", root)
	require.NoError(t, err)

	// Every path is org-prefixed and each physical form is labeled; the
	// archive's machinery never lists.
	assert.Contains(t, out, "mini-org/org.json")
	assert.Contains(t, out, "loose")
	assert.Contains(t, out, "rollup")
	assert.Contains(t, out, "bundle")
	assert.NotContains(t, out, ".sidecar")
	assert.NotContains(t, out, "gen0001.zip  ")

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		assert.Contains(t, line, "mini-org/", "every listed path is org-prefixed: %q", line)
	}
}

func TestListCmd_PathArg(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	out, _, err := runCmd(t, "list", root, "mini-org/projects")
	require.NoError(t, err)

	assert.NotContains(t, out, "org.json", "the org document sits outside the projects subtree")
	assert.Contains(t, out, miniPlanPath)
}

func TestListCmd_JSON(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	out, _, err := runCmd(t, "list", root, "--json")
	require.NoError(t, err)

	type record struct {
		Path      string `json:"path"`
		Org       string `json:"org"`
		Form      string `json:"form"`
		Container string `json:"container"`
		Modified  string `json:"modified"`
		Size      int64  `json:"size"`
		Offloaded bool   `json:"offloaded"`
	}

	byPath := map[string]record{}

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var rec record

		require.NoError(t, json.Unmarshal([]byte(line), &rec), "each line is one JSON object: %q", line)

		byPath[rec.Path] = rec
	}

	loose := byPath["mini-org/org.json"]
	assert.Equal(t, "mini-org", loose.Org)
	assert.Equal(t, "loose", loose.Form)
	assert.EqualValues(t, len(miniOrgContent), loose.Size)
	assert.NotEmpty(t, loose.Modified, "a loose entry carries its mod time")
	assert.Empty(t, loose.Container)

	bundled := byPath[miniPlanPath]
	assert.Equal(t, "bundle", bundled.Form)
	assert.EqualValues(t, len(miniPlanContent), bundled.Size)
	assert.Equal(t, "mini-org/"+miniWs+"/bundles/logs.gen0001.zip", bundled.Container,
		"the container is org-prefixed so it is addressable with show")
	assert.False(t, bundled.Offloaded)

	rolled := byPath[miniCVPath]
	assert.Equal(t, "rollup", rolled.Form)
	assert.Equal(t, "mini-org/"+miniWs+"/rollups/config-versions.ndjson", rolled.Container)
}

func TestListCmd_OrgDirIsStillPrefixed(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	// Opened on the org directory itself, addresses stay org-prefixed: the
	// same path works no matter which directory the archive was opened on.
	out, _, err := runCmd(t, "list", filepath.Join(root, "mini-org"))
	require.NoError(t, err)
	assert.Contains(t, out, "mini-org/org.json")
}

func TestListCmd_LoneArgHint(t *testing.T) { //nolint:paralleltest // changes the working directory
	root := buildMiniArchive(t)
	t.Chdir(root)

	// From inside the archive, a lone path argument is a likely mistake: the
	// error suggests the two-argument form.
	_, _, err := runCmd(t, "list", "mini-org/projects")
	require.ErrorIs(t, err, view.ErrNotArchive)
	require.ErrorContains(t, err, "a lone argument names the archive directory")
	require.ErrorContains(t, err, "list . mini-org/projects")
}

func TestShowCmd(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
		want string
	}{
		"loose file, exact bytes, no added newline": {
			path: "mini-org/org.json",
			want: miniOrgContent,
		},
		"bundled member": {
			path: miniPlanPath,
			want: miniPlanContent,
		},
		"rolled-up member": {
			path: miniCVPath,
			want: miniCVContent,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildMiniArchive(t)

			out, _, err := runCmd(t, "show", root, tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}

func TestShowCmd_Directory(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	_, _, err := runCmd(t, "show", root, "mini-org/projects")
	require.ErrorIs(t, err, view.ErrNotFile)
	require.ErrorContains(t, err, "list", "the error points at the listing command")
}

func TestShowCmd_UnknownOrg(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	_, _, err := runCmd(t, "show", root, "nope/org.json")
	require.ErrorIs(t, err, view.ErrNoOrg)
	require.ErrorContains(t, err, "mini-org", "the error names the known organizations")
}

func TestShowCmd_RequiresAPath(t *testing.T) {
	t.Parallel()

	_, _, err := runCmd(t, "show")
	require.Error(t, err, "show without a path is a usage error")
}

func TestExtractCmd(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := t.TempDir()

	out, _, err := runCmd(t, "extract", root, "--target", target)
	require.NoError(t, err)
	assert.Contains(t, out, "extracted")
	assert.Contains(t, out, target)

	// Every physical form lands as a plain file with exact bytes.
	got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(miniPlanPath)))
	require.NoError(t, err)
	assert.Equal(t, miniPlanContent, string(got))

	got, err = os.ReadFile(filepath.Join(target, filepath.FromSlash(miniCVPath)))
	require.NoError(t, err)
	assert.JSONEq(t, miniCVContent, string(got))

	got, err = os.ReadFile(filepath.Join(target, "mini-org", "org.json"))
	require.NoError(t, err)
	assert.JSONEq(t, miniOrgContent, string(got))
}

func TestExtractCmd_DryRunMatchesList(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	listOut, _, err := runCmd(t, "list", root, "--json")
	require.NoError(t, err)

	var wantFiles int

	var wantBytes int64

	for line := range strings.SplitSeq(strings.TrimSpace(listOut), "\n") {
		var rec struct {
			Size int64 `json:"size"`
		}

		require.NoError(t, json.Unmarshal([]byte(line), &rec))

		wantFiles++
		wantBytes += rec.Size
	}

	out, _, err := runCmd(t, "extract", root, "--dry-run", "--json")
	require.NoError(t, err)

	var report struct {
		Files  int   `json:"files"`
		Bytes  int64 `json:"bytes"`
		DryRun bool  `json:"dryRun"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.True(t, report.DryRun)
	assert.Equal(t, wantFiles, report.Files, "dry-run totals match the listing")
	assert.Equal(t, wantBytes, report.Bytes)
}

// evictMiniTarball leaves the stub of a configuration-version tarball evicted
// to the remote store. The stub records the length of content, standing in
// for bytes the archive no longer holds locally. It returns the tarball's
// org-prefixed path.
func evictMiniTarball(t *testing.T, root, content string) string {
	t.Helper()

	rel := "config-versions/cv-1.tar.gz"

	writeMini(t, filepath.Join(root, "mini-org"), rel+".remote.json",
		`{"version":1,"size":`+strconv.Itoa(len(content))+`,"sha256":""}`)

	return "mini-org/" + rel
}

// mirrorMiniTarball evicts a configuration-version tarball to a mirror the
// commands can actually reach: the stub stands in for the local file, the org
// root records where the mirror is, and the bytes sit at the mirrored key
// under a file:// bucket, which needs no credentials and no fake. It returns
// the tarball's org-prefixed path.
func mirrorMiniTarball(t *testing.T, root, content string) string {
	t.Helper()

	const (
		rel    = "config-versions/cv-1.tar.gz"
		prefix = "hcp"
	)

	org := filepath.Join(root, "mini-org")
	mirror := t.TempDir()
	sum := sha256.Sum256([]byte(content))

	writeMini(t, org, rel+store.RemoteStubSuffix,
		`{"version":1,"size":`+strconv.Itoa(len(content))+`,"sha256":"`+hex.EncodeToString(sum[:])+`"}`)

	bucket := (&url.URL{Scheme: "file", Path: mirror}).String()
	writeMini(t, org, remote.MarkerName, `{"version":1,"url":"`+bucket+`","prefix":"`+prefix+`"}`)
	writeMini(t, mirror, path.Join(prefix, "mini-org", rel), content)

	return "mini-org/" + rel
}

func TestExtractCmd_FetchesEvictedTarballFromMirror(t *testing.T) {
	t.Parallel()

	const content = "mirrored tarball bytes"

	root := buildMiniArchive(t)
	tarball := mirrorMiniTarball(t, root, content)

	// The dry run predicts a complete recovery, and says what the run will
	// pull down to get there: with no opt-out flag, this is where an operator
	// sees the egress before paying it.
	out, _, err := runCmd(t, "extract", root, "--dry-run", "--json")
	require.NoError(t, err, "an evicted object with a mirror behind it is recoverable")

	type extractReport struct {
		Files       int   `json:"files"`
		Errored     int   `json:"errored"`
		RemoteFiles int   `json:"remoteFiles"`
		RemoteBytes int64 `json:"remoteBytes"`
	}

	var predicted extractReport

	require.NoError(t, json.Unmarshal([]byte(out), &predicted))
	assert.Zero(t, predicted.Errored)
	assert.Equal(t, 1, predicted.RemoteFiles)
	assert.EqualValues(t, len(content), predicted.RemoteBytes)

	text, _, err := runCmd(t, "extract", root, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, text, "to fetch from the remote store")

	// And the run delivers it: the bytes come back from the bucket, byte-exact.
	target := t.TempDir()

	summary, _, err := runCmd(t, "extract", root, "--target", target, "--json")
	require.NoError(t, err)

	var ran extractReport

	require.NoError(t, json.Unmarshal([]byte(summary), &ran))
	assert.Zero(t, ran.Errored)
	assert.Equal(t, predicted.Files, ran.Files, "the dry run predicted the run it described")
	assert.Zero(t, ran.RemoteFiles, "the volume line is a prediction, not a result")

	data, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(tarball)))
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func TestListCmd_EvictedObjectsReadRemote(t *testing.T) {
	t.Parallel()

	// Both evictable surfaces render the same way: what a reader needs to know
	// is that the bytes are not here, not which shape held them.
	root := buildMiniArchive(t)
	tarball := evictMiniTarball(t, root, "tarball bytes")
	require.NoError(t, os.Remove(filepath.Join(root, "mini-org",
		filepath.FromSlash(miniWs), "bundles", "logs.gen0001.zip")))

	out, _, err := runCmd(t, "list", root)
	require.NoError(t, err)

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, tarball) || strings.HasSuffix(line, miniPlanPath) {
			assert.Contains(t, line, "remote", "an evicted object reads remote: %s", line)
		}
	}

	assert.Contains(t, out, tarball, "the evicted tarball is listed at all")
	assert.NotContains(t, out, ".remote.json", "its stub is not")
}

func TestExtractCmd_DryRunCountsRemoteOnlyObjects(t *testing.T) {
	t.Parallel()

	// The dry run predicts the run it plans. It cannot foresee every failure,
	// but a remote-only object is one it can, so it counts it where the real
	// run will and fails the same way.
	root := buildMiniArchive(t)
	evictMiniTarball(t, root, "tarball bytes")

	listOut, _, err := runCmd(t, "list", root, "--json")
	require.NoError(t, err)

	wantEntries := len(strings.Split(strings.TrimSpace(listOut), "\n"))

	out, _, err := runCmd(t, "extract", root, "--dry-run", "--json")
	require.ErrorIs(t, err, main.ErrExtractIncomplete)

	var report struct {
		Files   int  `json:"files"`
		Errored int  `json:"errored"`
		DryRun  bool `json:"dryRun"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.True(t, report.DryRun)
	assert.Equal(t, 1, report.Errored, "the evicted tarball cannot be recovered")
	assert.Equal(t, wantEntries, report.Files+report.Errored,
		"the plan still accounts for every listed object")

	// The real run reaches the same verdict, by trying.
	target := t.TempDir()

	summary, _, err := runCmd(t, "extract", root, "--target", target, "--json")
	require.ErrorIs(t, err, main.ErrExtractIncomplete)

	require.NoError(t, json.Unmarshal([]byte(summary), &report))
	assert.Equal(t, 1, report.Errored)

	// The text summary says what a dry run can honestly say: nothing has been
	// attempted, so nothing has errored yet.
	text, _, err := runCmd(t, "extract", root, "--dry-run")
	require.ErrorIs(t, err, main.ErrExtractIncomplete)
	assert.Contains(t, text, "1 not recoverable")
	assert.NotContains(t, text, "errored")
}

func TestExtractCmd_DryRunCountsOffloadedBundleMembers(t *testing.T) {
	t.Parallel()

	// An evicted bundle in an organization recording no mirror loses its
	// members exactly as an evicted tarball loses itself: the sidecar still
	// lists them, and nothing can fetch the zip their bytes live in. What makes
	// an object unrecoverable is the missing mirror, not the shape that held
	// it, so the dry run counts these where the real run will.
	root := buildMiniArchive(t)
	require.NoError(t, os.Remove(filepath.Join(root, "mini-org",
		filepath.FromSlash(miniWs), "bundles", "logs.gen0001.zip")))

	out, errOut, err := runCmd(t, "extract", root, "--dry-run", "--json")
	require.ErrorIs(t, err, main.ErrExtractIncomplete)

	var predicted struct {
		Errored     int `json:"errored"`
		RemoteFiles int `json:"remoteFiles"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &predicted))
	assert.Equal(t, 1, predicted.Errored, "the bundled member cannot be recovered")
	assert.Zero(t, predicted.RemoteFiles, "and it is not volume the run would fetch either")
	assert.Contains(t, errOut, miniPlanPath, "the object is named where the run streams its failures")

	// The real run reaches the same verdict, by trying.
	target := t.TempDir()

	summary, _, err := runCmd(t, "extract", root, "--target", target, "--json")
	require.ErrorIs(t, err, main.ErrExtractIncomplete)

	var ran struct {
		Errored int `json:"errored"`
	}

	require.NoError(t, json.Unmarshal([]byte(summary), &ran))
	assert.Equal(t, 1, ran.Errored)
}

func TestExtractCmd_DryRunText(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	out, _, err := runCmd(t, "extract", root, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "would extract")
}

func TestExtractCmd_JSONSummary(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := t.TempDir()

	out, _, err := runCmd(t, "extract", root, "--target", target, "--json")
	require.NoError(t, err)

	var report struct {
		Target  string `json:"target"`
		Files   int    `json:"files"`
		Errored int    `json:"errored"`
		DryRun  bool   `json:"dryRun"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, target, report.Target)
	assert.Positive(t, report.Files)
	assert.Zero(t, report.Errored)
	assert.False(t, report.DryRun)
}

func TestExtractCmd_RequiresTarget(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	_, _, err := runCmd(t, "extract", root)
	require.ErrorIs(t, err, view.ErrNoTarget)
}

func TestExtractCmd_RefusesTargetInsideArchive(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	_, _, err := runCmd(t, "extract", root, "--target", filepath.Join(root, "restore"))
	require.ErrorIs(t, err, main.ErrTargetInArchive)

	// A sibling sharing the directory-name prefix is outside.
	sibling := root + "-restore"
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })

	_, _, err = runCmd(t, "extract", root, "--target", sibling)
	require.NoError(t, err)
}

func TestExtractCmd_PerFileFailuresExitNonzero(t *testing.T) {
	t.Parallel()

	// An evicted-looking bundle (zip gone, sidecar kept, no remote) makes its
	// member unrecoverable: the run continues, the summary still prints to
	// stdout, the failure lands on stderr, and the command exits non-zero.
	root := buildMiniArchive(t)
	require.NoError(t, os.Remove(filepath.Join(root, "mini-org",
		filepath.FromSlash(miniWs), "bundles", "logs.gen0001.zip")))

	target := t.TempDir()

	out, stderr, err := runCmd(t, "extract", root, "--target", target)
	require.ErrorIs(t, err, main.ErrExtractIncomplete)
	assert.Contains(t, out, "extracted")
	assert.Contains(t, out, "1 errored")
	assert.Contains(t, stderr, miniPlanPath, "the failed file is reported on stderr")

	// The rest of the archive still recovered.
	got, readErr := os.ReadFile(filepath.Join(target, filepath.FromSlash(miniCVPath)))
	require.NoError(t, readErr)
	assert.JSONEq(t, miniCVContent, string(got))
}

func TestExtractCmd_Verbose(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := t.TempDir()

	_, stderr, err := runCmd(t, "extract", root, "--target", target, "-v")
	require.NoError(t, err)
	assert.Contains(t, stderr, miniPlanPath, "verbose streams one line per file to stderr")
}
