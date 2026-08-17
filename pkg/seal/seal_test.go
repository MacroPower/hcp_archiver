package seal_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
)

// writeSource writes data to name under dir and returns its path.
func writeSource(t *testing.T, dir, name string, data []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

// sha256hex returns the lowercase hex SHA-256 of data.
func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// extract reads one member's bytes out of a bundle.
func extract(t *testing.T, bundlePath, name string) []byte {
	t.Helper()

	zr, err := zip.OpenReader(bundlePath)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, zr.Close()) })

	for _, f := range zr.File {
		if f.Name != name {
			continue
		}

		rc, openErr := f.Open()
		require.NoError(t, openErr)

		data, readErr := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, readErr)

		return data
	}

	t.Fatalf("member %q not found in %s", name, bundlePath)

	return nil
}

// readSidecar parses a bundle's sidecar index.
func readSidecar(t *testing.T, path string) []seal.Entry {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })

	var out []seal.Entry

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}

		var e seal.Entry

		require.NoError(t, json.Unmarshal(scanner.Bytes(), &e))

		out = append(out, e)
	}

	require.NoError(t, scanner.Err())

	return out
}

func TestSeal_BundlesAndRemovesSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logData := []byte("plan: 3 to add, 0 to change, 0 to destroy\n")
	stateData := []byte(`{"version":4,"serial":7,"outputs":{}}`)

	planLog := writeSource(t, dir, "plan.log", logData)
	state := writeSource(t, dir, "state.json", stateData)

	// The bundle lives in a directory the seal creates as it commits.
	bundlePath := filepath.Join(dir, "bundles", "logs.gen0001.zip")

	entries, err := seal.Seal(bundlePath, []seal.Member{
		{Name: "runs/r1/plan.log", Source: planLog, Compress: true},
		{Name: "state-versions/s1.tfstate.json", Source: state, Compress: false},
	})
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// The bundle and sidecar exist; the loose sources are gone.
	_, statErr := os.Stat(bundlePath)
	require.NoError(t, statErr)

	_, statErr = os.Stat(bundlePath + ".sidecar.ndjson")
	require.NoError(t, statErr)

	for _, src := range []string{planLog, state} {
		_, statErr = os.Stat(src)
		assert.True(t, os.IsNotExist(statErr), "the sealed source %s is removed", src)
	}

	// The entries record the method, size, and a correct digest per member.
	byName := make(map[string]seal.Entry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}

	assert.Equal(t, "deflate", byName["runs/r1/plan.log"].Method)
	assert.Equal(t, "store", byName["state-versions/s1.tfstate.json"].Method)
	assert.Equal(t, int64(len(stateData)), byName["state-versions/s1.tfstate.json"].Size)
	assert.Equal(t, sha256hex(stateData), byName["state-versions/s1.tfstate.json"].SHA256)

	// Extraction reproduces the original bytes for both methods.
	assert.Equal(t, logData, extract(t, bundlePath, "runs/r1/plan.log"))
	assert.Equal(t, stateData, extract(t, bundlePath, "state-versions/s1.tfstate.json"))
}

func TestSeal_StoredMemberStaysGreppable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	secret := []byte("outputs.db_password = s3cr3t-value")
	state := writeSource(t, dir, "state.json", secret)

	bundlePath := filepath.Join(dir, "state.gen0001.zip")

	_, err := seal.Seal(bundlePath, []seal.Member{
		{Name: "state.json", Source: state, Compress: false},
	})
	require.NoError(t, err)

	//nolint:gosec // Reading a test-controlled bundle path.
	raw, err := os.ReadFile(bundlePath)
	require.NoError(t, err)

	// A stored member's bytes sit uncompressed in the zip, so a raw byte search
	// (grep -a) finds the plaintext without decompression.
	assert.True(t, bytes.Contains(raw, secret), "a stored member stays greppable on disk")
}

func TestSeal_EmptyMembersWritesNothing(t *testing.T) {
	t.Parallel()

	bundlePath := filepath.Join(t.TempDir(), "logs.zip")

	entries, err := seal.Seal(bundlePath, nil)
	require.NoError(t, err)
	assert.Nil(t, entries)

	_, statErr := os.Stat(bundlePath)
	assert.True(t, os.IsNotExist(statErr), "no bundle is written for an empty seal")
}

func TestSeal_RejectsMemberNameEscapingArchive(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name string
	}{
		"parent traversal":    {name: "../../../etc/cron.d/x"},
		"absolute path":       {name: "/etc/passwd"},
		"interior dotdot":     {name: "runs/../../escape"},
		"trailing slash":      {name: "runs/"},
		"bare dotdot":         {name: ".."},
		"empty name":          {name: ""},
		"backslash traversal": {name: `runs\..\..\escape`},
		"backslash separator": {name: `runs\run-1\plan.json`},
		"unc-style prefix":    {name: `\\host\share\x`},
	}

	for tn, tc := range tests {
		t.Run(tn, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			src := writeSource(t, dir, "payload", []byte("data"))

			_, sealErr := seal.Seal(filepath.Join(dir, "bundle.zip"), []seal.Member{
				{Name: tc.name, Source: src},
			})
			require.ErrorIs(t, sealErr, seal.ErrMemberName)

			rollupErr := seal.Rollup(filepath.Join(dir, "rollup.ndjson"), []seal.Member{
				{Name: tc.name, Source: src},
			})
			require.ErrorIs(t, rollupErr, seal.ErrMemberName)
		})
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name string
		err  error
	}{
		"clean relative path":  {name: "projects/default/workspaces/app/run.json"},
		"single segment":       {name: "org.json"},
		"dot-prefixed segment": {name: "projects/.hidden/file"},
		"parent traversal":     {name: "../../../etc/cron.d/x", err: seal.ErrMemberName},
		"absolute path":        {name: "/etc/passwd", err: seal.ErrMemberName},
		"interior dotdot":      {name: "runs/../../escape", err: seal.ErrMemberName},
		"trailing slash":       {name: "runs/", err: seal.ErrMemberName},
		"double slash":         {name: "runs//plan.json", err: seal.ErrMemberName},
		"bare dot":             {name: ".", err: seal.ErrMemberName},
		"bare dotdot":          {name: "..", err: seal.ErrMemberName},
		"empty name":           {name: "", err: seal.ErrMemberName},
		"backslash separator":  {name: `runs\run-1\plan.json`, err: seal.ErrMemberName},
	}

	for tn, tc := range tests {
		t.Run(tn, func(t *testing.T) {
			t.Parallel()

			err := seal.ValidName(tc.name)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestSeal_RejectsDuplicateMemberNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := writeSource(t, dir, "a", []byte("first"))
	b := writeSource(t, dir, "b", []byte("second"))

	_, sealErr := seal.Seal(filepath.Join(dir, "bundle.zip"), []seal.Member{
		{Name: "runs/r/x.log", Source: a, Compress: true},
		{Name: "runs/r/x.log", Source: b, Compress: true},
	})
	require.ErrorIs(t, sealErr, seal.ErrDuplicateMember)

	rollupErr := seal.Rollup(filepath.Join(dir, "rollup.ndjson"), []seal.Member{
		{Name: "runs/r/x.json", Source: a},
		{Name: "runs/r/x.json", Source: b},
	})
	require.ErrorIs(t, rollupErr, seal.ErrDuplicateMember)
}

func TestSeal_SidecarRecordsEveryMember(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := writeSource(t, dir, "a.log", []byte("aaa"))
	b := writeSource(t, dir, "b.log", []byte("bbbb"))

	bundlePath := filepath.Join(dir, "logs.gen0002.zip")

	entries, err := seal.Seal(bundlePath, []seal.Member{
		{Name: "runs/r/a.log", Source: a, Compress: true},
		{Name: "runs/r/b.log", Source: b, Compress: true},
	})
	require.NoError(t, err)

	sidecar := readSidecar(t, bundlePath+".sidecar.ndjson")
	require.Len(t, sidecar, len(entries))

	// The sidecar records exactly what Seal returned, keyed for a search that
	// never opens the zip.
	assert.Equal(t, entries, sidecar)

	for _, e := range sidecar {
		assert.Equal(t, "logs.gen0002.zip", e.Bundle)
	}
}

func TestReadSidecar_RoundTrips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	planLog := writeSource(t, dir, "plan.log", []byte("plan output\n"))
	state := writeSource(t, dir, "state.json", []byte(`{"version":4}`))

	bundlePath := filepath.Join(dir, "bundles", "logs.gen0001.zip")

	entries, err := seal.Seal(bundlePath, []seal.Member{
		{Name: "runs/r1/plan.log", Source: planLog, Compress: true},
		{Name: "state-versions/s1.json", Source: state, Compress: false},
	})
	require.NoError(t, err)

	got, err := seal.ReadSidecar(bundlePath + seal.SidecarSuffix)
	require.NoError(t, err)
	assert.Equal(t, entries, got, "ReadSidecar decodes exactly what Seal wrote")

	// A missing sidecar reads as no entries and no error, so a caller can probe
	// for one without a separate stat.
	absent, err := seal.ReadSidecar(filepath.Join(dir, "nope.sidecar.ndjson"))
	require.NoError(t, err)
	assert.Nil(t, absent)
}

func TestSeal_BestEffortRemovalToleratesUnremovableSource(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the source removal cannot be made to fail")
	}

	dir := t.TempDir()

	// The source sits in a directory the process can search but not write, so
	// removing the file inside it fails with EACCES rather than ENOENT.
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(locked, 0o700))

	src := writeSource(t, locked, "state.json", []byte(`{"version":4}`))
	require.NoError(t, os.Chmod(locked, 0o500))
	t.Cleanup(func() {
		//nolint:errcheck,gosec // Restore so t.TempDir cleanup can remove the tree.
		os.Chmod(locked, 0o700)
	})

	bundlePath := filepath.Join(dir, "bundles", "logs.gen0001.zip")

	entries, err := seal.Seal(bundlePath, []seal.Member{
		{Name: "state-versions/s1.json", Source: src, Compress: false},
	})
	require.NoError(t, err, "a removal failure does not fail the seal")
	require.Len(t, entries, 1)

	// The durable bundle and sidecar exist even though the source could not be
	// removed.
	_, statErr := os.Stat(bundlePath)
	require.NoError(t, statErr)

	_, statErr = os.Stat(bundlePath + seal.SidecarSuffix)
	require.NoError(t, statErr)

	// The unremovable source survives in place, left for the next run to
	// reconcile against the now-committed sidecar.
	_, statErr = os.Stat(src)
	require.NoError(t, statErr, "the source that could not be removed is kept")
}

// rollupRecord mirrors the roll-up line shape for reading back a roll-up in tests.
type rollupRecord struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// readRollup parses a roll-up's NDJSON lines.
func readRollup(t *testing.T, path string) []rollupRecord {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var out []rollupRecord

	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var r rollupRecord

		require.NoError(t, json.Unmarshal(line, &r))

		out = append(out, r)
	}

	return out
}

func TestRollup_CoalescesAndRemovesSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A multi-line indented JSON, the shape the small metadata takes on disk.
	body := []byte("{\n  \"id\": \"run-1\"\n}")
	src := writeSource(t, dir, "run.json", body)
	events := writeSource(t, dir, "events.json", []byte(`[]`))

	rollupPath := filepath.Join(dir, "rollups", "runs.ndjson")

	require.NoError(t, seal.Rollup(rollupPath, []seal.Member{
		{Name: "runs/r1/run.json", Source: src},
		{Name: "runs/r1/events.json", Source: events},
	}))

	// The loose sources are coalesced away.
	for _, s := range []string{src, events} {
		_, statErr := os.Stat(s)
		assert.True(t, os.IsNotExist(statErr), "%s coalesced away", s)
	}

	lines := readRollup(t, rollupPath)
	require.Len(t, lines, 2)

	byPath := make(map[string]rollupRecord, len(lines))
	for _, l := range lines {
		byPath[l.Path] = l
	}

	// The content round-trips byte for byte, digest and all, so an extract
	// reproduces the original multi-line file.
	assert.Equal(t, string(body), byPath["runs/r1/run.json"].Content)
	assert.Equal(t, sha256hex(body), byPath["runs/r1/run.json"].SHA256)
}

func TestRollup_AppendsToExisting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rollupPath := filepath.Join(dir, "runs.ndjson")

	first := writeSource(t, dir, "a.json", []byte(`{"a":1}`))
	require.NoError(t, seal.Rollup(rollupPath, []seal.Member{{Name: "a", Source: first}}))

	second := writeSource(t, dir, "b.json", []byte(`{"b":2}`))
	require.NoError(t, seal.Rollup(rollupPath, []seal.Member{{Name: "b", Source: second}}))

	// A roll-up only grows: the second fold appends after the first.
	lines := readRollup(t, rollupPath)
	require.Len(t, lines, 2)
	assert.Equal(t, "a", lines[0].Path)
	assert.Equal(t, "b", lines[1].Path)
}

func TestRollup_RejectsNonUTF8Content(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rollupPath := filepath.Join(dir, "runs.ndjson")

	// A lone 0xFF is not valid UTF-8; a JSON string cannot carry it byte for byte,
	// so the roll-up must refuse it rather than mangle it into U+FFFD and desync
	// the stored content from its recorded digest.
	src := writeSource(t, dir, "bad.bin", []byte{0x68, 0x69, 0xff})

	err := seal.Rollup(rollupPath, []seal.Member{{Name: "bad", Source: src}})
	require.ErrorIs(t, err, seal.ErrContentNotUTF8)

	// Nothing is written and the source is left in place: the refusal is total.
	_, statErr := os.Stat(rollupPath)
	assert.True(t, os.IsNotExist(statErr), "no roll-up is written")

	_, srcErr := os.Stat(src)
	require.NoError(t, srcErr, "the source is left in place")
}

func TestRollup_EmptyWritesNothing(t *testing.T) {
	t.Parallel()

	rollupPath := filepath.Join(t.TempDir(), "runs.ndjson")

	require.NoError(t, seal.Rollup(rollupPath, nil))

	_, statErr := os.Stat(rollupPath)
	assert.True(t, os.IsNotExist(statErr), "no roll-up is written for an empty fold")
}

func TestRollup_BestEffortRemovalToleratesUnremovableSource(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the source removal cannot be made to fail")
	}

	dir := t.TempDir()

	// The source sits in a directory the process can search but not write, so
	// removing the file inside it fails with EACCES rather than ENOENT.
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(locked, 0o700))

	src := writeSource(t, locked, "run.json", []byte(`{"id":"run-1"}`))
	require.NoError(t, os.Chmod(locked, 0o500))
	t.Cleanup(func() {
		//nolint:errcheck,gosec // Restore so t.TempDir cleanup can remove the tree.
		os.Chmod(locked, 0o700)
	})

	rollupPath := filepath.Join(dir, "rollups", "runs.ndjson")

	require.NoError(t, seal.Rollup(rollupPath, []seal.Member{
		{Name: "runs/r1/run.json", Source: src},
	}), "a removal failure does not fail the roll-up")

	// The durable roll-up carries the member even though its source could not be
	// removed.
	lines := readRollup(t, rollupPath)
	require.Len(t, lines, 1)
	assert.Equal(t, "runs/r1/run.json", lines[0].Path)

	// The unremovable source survives in place, left for the next run to re-fold
	// against the now-committed roll-up.
	_, statErr := os.Stat(src)
	require.NoError(t, statErr, "the source that could not be removed is kept")
}
