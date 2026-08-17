package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// payload is a plain struct serialized through the encoding/json fallback.
type payload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestStore_AbsPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := store.New(root)

	assert.Equal(t, root, s.Root())
	assert.Equal(t, root+"/teams/t1/team.json", s.AbsPath("teams/t1/team.json"))
}

func TestStore_Exists(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	present, err := s.Exists("org.json")
	require.NoError(t, err)
	assert.False(t, present)

	_, err = s.WriteBytes("org.json", []byte("{}"))
	require.NoError(t, err)

	present, err = s.Exists("org.json")
	require.NoError(t, err)
	assert.True(t, present)
}

func TestStore_WriteJSON_skipsRewriteWhenUnchanged(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())
	rel := s.WorkspaceFile("proj", "ws", "workspace.json")

	first, err := s.WriteJSON(rel, payload{Name: "ws", Count: 1})
	require.NoError(t, err)
	assert.True(t, first.Changed)
	assert.NotZero(t, first.Size)
	assert.NotEmpty(t, first.SHA256)

	fi1, err := os.Stat(s.AbsPath(rel))
	require.NoError(t, err)

	// Identical input: no rewrite, changed=false, signature unchanged.
	second, err := s.WriteJSON(rel, payload{Name: "ws", Count: 1})
	require.NoError(t, err)
	assert.False(t, second.Changed)
	assert.Equal(t, first.SHA256, second.SHA256)
	assert.Equal(t, first.Size, second.Size)

	fi2, err := os.Stat(s.AbsPath(rel))
	require.NoError(t, err)
	assert.True(t, os.SameFile(fi1, fi2), "identical write must not replace the file")

	// Changed input: rewrite, changed=true, new signature and new inode.
	third, err := s.WriteJSON(rel, payload{Name: "ws", Count: 2})
	require.NoError(t, err)
	assert.True(t, third.Changed)
	assert.NotEqual(t, first.SHA256, third.SHA256)

	fi3, err := os.Stat(s.AbsPath(rel))
	require.NoError(t, err)
	assert.False(t, os.SameFile(fi1, fi3), "changed write must replace the file")

	got, err := os.ReadFile(s.AbsPath(rel))
	require.NoError(t, err)
	assert.Contains(t, string(got), `"count": 2`)
}

func TestStore_WriteBytes(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())
	rel := s.Policy("pol-1", "sentinel")
	data := []byte("main = rule { true }\n")

	res, err := s.WriteBytes(rel, data)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, int64(len(data)), res.Size)
	assert.Equal(t, hexSum(data), res.SHA256)

	got, err := os.ReadFile(s.AbsPath(rel))
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestStore_WriteReader_streamsAndSigns(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())
	rel := s.ConfigVersionTarball("cv-1")
	data := bytes.Repeat([]byte("tarball-bytes-"), 5000)

	res, err := s.WriteReader(rel, strings.NewReader(string(data)))
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, int64(len(data)), res.Size)
	assert.Equal(t, hexSum(data), res.SHA256)

	got, err := os.ReadFile(s.AbsPath(rel))
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

// hexSum returns the hex-encoded SHA-256 of data for signature comparison.
func hexSum(data []byte) string {
	h := sha256.Sum256(data)

	return hex.EncodeToString(h[:])
}
