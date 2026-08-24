package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SnapshotMeta is the run-level slice of a shard snapshot, the fields that
// order two copies of the same snapshot in time. Only the org-root shard's
// snapshot records them; every other shard serializes them zero, so a
// non-root snapshot offers nothing to order by.
type SnapshotMeta struct {
	// LastRunAt is the start time of the last run that wrote the snapshot.
	LastRunAt time.Time
	// RunCount is the number of runs the snapshot has accumulated.
	RunCount int
}

// Newer reports whether m is strictly newer than other: a later run start,
// with a run count that has not gone backward. Two snapshots neither of which
// is strictly newer (equal timestamps, or a timestamp and count that
// disagree about direction) are unordered, and a caller deciding which to
// keep must refuse rather than guess.
func (m SnapshotMeta) Newer(other SnapshotMeta) bool {
	return m.LastRunAt.After(other.LastRunAt) && m.RunCount >= other.RunCount
}

// DecodeSnapshotMeta reads the run-level ordering fields from one shard
// snapshot's serialized form. It decodes only the fields it reports, so a
// snapshot written by a newer build still yields its metadata as long as the
// field names hold.
func DecodeSnapshotMeta(r io.Reader) (SnapshotMeta, error) {
	var doc struct {
		LastRunAt time.Time `json:"lastRunAt"`
		RunCount  int       `json:"runCount"`
	}

	err := json.NewDecoder(r).Decode(&doc)
	if err != nil {
		return SnapshotMeta{}, fmt.Errorf("decode snapshot metadata: %w", err)
	}

	return SnapshotMeta{LastRunAt: doc.LastRunAt, RunCount: doc.RunCount}, nil
}
