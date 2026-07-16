package archiver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// remoteConfig maps the validated configuration surface onto the remote
// client's transport configuration — the one place the two shapes meet, so
// the config package never imports a storage SDK.
func remoteConfig(rc *config.RemoteConfig) remote.Config {
	return remote.Config{
		URL:         rc.URL,
		Prefix:      rc.Prefix,
		PartSize:    rc.PartSize,
		Concurrency: rc.Concurrency,
	}
}

// syncOrg mirrors the organization's archive tree to the remote store at the
// run's close, logging the sweep's tallies and returning them. It is a no-op
// without a remote or when ctx is already canceled (an interrupted run winds
// down; the next run sweeps instead).
//
// A per-file failure never aborts the sweep or the organization — local disk
// stays canonical and the next run re-sweeps — but the returned tally's
// Failed count marks the run incomplete (see [Archiver.Run]): the mirror is
// the archive's long-term record, and a scheduled run must not report
// success while it is knowingly behind.
func (a *Archiver) syncOrg(ctx context.Context, env *collect.Env, orgName string) collect.SyncStats {
	if a.remote == nil || ctx.Err() != nil {
		return collect.SyncStats{}
	}

	stats := env.SyncArchive(ctx)

	level := slog.LevelInfo
	if stats.Failed > 0 {
		level = slog.LevelWarn
	}

	// The eager failures (as-written and seal-boundary uploads that warned
	// and deferred) ride along for visibility only: the sweep just retried
	// them, so its own Failed count remains the sole run-incomplete signal.
	a.logger.LogAttrs(ctx, level, "remote_sync_complete",
		slog.String("org", orgName),
		slog.Int("uploaded", stats.Uploaded),
		slog.Int64("uploaded_bytes", stats.UploadedBytes),
		slog.Int("skipped", stats.Skipped),
		slog.Int("evicted", stats.Evicted),
		slog.Int("pruned", stats.Pruned),
		slog.Int("failed", stats.Failed),
		slog.Int("eager_failed", env.EagerFailures()),
	)

	return stats
}

// writeRemoteMarker records the read-relevant remote settings at the
// organization's archive root, so a later `view` of the archive can reach
// the offloaded bundles without the original configuration file, then
// mirrors it eagerly: the marker is what an interrupted run's mirror needs
// to locate its evicted bundles, so it must not wait for the close sweep. A
// mirror failure warns and defers to the sweep; only the local side can
// fail the run.
func (a *Archiver) writeRemoteMarker(ctx context.Context, st *store.Store, orgName string) error {
	cfg := remoteConfig(a.cfg.Remote)

	data, err := json.MarshalIndent(cfg.Marker(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote marker: %w", err)
	}

	data = append(data, '\n')

	err = atomicfile.WriteFile(filepath.Join(st.Root(), remote.MarkerName), data)
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	err = a.remote.Put(ctx, cfg.Key(orgName, remote.MarkerName), data)
	if err != nil && ctx.Err() == nil {
		a.logger.LogAttrs(ctx, slog.LevelWarn, "remote_marker_sync_error",
			slog.String("org", orgName),
			slog.String("error", err.Error()),
		)
	}

	return nil
}
