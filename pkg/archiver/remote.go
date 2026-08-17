package archiver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// ErrRemoteRelocated reports a configured remote that does not match the
// mirror location the archive's marker records. Evicted surfaces live only
// at the recorded location, so a re-point must be an explicit migration, not
// a config edit the run silently follows.
var ErrRemoteRelocated = errors.New("configured remote does not match the archive's recorded mirror")

// RemoteConfig maps the validated configuration surface onto the remote
// client's transport configuration: the one place the two shapes meet, so
// the config package never imports a storage SDK. The inspect commands share
// it to open an archive against the configuration file's remote.
func RemoteConfig(rc *config.RemoteConfig) remote.Config {
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
// down; the next run sweeps instead). A non-nil prog receives the sweep's
// file-settling progress.
//
// A per-file failure never aborts the sweep or the organization, since local
// disk stays canonical and the next run re-sweeps, but the returned tally's
// Failed count marks the run incomplete (see [Archiver.Run]): the mirror is
// the archive's long-term record, and a scheduled run must not report
// success while it is knowingly behind.
func (a *Archiver) syncOrg(
	ctx context.Context,
	env *collect.Env,
	orgName string,
	prog collect.SyncProgress,
) collect.SyncStats {
	if a.remote == nil || ctx.Err() != nil {
		return collect.SyncStats{}
	}

	stats := env.SyncArchive(ctx, collect.WithSyncProgress(prog))

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
// mirrors it eagerly through the environment's as-written motion: the marker
// is what an interrupted run's mirror needs to locate its evicted bundles,
// so it must not wait for the close sweep. A mirror failure warns, counts
// into the eager-failure tally like every other as-written upload, and
// defers to the sweep; only the local side can fail the run.
//
// It runs after [manifest.Load] acquired the organization's cross-process
// flock, since the marker is a mutation of the archive root like any other,
// and writing it outside the single-writer exclusion would let a losing
// concurrent process re-point a marker another run then faithfully mirrors.
// It also refuses a re-pointed remote outright (see checkExistingMarker).
func (a *Archiver) writeRemoteMarker(ctx context.Context, env *collect.Env, st *store.Store) error {
	cfg := RemoteConfig(a.cfg.Remote)
	marker := cfg.Marker()

	existing, hasExisting, err := checkExistingMarker(st.Root(), marker)
	if err != nil {
		return err
	}

	// A partial marker stays partial here: this write happens before anything
	// is collected, so it cannot speak for the tree's completeness. Rewriting
	// a bootstrapped browse cache's marker complete at this point would hide
	// every mirror-only object from later flag-less opens the moment the run
	// is interrupted or filtered. Promotion to complete belongs to the close
	// (see [Archiver.promoteRemoteMarker]), after the sweep has proven the
	// local tree accounts for everything the mirror holds.
	if hasExisting {
		marker.Partial = existing.Partial
	}

	return a.persistMarker(ctx, env, st, marker)
}

// promoteRemoteMarker rewrites a partial marker as complete at the run's
// close. It runs only after a clean close sweep: the sweep mirrors every
// local file and prunes every remote key nothing local backs (counting each
// refusal into its failed tally), so a sweep that settled with zero failures
// proves the mirror holds nothing the local tree and its eviction records do
// not account for, which is exactly what a complete marker asserts. An absent
// or already-complete marker is left alone.
func (a *Archiver) promoteRemoteMarker(ctx context.Context, env *collect.Env, st *store.Store) error {
	existing, ok, err := remote.ReadMarker(st.Root())
	if err != nil || !ok || !existing.Partial {
		return err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	marker := RemoteConfig(a.cfg.Remote).Marker()

	return a.persistMarker(ctx, env, st, marker)
}

// persistMarker writes marker at the organization's archive root and mirrors
// it eagerly through the environment's as-written motion.
func (a *Archiver) persistMarker(ctx context.Context, env *collect.Env, st *store.Store, marker remote.Marker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote marker: %w", err)
	}

	data = append(data, '\n')

	res, err := st.WriteBytes(remote.MarkerName, data)
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	env.EagerSync(ctx, remote.MarkerName, res)

	return nil
}

// checkExistingMarker refuses to overwrite a marker recording a different
// mirror location than the one configured. After eviction the recorded
// bucket and prefix hold the archive's only copies of its cold surfaces, and
// the marker is the only durable pointer to them: silently re-pointing it
// would present the new bucket as a complete mirror it can never become and
// leave `view` unable to reach any evicted bundle. The operator's way out is
// stated in the error: copy the old prefix to the new location, then update
// or delete the marker as consent. The sweep's evicted-surface verification
// still proves the new location actually holds every only-copy before the
// run can exit clean.
//
// A marker that does not exist, or records nothing (a hand-cleared file, the
// deletion consent above), passes; a marker written by a newer build refuses
// per the versioning contract. The existing marker is returned alongside so
// the rewrite can carry its [remote.Marker.Partial] flag forward.
func checkExistingMarker(root string, marker remote.Marker) (remote.Marker, bool, error) {
	existing, ok, err := remote.ReadMarker(root)
	if err != nil || !ok {
		return remote.Marker{}, false, err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	if existing.Conflicts(marker) {
		return remote.Marker{}, false, fmt.Errorf(
			"%w: the archive records its mirror at %q prefix %q, but the configuration names %q prefix %q; "+
				"evicted bundles live only at the recorded location; copy the old prefix to the new "+
				"location, then update or delete %s to consent (the close sweep verifies every evicted "+
				"surface either way)",
			ErrRemoteRelocated, existing.URL, existing.Prefix, marker.URL, marker.Prefix, remote.MarkerName)
	}

	return existing, true, nil
}
