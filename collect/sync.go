package collect

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// SyncStats tallies one sync pass: a whole-tree [Env.SyncArchive] sweep or a
// seal-boundary [Env.SyncSubtree].
type SyncStats struct {
	// UploadedBytes is the total size of the files uploaded.
	UploadedBytes int64
	// Uploaded counts files uploaded because the remote copy was absent or
	// differed.
	Uploaded int
	// Skipped counts files whose remote copy already matched.
	Skipped int
	// Evicted counts cold surfaces (sealed bundles, settled tarballs) moved
	// remote and removed locally.
	Evicted int
	// Pruned counts stale remote keys deleted because nothing local backs
	// them anymore.
	Pruned int
	// Failed counts failed sweep operations — a file's upload or eviction,
	// the inventory listing, the walk, or a prune batch; each is logged and
	// the next run's sweep retries.
	Failed int
}

// syncCounters is the goroutine-shared accumulator behind [SyncStats]; the
// synced files fan out over an errgroup, so every field is atomic.
type syncCounters struct {
	uploadedBytes atomic.Int64
	uploaded      atomic.Int64
	skipped       atomic.Int64
	evicted       atomic.Int64
	pruned        atomic.Int64
	failed        atomic.Int64
	// The monotonic count of files that reached an outcome (uploaded, skipped,
	// evicted, or failed), driving the progress line: each Add returns a unique
	// value, so a decade marker fires exactly once, unlike a sum of the
	// per-category loads that could step over the interval.
	settled atomic.Int64
}

// stats converts the accumulator into the returned [SyncStats].
func (c *syncCounters) stats() SyncStats {
	return SyncStats{
		UploadedBytes: c.uploadedBytes.Load(),
		Uploaded:      int(c.uploaded.Load()),
		Skipped:       int(c.skipped.Load()),
		Evicted:       int(c.evicted.Load()),
		Pruned:        int(c.pruned.Load()),
		Failed:        int(c.failed.Load()),
	}
}

// Sentinel errors reported by [Env.OffloadFile] when the eviction's verify
// gate refuses the custody transfer.
var (
	// ErrOffloadUnproven indicates a cold surface whose local bytes no longer
	// match the proof that settled them (a bundle's sidecar digests, a
	// tarball's ledger signature); the rotted file is never uploaded and
	// stays local for manual inspection.
	ErrOffloadUnproven = errors.New("offload source does not match its recorded proof")

	// ErrRemoteCopyMismatch indicates a remote object already at an eviction
	// key whose size or recorded digest differs from the proven local bytes;
	// the local file is kept canonical and the remote object needs manual
	// inspection. The mismatch never resolves on its own: the sweep refuses
	// to overwrite remote history at the key, so every run re-reports it.
	ErrRemoteCopyMismatch = errors.New("remote copy differs from the proven local file")
)

// OffloadFile moves the local file at relPath to the remote store: it
// re-proves the local bytes against the record that settled them, uploads
// the file if the store does not already hold its key, re-confirms the
// remote copy, and only then removes the local file.
//
// The gate runs at both ends of the transfer, because after the delete the
// remote copy is the archive's only copy. Before any remote traffic the
// local bytes are verified against their proof — a bundle zip member by
// member against the sidecar digests [seal.Seal] recorded, a
// configuration-version tarball against its ledger signature — so rot that
// crept in after the artifact was proven is refused ([ErrOffloadUnproven])
// rather than enshrined as the long-term record. The upload then records the
// file's digests as object metadata, and the confirm compares size plus
// every digest both sides carry, so a foreign or stale object at the key
// refuses the delete ([ErrRemoteCopyMismatch]); an object without recorded
// digests (an upload by an older build) still gates on size.
//
// Every state is derived from what is observable — the local file, its
// recorded proof, and a metadata probe — so any crash point self-heals on
// the next sweep with no persisted flags: an interrupted upload re-uploads
// (an aborted parted upload is not an object), an upload that finished
// before the local delete is found by the probe, matched digest for digest,
// and evicted without re-uploading, and a finished eviction is a no-op.
func (e *Env) OffloadFile(ctx context.Context, relPath string) error {
	rc := e.remote
	key := e.RemoteKey(relPath)
	absPath := e.store.AbsPath(relPath)

	local, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat offload source: %w", err)
	}

	digests, err := e.proveOffloadSource(relPath, absPath)
	if err != nil {
		return err
	}

	info, err := rc.Head(ctx, key)

	switch {
	case errors.Is(err, remote.ErrNotFound):
		start := time.Now()

		uploadErr := e.uploadFile(ctx, absPath, key, local.Size(), digests)
		if uploadErr != nil {
			return uploadErr
		}

		info, err = rc.Head(ctx, key)
		if err != nil {
			return fmt.Errorf("confirm remote copy: %w", err)
		}

		e.logger.LogAttrs(ctx, slog.LevelInfo, "offload_uploaded",
			slog.String("key", key),
			slog.Int64("bytes", local.Size()),
			slog.Duration("duration", time.Since(start)),
		)

	case err != nil:
		return fmt.Errorf("probe remote copy: %w", err)
	}

	err = confirmRemoteCopy(info, local.Size(), digests, key)
	if err != nil {
		return err
	}

	err = os.Remove(absPath)
	if err != nil {
		return fmt.Errorf("remove evicted file: %w", err)
	}

	e.logger.LogAttrs(ctx, slog.LevelInfo, "offload_evicted",
		slog.String("path", relPath),
		slog.String("key", key),
	)

	return nil
}

// proveOffloadSource re-proves the local bytes at relPath against the record
// that settled them and returns the digests the upload records with the
// remote object. Only the two evictable surfaces carry a proof: a sealed
// bundle re-verifies member by member against its sidecar, and a
// configuration-version tarball re-hashes against its ledger signature.
// Anything else, or a file that fails its proof, reports
// [ErrOffloadUnproven].
func (e *Env) proveOffloadSource(relPath, absPath string) (remote.Digests, error) {
	digests, err := hashFile(absPath)
	if err != nil {
		return remote.Digests{}, fmt.Errorf("hash offload source: %w", err)
	}

	switch {
	case isBundleZip(relPath):
		entries, sidecarErr := seal.ReadSidecar(absPath + seal.SidecarSuffix)
		if sidecarErr != nil {
			return remote.Digests{}, fmt.Errorf("read sidecar proof: %w", sidecarErr)
		}

		if len(entries) == 0 {
			return remote.Digests{}, fmt.Errorf("%w: bundle %q has no sidecar", ErrOffloadUnproven, relPath)
		}

		verifyErr := seal.Verify(absPath, entries)
		if verifyErr != nil {
			return remote.Digests{}, fmt.Errorf("%w: %w", ErrOffloadUnproven, verifyErr)
		}

	case isConfigTarball(relPath):
		entry, ok := e.ledger.Entry(relPath)
		if !ok || entry.Status != manifest.StatusDone || entry.Signature == nil {
			return remote.Digests{}, fmt.Errorf("%w: tarball %q has no settled ledger entry",
				ErrOffloadUnproven, relPath)
		}

		if entry.Signature.Hash != "" && entry.Signature.Hash != digests.SHA256 {
			return remote.Digests{}, fmt.Errorf("%w: tarball %q does not hash to its ledger signature",
				ErrOffloadUnproven, relPath)
		}

	default:
		return remote.Digests{}, fmt.Errorf("%w: %q is not an evictable surface", ErrOffloadUnproven, relPath)
	}

	return digests, nil
}

// confirmRemoteCopy proves the remote object carries the proven local
// content before the local copy is released: the sizes must match, and so
// must every digest both sides carry. A remote object without recorded
// digests (an upload by an older build, a foreign write) still gates on
// size, the strongest egress-free comparison available for it.
func confirmRemoteCopy(info remote.ObjectInfo, size int64, digests remote.Digests, key string) error {
	switch {
	case info.Size != size:
		return fmt.Errorf("%w: %q is %d bytes remote, %d local (needs manual inspection)",
			ErrRemoteCopyMismatch, key, info.Size, size)

	case info.SHA256 != "" && digests.SHA256 != "" && info.SHA256 != digests.SHA256:
		return fmt.Errorf("%w: %q records sha256 %s remote, %s local (needs manual inspection)",
			ErrRemoteCopyMismatch, key, info.SHA256, digests.SHA256)

	case len(info.MD5) > 0 && len(digests.MD5) > 0 && !bytes.Equal(info.MD5, digests.MD5):
		return fmt.Errorf("%w: %q records a different md5 (needs manual inspection)",
			ErrRemoteCopyMismatch, key)
	}

	return nil
}

// hashFile streams the file at absPath through the two digests the remote
// store compares in: the SHA-256 the archive's local proofs carry and the
// MD5 the stores record.
func hashFile(absPath string) (remote.Digests, error) {
	//nolint:gosec // The path is composed by the store from its archive root.
	f, err := os.Open(absPath)
	if err != nil {
		return remote.Digests{}, fmt.Errorf("open: %w", err)
	}

	sha := sha256.New()
	//nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	sum := md5.New()

	_, copyErr := io.Copy(io.MultiWriter(sha, sum), f)
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		return remote.Digests{}, fmt.Errorf("hash: %w", copyErr)
	case closeErr != nil:
		return remote.Digests{}, fmt.Errorf("close: %w", closeErr)
	}

	return remote.Digests{
		SHA256: hex.EncodeToString(sha.Sum(nil)),
		MD5:    sum.Sum(nil),
	}, nil
}

// uploadFile streams one local file to the remote store at key, letting the
// client split a large body into a concurrent parted upload; digests ride
// with the write as its integrity check and recorded metadata.
func (e *Env) uploadFile(ctx context.Context, absPath, key string, size int64, digests remote.Digests) error {
	//nolint:gosec // The path is composed by the store from its archive root.
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open offload source: %w", err)
	}

	uploadErr := e.remote.Upload(ctx, key, f, size, digests)
	closeErr := f.Close()

	switch {
	case uploadErr != nil:
		return fmt.Errorf("upload: %w", uploadErr)
	case closeErr != nil:
		return fmt.Errorf("close after upload: %w", closeErr)
	}

	return nil
}

// syncAction classifies what the sweep does with one walked file.
type syncAction int

const (
	// Leave the file alone: an unverified orphan zip or an unproven
	// tarball, kept local and never uploaded.
	actionSkip syncAction = iota
	// Offload the file and remove it locally: a sealed bundle or a settled
	// configuration-version tarball.
	actionEvict
	// Upload the file if the remote copy is absent or differs, keeping the
	// local copy canonical.
	actionSync
)

// SyncArchive mirrors the organization's archive tree to the remote store; it
// is a no-op without one configured.
//
// It walks every regular file under the store root and classifies it. Sealed
// bundles with their sidecar beside them and settled configuration-version
// tarballs are evicted through [Env.OffloadFile] (the bundle case is a
// backstop for seal-time eviction: a workspace filtered out of later runs, a
// prior failure, a remote newly pointed at an old archive). The ledger flock
// target and the per-shard log.ndjson are never uploaded — a stale remote log
// replayed onto a restored tree could resurrect old ledger state; the
// post-compaction snapshot.json is the durable record. Everything else syncs
// incrementally, gated by one upfront listing inventory: an absent key or a
// size change uploads; a size match compares the local MD5 against the
// store's recorded digest (from the listing, or one Head on backends whose
// listings omit it); with no digest recorded a size match is trusted.
//
// After the uploads, remote keys nothing local backs anymore are pruned, so
// the mirror tracks local deletions and files later sealed into other forms;
// evicted surfaces (bundle zips, configuration-version tarballs) are exempt
// by shape, being remote-only by design. Per-file
// failures are logged and counted, never fatal: local disk stays canonical
// and the next run re-sweeps. A context cancellation stops the sweep early,
// leaving the rest to the next run.
func (e *Env) SyncArchive(ctx context.Context) SyncStats {
	if e.remote == nil {
		return SyncStats{}
	}

	counters := &syncCounters{}
	orgPrefix := e.RemoteKey("") + "/"

	inventory, ok := e.listInventory(ctx, orgPrefix, counters)
	if !ok {
		return counters.stats()
	}

	sweep, err := e.classifyTree(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// As above: a cancellation surfacing from the tree walk is the
			// wind-down, not a sync failure.
			return counters.stats()
		}

		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_walk_error",
			slog.String("error", err.Error()),
		)

		counters.failed.Add(1)

		return counters.stats()
	}

	// Cold surfaces move sequentially: each is one large upload already
	// parallelized inside the transfer manager, and each ends in a local
	// delete, which deserves the simplest possible ordering.
	for _, relPath := range sweep.evict {
		if ctx.Err() != nil {
			return counters.stats()
		}

		evictErr := e.OffloadFile(ctx, relPath)

		switch {
		case evictErr != nil && ctx.Err() != nil:
			// A cancellation surfacing from inside the offload is the wind-down,
			// not an eviction failure: return like the top-of-loop guard, leaving
			// the surface for the next run rather than counting it failed.
			return counters.stats()
		case evictErr != nil:
			e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_evict_error",
				slog.String("path", relPath),
				slog.String("error", evictErr.Error()),
			)
			counters.failed.Add(1)

		default:
			counters.evicted.Add(1)
		}

		e.recordSyncProgress(ctx, counters)
	}

	e.syncFiles(ctx, sweep.sync, inventory, counters)

	if ctx.Err() == nil {
		e.pruneRemote(ctx, orgPrefix, inventory, sweep, counters)
	}

	return counters.stats()
}

// listInventory runs one scoped inventory listing for a sync pass, settling
// the shared failure ladder: a cancellation surfacing from the list is the
// wind-down, reported without logging or counting (matching the passes'
// per-file guards), while any other error warns and counts one failure. The
// second return is false on either early-return path.
func (e *Env) listInventory(
	ctx context.Context,
	prefix string,
	counters *syncCounters,
) (map[string]remote.ObjectInfo, bool) {
	inventory, err := e.remote.List(ctx, prefix)
	if err == nil {
		return inventory, true
	}

	if ctx.Err() == nil {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_inventory_error",
			slog.String("prefix", prefix),
			slog.String("error", err.Error()),
		)

		counters.failed.Add(1)
	}

	return nil, false
}

// treeSweep is one classification pass over the archive tree: the files to
// evict, the files to sync, and the remote key of every file the mirror must
// not prune.
type treeSweep struct {
	keep  map[string]struct{}
	evict []string
	sync  []string
}

// classifyTree walks the store root and sorts every regular file into the
// sweep's evict or sync list, per the classification ladder on
// [Env.SyncArchive]. A skipped-but-eligible file (an orphan zip, an unproven
// tarball) lands in neither list but still marks its key kept, so the prune
// step cannot delete a remote copy out from under a local file the sweep
// declined to touch; staging temps and ledger-internal files mark nothing.
func (e *Env) classifyTree(ctx context.Context) (*treeSweep, error) {
	sweep := &treeSweep{keep: make(map[string]struct{})}

	err := e.walkEligible(ctx, "", func(relPath string) error {
		sweep.keep[e.RemoteKey(relPath)] = struct{}{}

		switch e.classifyFile(ctx, relPath) {
		case actionEvict:
			sweep.evict = append(sweep.evict, relPath)
		case actionSync:
			sweep.sync = append(sweep.sync, relPath)
		case actionSkip:
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk archive tree: %w", err)
	}

	slices.Sort(sweep.evict)
	slices.Sort(sweep.sync)

	return sweep, nil
}

// walkEligible walks the tree under the archive-relative relPrefix (the whole
// archive when empty) and hands each regular, sync-eligible file's
// archive-relative path to fn: the shared skeleton of the sync passes'
// walks, owning the cancellation check, the relativization, and the
// [syncEligible] gate. An error from fn, the walk, or the cancellation
// propagates.
func (e *Env) walkEligible(ctx context.Context, relPrefix string, fn func(relPath string) error) error {
	root := e.store.Root()

	//nolint:wrapcheck // Callers wrap with their pass's context.
	return filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relPrefix)),
		func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case ctx.Err() != nil:
				return ctx.Err()
			case !d.Type().IsRegular():
				return nil
			}

			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return fmt.Errorf("relativize %q: %w", p, relErr)
			}

			relPath := filepath.ToSlash(rel)
			if !syncEligible(relPath) {
				return nil
			}

			return fn(relPath)
		})
}

// syncEligible reports whether a file belongs to the mirror at all. Staging
// temps are partial writes a crash left behind; the ledger's flock target
// carries meaning only as a kernel lock; and the per-shard replay log is
// dangerous to mirror — a stale remote log.ndjson restored over a newer
// snapshot would replay resurrected ledger state. None is uploaded or
// protected from pruning.
func syncEligible(relPath string) bool {
	base := path.Base(relPath)

	if atomicfile.IsTemp(base) {
		return false
	}

	if path.Base(path.Dir(relPath)) == manifest.LedgerDirName {
		return base != manifest.LockFileName && base != manifest.LogFileName
	}

	return true
}

// classifyFile applies the sweep's classification ladder to one eligible file.
func (e *Env) classifyFile(ctx context.Context, relPath string) syncAction {
	if isBundleZip(relPath) {
		// Only a zip with its sidecar beside it is sealed and verified; an
		// orphan from a crash mid-seal is unverified, with its loose sources
		// still canonical, and must never be uploaded.
		if !e.bundleSealed(relPath) {
			return actionSkip
		}

		return actionEvict
	}

	if isConfigTarball(relPath) {
		return e.classifyTarball(ctx, relPath)
	}

	return actionSync
}

// bundleSealed reports whether the zip at relPath is proven by the sidecar
// beside it: seal.Seal writes the sidecar only after the bundle's read-back
// verifies, so its presence is the sealed-and-verified marker. A stat error
// reports unproven.
func (e *Env) bundleSealed(relPath string) bool {
	ok, err := e.store.Exists(relPath + seal.SidecarSuffix)

	return err == nil && ok
}

// classifyTarball gates a configuration-version tarball on its ledger record:
// only a done entry whose recorded size matches the local bytes classifies
// for eviction (the eviction itself then re-proves the bytes against the
// signature's SHA-256; this check is the cheap stat-only screen). An
// unproven one is neither evicted nor synced — a synced copy at the eviction
// key carries the suspect bytes and could later pass a proper eviction's
// confirm, letting it delete the only local copy, and a ledger-declared size
// mismatch means the local file is suspect — so it is skipped with a warning
// and stays local.
func (e *Env) classifyTarball(ctx context.Context, relPath string) syncAction {
	entry, ok := e.ledger.Entry(relPath)
	if ok && entry.Status == manifest.StatusDone && entry.Signature != nil {
		info, err := os.Stat(e.store.AbsPath(relPath))
		if err == nil && info.Size() == entry.Signature.Size {
			return actionEvict
		}
	}

	e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_tarball_unproven",
		slog.String("path", relPath),
	)

	return actionSkip
}

// isBundleZip reports a sealed-bundle zip: a .zip directly under a bundles/
// directory.
func isBundleZip(relPath string) bool {
	return strings.HasSuffix(relPath, ".zip") && path.Base(path.Dir(relPath)) == store.BundlesDirName
}

// isConfigTarball reports an org-wide configuration-version tarball.
func isConfigTarball(relPath string) bool {
	return strings.HasPrefix(relPath, store.ConfigVersionsDirName+"/") && strings.HasSuffix(relPath, ".tar.gz")
}

// underWorkspaceSubtree reports whether relPath lives inside a workspace's
// subtree (projects/<project>/workspaces/<workspace>/...), the scope whose
// mirror converges at the workspace's seal boundary rather than as written.
func underWorkspaceSubtree(relPath string) bool {
	const minSegments = 5 // projects/<p>/workspaces/<ws>/<file>

	segs := strings.Split(relPath, "/")

	return len(segs) >= minSegments && segs[0] == "projects" && segs[2] == "workspaces"
}

// eagerScope reports whether a freshly committed file syncs to the remote the
// moment its content changes. Workspace subtrees are excluded because their
// seal-destined files would be re-shaped minutes later (the subtree syncs at
// its seal boundary instead), and the eviction surfaces are excluded because
// they move by eviction, never by sync.
func eagerScope(relPath string) bool {
	return !underWorkspaceSubtree(relPath) && !isConfigTarball(relPath) && !isBundleZip(relPath)
}

// sealBoundarySkip reports the files [Env.SyncSubtree] leaves to the close
// sweep even though they pass the shared [syncEligible] gate: every file
// under a .ledger/ directory (a mid-run shard snapshot is stale by
// construction; the sweep mirrors the post-compaction one) and every bundle
// zip (an evicted zip is already gone, and an orphan without its sidecar is
// never uploaded).
func sealBoundarySkip(relPath string) bool {
	return path.Base(path.Dir(relPath)) == manifest.LedgerDirName || isBundleZip(relPath)
}

// eagerSync mirrors one just-committed file to the remote store, the
// as-written motion behind the archive primitives: [Env.recordDone] calls it
// after every ledger record, and it uploads only when a remote is configured,
// the commit actually changed the on-disk bytes, and the file is org-scope
// ([eagerScope]). The upload is unconditional — the bytes just changed, so
// the remote copy is stale by definition and no inventory probe is needed.
//
// A semaphore sized [DefaultConcurrency] bounds the burst: the per-page
// archive fan-out is not limited, so without it hundreds of writes landing at
// once would open as many concurrent uploads. A failure warns, counts toward
// [Env.EagerFailures], and defers to the close sweep; it never fails the
// write it rides on. A cancellation settles nothing, matching the sweep's
// wind-down semantics.
func (e *Env) eagerSync(ctx context.Context, relPath string, res store.WriteResult) {
	if e.remote == nil || !res.Changed || !eagerScope(relPath) {
		return
	}

	// An acquire that fails is a cancellation: the wind-down settles nothing,
	// leaving the file to the next run's sweep.
	if e.eagerSem.Acquire(ctx, 1) != nil {
		return
	}

	defer e.eagerSem.Release(1)

	err := e.putFile(ctx, relPath, res.Size)
	if err != nil {
		if ctx.Err() != nil {
			return
		}

		e.logger.LogAttrs(ctx, slog.LevelWarn, "eager_sync_error",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)
		e.eagerFailed.Add(1)
	}
}

// SyncSubtree mirrors the search layer under one archive-relative prefix to
// the remote store, the per-workspace motion at the seal boundary; it is a
// no-op without a remote configured.
//
// It runs one scoped inventory listing and settles each eligible file through
// the same incremental gate as [Env.SyncArchive], so an unchanged file skips
// and a file left stale by an interrupted prior run still uploads. Files
// under a .ledger/ directory are skipped whole — a mid-run shard snapshot is
// stale by construction; the close sweep mirrors the post-compaction one —
// and bundle zips are skipped too (an evicted zip is already gone, and an
// orphan without its sidecar must never be uploaded). Files settle
// sequentially: workspaces seal concurrently, so the workspace goroutines are
// the parallelism, exactly as they are for bundle eviction.
//
// There is no prune — deletion reconciliation stays global at the close
// sweep. Per-file failures warn, count in the returned stats, and add to
// [Env.EagerFailures]; they never abort the subtree or the seal, and the
// close sweep retries them. A cancellation winds down settling nothing.
func (e *Env) SyncSubtree(ctx context.Context, relPrefix string) SyncStats {
	if e.remote == nil {
		return SyncStats{}
	}

	counters := &syncCounters{}
	defer func() {
		e.eagerFailed.Add(counters.failed.Load())
	}()

	prefix := e.RemoteKey(relPrefix) + "/"

	inventory, ok := e.listInventory(ctx, prefix, counters)
	if !ok {
		return counters.stats()
	}

	err := e.walkEligible(ctx, relPrefix, func(relPath string) error {
		if sealBoundarySkip(relPath) {
			return nil
		}

		e.syncFile(ctx, relPath, inventory, counters)

		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A subtree that never materialized has nothing to mirror.
		case ctx.Err() != nil:
			// As above: a cancellation surfacing from the walk is the wind-down.
		default:
			e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_walk_error",
				slog.String("prefix", relPrefix),
				slog.String("error", err.Error()),
			)

			counters.failed.Add(1)
		}
	}

	return counters.stats()
}

// syncFiles settles each search-layer file against the inventory, fanned out
// at the environment's concurrency ceiling. Workers record outcomes in
// counters and never fail the group; a canceled context drains without
// starting new files.
func (e *Env) syncFiles(
	ctx context.Context,
	files []string,
	inventory map[string]remote.ObjectInfo,
	counters *syncCounters,
) {
	var g errgroup.Group

	g.SetLimit(e.Concurrency())

	for _, relPath := range files {
		if ctx.Err() != nil {
			break
		}

		g.Go(func() error {
			// A cancellation mid-fan-out is not a worker failure: the started
			// worker just declines the file, leaving it to the next run.
			//nolint:nilerr // See above: a canceled worker settles nothing.
			if ctx.Err() != nil {
				return nil
			}

			e.syncFile(ctx, relPath, inventory, counters)
			e.recordSyncProgress(ctx, counters)

			return nil
		})
	}

	//nolint:errcheck // Workers never return an error; Wait is the barrier.
	_ = g.Wait()
}

// syncFile settles one file against the remote store: upload when the remote
// copy is absent or differs, skip when it matches.
func (e *Env) syncFile(
	ctx context.Context,
	relPath string,
	inventory map[string]remote.ObjectInfo,
	counters *syncCounters,
) {
	needed, size, err := e.uploadNeeded(ctx, relPath, inventory)
	if err == nil && needed {
		err = e.putFile(ctx, relPath, size)
	}

	switch {
	case err != nil && ctx.Err() != nil:
		// A cancellation surfacing mid-upload is the wind-down, not a per-file
		// failure: the worker settles nothing and leaves the file for the next
		// run, matching the evict loop's in-flight guard.

	case err != nil:
		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_file_error",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)
		counters.failed.Add(1)

	case needed:
		counters.uploaded.Add(1)
		counters.uploadedBytes.Add(size)

	default:
		counters.skipped.Add(1)
	}
}

// putFile uploads one search-layer file: small files ride whole in memory
// through [remote.Client.Put], whose recorded full-object digest is what
// later sweeps' inventories compare, while a file at or past the streaming
// threshold streams from disk so the sweep's memory never scales with the
// archive's largest roll-up.
func (e *Env) putFile(ctx context.Context, relPath string, size int64) error {
	if size >= e.streamThreshold {
		return e.streamFile(ctx, relPath, size)
	}

	//nolint:gosec // The path is composed by the store from its archive root.
	data, err := os.ReadFile(e.store.AbsPath(relPath))
	if err != nil {
		return fmt.Errorf("read sync source: %w", err)
	}

	err = e.remote.Put(ctx, e.RemoteKey(relPath), data)
	if err != nil {
		return fmt.Errorf("put sync source: %w", err)
	}

	return nil
}

// streamFile syncs one large file from disk in two passes: a hash pass
// first, so the write carries the digests the incremental gate compares (a
// parted upload records no backend digest of its own, so the metadata copy
// is what keeps a big roll-up's gate content-aware instead of size-only),
// then a streamed upload whose commit verifies the body against the MD5.
func (e *Env) streamFile(ctx context.Context, relPath string, size int64) error {
	absPath := e.store.AbsPath(relPath)

	digests, err := hashFile(absPath)
	if err != nil {
		return fmt.Errorf("hash sync source: %w", err)
	}

	//nolint:gosec // The path is composed by the store from its archive root.
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open sync source: %w", err)
	}

	uploadErr := e.remote.Upload(ctx, e.RemoteKey(relPath), f, size, digests)
	closeErr := f.Close()

	switch {
	case uploadErr != nil:
		return fmt.Errorf("stream sync source: %w", uploadErr)
	case closeErr != nil:
		return fmt.Errorf("close sync source: %w", closeErr)
	}

	return nil
}

// uploadNeeded is the incremental gate: it reports whether the file's remote
// copy is absent or differs, and the local size. The comparison degrades in
// order — size, then the local MD5 against the digest the inventory records
// when its listing carries one, then one Head for backends whose listings
// omit a digest the object still carries, then size alone when the store
// recorded none at all.
func (e *Env) uploadNeeded(
	ctx context.Context,
	relPath string,
	inventory map[string]remote.ObjectInfo,
) (bool, int64, error) {
	info, err := os.Stat(e.store.AbsPath(relPath))
	if err != nil {
		return false, 0, fmt.Errorf("stat sync source: %w", err)
	}

	listed, ok := inventory[e.RemoteKey(relPath)]
	if !ok || listed.Size != info.Size() {
		return true, info.Size(), nil
	}

	digest := listed.MD5

	if digest == nil {
		remoteInfo, headErr := e.remote.Head(ctx, e.RemoteKey(relPath))

		switch {
		case errors.Is(headErr, remote.ErrNotFound):
			// Listed a moment ago, gone now (a concurrent delete): the upload
			// is needed, not an error.
			return true, info.Size(), nil
		case headErr != nil:
			return false, 0, fmt.Errorf("head sync target: %w", headErr)
		}

		digest = remoteInfo.MD5
	}

	if digest != nil {
		local, hashErr := e.md5File(relPath)
		if hashErr != nil {
			return false, 0, hashErr
		}

		return !bytes.Equal(local, digest), info.Size(), nil
	}

	// No digest is comparable (an object this tool never wrote, on a store
	// that records none): an equal size is the whole gate.
	return false, info.Size(), nil
}

// md5File streams the file at relPath through an MD5 digest, the currency
// the store's recorded content digests compare in.
func (e *Env) md5File(relPath string) ([]byte, error) {
	//nolint:gosec // Content-digest comparison only, not a security control.
	h := md5.New()

	//nolint:gosec // The path is composed by the store from its archive root.
	f, err := os.Open(e.store.AbsPath(relPath))
	if err != nil {
		return nil, fmt.Errorf("open sync source: %w", err)
	}

	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		return nil, fmt.Errorf("hash sync source: %w", copyErr)
	case closeErr != nil:
		return nil, fmt.Errorf("close sync source: %w", closeErr)
	}

	return h.Sum(nil), nil
}

// pruneRemote deletes the inventory keys the sweep saw no local file for, so
// the mirror does not accumulate stale copies of files later sealed into
// other forms (a restored stale loose run.json would shadow its newer roll-up
// line) and a locally deleted subtree is forgotten remotely, consistent with
// the ledger's "deleting .ledger forgets" stance.
//
// Evicted surfaces are exempt by shape (see [evictedSurface]): after
// eviction the remote copy is the only copy, so its survival must not hinge
// on local state. As a guard against a wrong or empty root, nothing is
// pruned when the walk saw no local file at all.
func (e *Env) pruneRemote(
	ctx context.Context,
	orgPrefix string,
	inventory map[string]remote.ObjectInfo,
	sweep *treeSweep,
	counters *syncCounters,
) {
	if len(sweep.keep) == 0 {
		return
	}

	var stale []string

	for key := range inventory {
		if _, kept := sweep.keep[key]; kept {
			continue
		}

		relPath, ok := strings.CutPrefix(key, orgPrefix)
		if !ok || evictedSurface(relPath) {
			continue
		}

		stale = append(stale, key)
	}

	if len(stale) == 0 {
		return
	}

	slices.Sort(stale)

	deleted, err := e.remote.Delete(ctx, stale)

	// Count what was actually removed before crediting failure, so a partial
	// delete (early batches succeed, a later one errors) still reports the keys
	// it pruned rather than zero.
	counters.pruned.Add(int64(deleted))

	if err != nil {
		// A cancellation surfacing mid-prune is the wind-down, not a prune
		// failure: the unfinished keys wait for the next run's sweep, matching
		// the evict and sync loops' in-flight guards.
		if ctx.Err() != nil {
			return
		}

		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_prune_error",
			slog.Int("keys", len(stale)),
			slog.Int("deleted", deleted),
			slog.String("error", err.Error()),
		)
		counters.failed.Add(1)

		return
	}
}

// evictedSurface reports whether a remote-only key has an eviction shape: a
// sealed-bundle zip or a configuration-version tarball. The shape alone
// exempts it from pruning, with no look at the local sidecar or ledger entry
// that proved the eviction — after eviction the remote copy is the only copy,
// and gating its survival on local metadata would let a local loss (a wiped
// .ledger, a deleted workspace subtree taking its sidecars with it) cascade
// into deleting the archive's only bytes. The cost is that a deliberately
// deleted workspace leaves its bundles in the bucket, cleaned up by hand.
func evictedSurface(relPath string) bool {
	return isBundleZip(relPath) || isConfigTarball(relPath)
}

// syncProgressInterval is how many settled files pass between progress lines.
const syncProgressInterval = 1000

// recordSyncProgress counts one settled file and emits a progress line every
// [syncProgressInterval]th one. It is called exactly once per settled file, so
// the monotonic settled counter it advances lands each decade marker exactly
// once. The close sweep runs after the live progress reporter has stopped, so
// slog lines are its only visibility.
func (e *Env) recordSyncProgress(ctx context.Context, counters *syncCounters) {
	n := counters.settled.Add(1)
	if n%syncProgressInterval != 0 {
		return
	}

	e.logger.LogAttrs(ctx, slog.LevelInfo, "sync_progress",
		slog.Int64("files", n),
		slog.Int64("uploaded", counters.uploaded.Load()),
		slog.Int64("skipped", counters.skipped.Load()),
		slog.Int64("evicted", counters.evicted.Load()),
		slog.Int64("failed", counters.failed.Load()),
	)
}
