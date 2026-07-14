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
// client's configuration — the one place the two shapes meet, so the config
// package never imports the AWS SDK.
func remoteConfig(rc *config.RemoteConfig) remote.Config {
	return remote.Config{
		Bucket:           rc.Bucket,
		Prefix:           rc.Prefix,
		Endpoint:         rc.Endpoint,
		Region:           rc.Region,
		StorageClass:     rc.StorageClass,
		SyncStorageClass: rc.SyncStorageClass,
		PartSize:         rc.PartSize,
		Concurrency:      rc.Concurrency,
		ForcePathStyle:   rc.ForcePathStyle,
		DisableChecksums: rc.DisableChecksums,
	}
}

// syncOrg mirrors the organization's archive tree to the remote store at the
// run's close, logging the sweep's tallies. It is a no-op without a remote or
// when ctx is already canceled (an interrupted run winds down; the next run
// sweeps instead). Sync failures are logged and never affect the run's
// outcome or exit code: local disk stays canonical, matching eviction's
// warning-only stance.
func (a *Archiver) syncOrg(ctx context.Context, env *collect.Env, orgName string) {
	if a.remote == nil || ctx.Err() != nil {
		return
	}

	stats := env.SyncArchive(ctx)

	level := slog.LevelInfo
	if stats.Failed > 0 {
		level = slog.LevelWarn
	}

	a.logger.LogAttrs(ctx, level, "remote_sync_complete",
		slog.String("org", orgName),
		slog.Int("uploaded", stats.Uploaded),
		slog.Int64("uploaded_bytes", stats.UploadedBytes),
		slog.Int("skipped", stats.Skipped),
		slog.Int("evicted", stats.Evicted),
		slog.Int("pruned", stats.Pruned),
		slog.Int("failed", stats.Failed),
	)
}

// writeRemoteMarker records the read-relevant remote settings at the
// organization's archive root, so a later `view` of the archive can reach
// the offloaded bundles without the original configuration file.
func (a *Archiver) writeRemoteMarker(st *store.Store) error {
	marker := remoteConfig(a.cfg.Remote).Marker()

	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote marker: %w", err)
	}

	err = atomicfile.WriteFile(filepath.Join(st.Root(), remote.MarkerName), append(data, '\n'))
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	return nil
}
