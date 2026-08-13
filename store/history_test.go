package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/history"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// historyRecords parses the object's sidecar, oldest record first.
func historyRecords(t *testing.T, s *store.Store, relPath string) []history.Record {
	t.Helper()

	data, err := os.ReadFile(s.AbsPath(s.HistoryPath(relPath)))
	require.NoError(t, err)

	var out []history.Record

	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec history.Record

		require.NoError(t, json.Unmarshal([]byte(line), &rec))

		out = append(out, rec)
	}

	return out
}

// sidecarExists reports whether the object at relPath has a history sidecar.
func sidecarExists(t *testing.T, s *store.Store, relPath string) bool {
	t.Helper()

	present, err := s.Exists(s.HistoryPath(relPath))
	require.NoError(t, err)

	return present
}

func TestStore_HistoryPath(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	assert.Equal(t, "projects/p/workspaces/ws/variables.history.ndjson",
		s.HistoryPath("projects/p/workspaces/ws/variables.json"))
}

func TestStore_WriteJSONBytes_WithHistory(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/ws/variables.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	now := fetchedAt.Add(24 * time.Hour)
	v1 := []byte("{\n  \"v\": 1\n}")
	v2 := []byte("{\n  \"v\": 2\n}")

	t.Run("a changed commit supersedes the outgoing content", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		res, err := s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)
		assert.True(t, res.Changed)

		got, err := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, err)
		assert.Equal(t, v2, got, "the file holds the new content")

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 1)
		assert.Equal(t, string(v1), recs[0].Content, "the sidecar holds the superseded content")
		assert.Equal(t, fetchedAt, recs[0].FetchedAt, "stamped with the prior run's fetch time")
	})

	t.Run("an unchanged commit appends nothing", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		res, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)
		assert.False(t, res.Changed)

		assert.False(t, sidecarExists(t, s, relPath),
			"an object that never changes never grows a sidecar")
	})

	t.Run("a first write creates no sidecar", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, now))
		require.NoError(t, err)

		assert.False(t, sidecarExists(t, s, relPath))
	})

	t.Run("a zero fetchedAt falls back to the file's modification time", func(t *testing.T) {
		t.Parallel()

		// A rebuilt ledger carries no prior FetchedAt; the file itself still
		// knows roughly when its content landed.
		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1)
		require.NoError(t, err)

		mtime := time.Date(2026, time.June, 1, 8, 30, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(s.AbsPath(relPath), mtime, mtime))

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(time.Time{}, now))
		require.NoError(t, err)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 1)
		assert.True(t, mtime.Equal(recs[0].FetchedAt),
			"the superseded record is stamped with the file's mtime")
	})

	t.Run("a zero-length existing file supersedes nothing", func(t *testing.T) {
		t.Parallel()

		// An empty file holds no version, and a record carrying it would lose
		// its content field to omitempty: neither a version nor a tombstone.
		s := store.New(t.TempDir())

		require.NoError(t, os.MkdirAll(filepath.Dir(s.AbsPath(relPath)), 0o700))
		require.NoError(t, os.WriteFile(s.AbsPath(relPath), nil, 0o600))

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)

		assert.False(t, sidecarExists(t, s, relPath))
	})

	t.Run("a history append that cannot land fails the write intact", func(t *testing.T) {
		t.Parallel()

		// A directory squatting on the sidecar path makes the append fail;
		// the object file must keep its old content so no version is lost.
		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(s.AbsPath(s.HistoryPath(relPath)), 0o700))

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.Error(t, err)

		got, readErr := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, readErr)
		assert.Equal(t, v1, got, "the object file keeps the old content")
	})
}

func TestStore_WriteJSONBytes_DefaultKeepsNoHistory(t *testing.T) {
	t.Parallel()

	// Without the option a commit only overwrites: changed content replaces
	// the file and nothing else is written.
	const relPath = "org.json"

	s := store.New(t.TempDir())

	_, err := s.WriteJSONBytes(relPath, []byte(`{"v":1}`))
	require.NoError(t, err)

	res, err := s.WriteJSONBytes(relPath, []byte(`{"v":2}`))
	require.NoError(t, err)
	assert.True(t, res.Changed)

	assert.False(t, sidecarExists(t, s, relPath))
}

func TestStore_BuryHistory(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/ws/tags.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	deletedAt := fetchedAt.Add(24 * time.Hour)
	content := []byte("{\n  \"v\": 1\n}")

	t.Run("buries once per disappearance", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, content)
		require.NoError(t, err)

		buried, err := s.BuryHistory(relPath, fetchedAt, deletedAt)
		require.NoError(t, err)
		assert.True(t, buried)

		// Mutable re-404s every run; the repeated bury must not append.
		buried, err = s.BuryHistory(relPath, fetchedAt, deletedAt.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, buried)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 2)
		assert.Equal(t, string(content), recs[0].Content)
		assert.Equal(t, fetchedAt, recs[0].FetchedAt)
		assert.True(t, recs[1].Deleted)
		assert.Equal(t, deletedAt, recs[1].FetchedAt)

		got, err := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, err)
		assert.Equal(t, content, got, "the last-known file is left in place")
	})

	t.Run("nothing to bury creates no sidecar", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		buried, err := s.BuryHistory(relPath, fetchedAt, deletedAt)
		require.NoError(t, err)
		assert.False(t, buried)

		assert.False(t, sidecarExists(t, s, relPath))
	})

	t.Run("a zero fetchedAt falls back to the file's modification time", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, content)
		require.NoError(t, err)

		mtime := time.Date(2026, time.June, 1, 8, 30, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(s.AbsPath(relPath), mtime, mtime))

		buried, err := s.BuryHistory(relPath, time.Time{}, deletedAt)
		require.NoError(t, err)
		assert.True(t, buried)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 2)
		assert.True(t, mtime.Equal(recs[0].FetchedAt),
			"the flushed content is stamped with the file's mtime")
	})
}
