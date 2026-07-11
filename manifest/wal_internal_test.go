package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplayLogExcludesTornFragment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")

	// Two committed, newline-terminated records followed by a torn trailing
	// fragment with no commit marker, as a crash mid-append would leave.
	committed := []byte(`{"kind":"completed","key":"a"}` + "\n" +
		`{"kind":"completed","key":"b"}` + "\n")
	torn := []byte(`{"kind":"completed","key":"c`)

	require.NoError(t, os.WriteFile(path, append(append([]byte{}, committed...), torn...), 0o600))

	recs, size, err := replayLog(path)
	require.NoError(t, err)
	require.Len(t, recs, 2)

	// The reported size is the committed log, not the raw file: it excludes the
	// torn fragment the next append will truncate.
	require.Equal(t, int64(len(committed)), size)
}
