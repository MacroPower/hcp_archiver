package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// walKind tags a log record with the ledger field it mutates, so a replay
// applies each record to the same in-memory structure [Ledger.Flush] appended
// it from.
type walKind string

const (
	// WalEntry records one object's [Entry] under its archive-relative path.
	walEntry walKind = "entry"
	// WalWatermark records an append-mostly collection's high-water mark.
	walWatermark walKind = "watermark"
	// WalCompleted records that a collection has been walked to its end.
	walCompleted walKind = "completed"
	// WalRun records the run-level metadata (last run time, count, and summary).
	walRun walKind = "run"
)

// walRecord is one line of the append-only log: a single mutation the ledger
// accumulated since the last compaction. Only the fields the [walKind] uses are
// set, so a record stays compact and a replay reconstructs the same state a full
// snapshot would.
type walRecord struct {
	At        time.Time  `json:"at,omitzero"`
	LastRunAt time.Time  `json:"lastRunAt,omitzero"`
	Entry     *Entry     `json:"entry,omitempty"`
	LastRun   *RunRecord `json:"lastRun,omitempty"`
	Kind      walKind    `json:"kind"`
	Path      string     `json:"path,omitempty"`
	Key       string     `json:"key,omitempty"`
	RunCount  int        `json:"runCount,omitempty"`
}

// appendLog appends recs to the log at path as newline-terminated JSON lines,
// creating the file and any missing parent as it goes, and returns the number of
// bytes written.
//
// Each record is one line and the terminating newline is the commit marker: a
// crash mid-append leaves an uncommitted fragment past the final newline that
// the next append trims (see [atomicfile.Append]) and a replay drops, so a torn
// write loses at most the last record rather than corrupting the log. The batch
// is written in one call and flushed to stable storage before returning.
func appendLog(path string, recs []walRecord) (int64, error) {
	var buf bytes.Buffer

	for i := range recs {
		line, err := json.Marshal(&recs[i])
		if err != nil {
			return 0, fmt.Errorf("marshal log record: %w", err)
		}

		buf.Write(line)
		buf.WriteByte('\n')
	}

	_, err := atomicfile.Append(path, buf.Bytes())
	if err != nil {
		return 0, fmt.Errorf("append log: %w", err)
	}

	return int64(buf.Len()), nil
}

// replayLog reads the append-only log at path and returns its records in order,
// or nil when no log exists.
//
// Bytes after the final newline are an incomplete trailing write with no commit
// marker and are dropped; a complete, newline-terminated line that fails to
// parse is genuine corruption and returns [ErrCorruptManifest].
func replayLog(path string) ([]walRecord, error) {
	//nolint:gosec // The log path is derived from the operator-chosen manifest path.
	data, err := os.ReadFile(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read log %q: %w", path, err)
	}

	// Split on the newline commit marker. The final element holds any bytes after
	// the last newline: an incomplete trailing write, dropped by taking all but
	// the last split.
	lines := bytes.Split(data, []byte("\n"))
	lines = lines[:len(lines)-1]

	recs := make([]walRecord, 0, len(lines))

	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var rec walRecord

		err = json.Unmarshal(line, &rec)
		if err != nil {
			return nil, fmt.Errorf("%w: log record: %w", ErrCorruptManifest, err)
		}

		recs = append(recs, rec)
	}

	return recs, nil
}
