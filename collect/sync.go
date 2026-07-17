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
	"maps"
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
	// The run-wide eviction accounting lives here rather than at the callers
	// (the seal boundary and the close sweep's evict loop), so each eviction
	// counts exactly once whoever drives it. A non-cancel failure counts even
	// before any transfer (a stat error, a refused proof), matching the sweep's
	// per-pass failure semantics; a cancellation is the wind-down and settles
	// nothing.
	err := e.offloadFile(ctx, relPath)
	if err != nil && ctx.Err() == nil {
		e.remoteTally.fail()
	}

	return err
}

// offloadFile is the transfer behind [Env.OffloadFile], separated so the
// wrapper can settle the run-wide tally from its one return.
func (e *Env) offloadFile(ctx context.Context, relPath string) error {
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

	// The run tally credits eviction bytes only when this call actually moved
	// them: a probe that finds a finished prior upload transfers nothing.
	var uploadedBytes int64

	info, err := rc.Head(ctx, key)

	switch {
	case errors.Is(err, remote.ErrNotFound):
		start := time.Now()

		uploadErr := e.uploadFile(ctx, absPath, key, local.Size(), digests)
		if uploadErr != nil {
			return uploadErr
		}

		uploadedBytes = local.Size()

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

	// A remote copy carrying no digest at all leaves nothing egress-free to
	// compare, and after the delete below it is the archive's only copy, so
	// size alone must never release the local bytes. Whether the object is an
	// older build's upload or this tool's own upload whose metadata the store
	// dropped (the two are indistinguishable at the probe), the confirm
	// escalates to the strongest check there is: read the remote bytes back
	// whole and hash them against the local proof. Eviction is rare and the
	// egress is paid once, only in this degenerate case.
	if info.SHA256 == "" && len(info.MD5) == 0 {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "offload_confirm_readback",
			slog.String("key", key),
			slog.String("detail", "the remote copy records no digest; "+
				"reading it back whole to prove its content before the local delete"),
		)

		err = e.confirmRemoteContent(ctx, key, info.Size, digests.SHA256)
		if err != nil {
			return err
		}
	}

	err = os.Remove(absPath)
	if err != nil {
		return fmt.Errorf("remove evicted file: %w", err)
	}

	e.remoteTally.evict(uploadedBytes)

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
// digests passes this egress-free screen on size alone, and the caller then
// escalates it to a full content read-back (see [Env.confirmRemoteContent])
// — size is a screen here, never the custody gate.
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

// confirmRemoteContent proves a digestless remote copy byte for byte: the
// object is streamed back through ranged reads and hashed, and only a
// SHA-256 matching the local proof lets the custody transfer proceed. It is
// the eviction confirm's escalation path when no recorded digest exists to
// compare — the strongest verification available, paid in one read of the
// object.
func (e *Env) confirmRemoteContent(ctx context.Context, key string, size int64, wantSHA256 string) error {
	const chunk = 4 << 20

	h := sha256.New()
	buf := make([]byte, chunk)

	var off int64

	for off < size {
		n, err := e.remote.ReadAt(ctx, key, size, buf, off)
		if n > 0 {
			h.Write(buf[:n])

			off += int64(n)
		}

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("read back remote copy %q: %w", key, err)
		}
	}

	if off != size {
		return fmt.Errorf("%w: %q served %d of %d bytes on read-back (needs manual inspection)",
			ErrRemoteCopyMismatch, key, off, size)
	}

	if hex.EncodeToString(h.Sum(nil)) != wantSHA256 {
		return fmt.Errorf("%w: %q reads back different content than the local proof (needs manual inspection)",
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
	// Leave the file alone: an unverified orphan zip or a tarball with no
	// settled ledger entry, kept local and never uploaded.
	actionSkip syncAction = iota
	// Offload the file and remove it locally: a sealed bundle or a settled
	// configuration-version tarball.
	actionEvict
	// Upload the file if the remote copy is absent or differs, keeping the
	// local copy canonical.
	actionSync
	// Keep the file local like a skip, but count a sweep failure: a done
	// tarball whose local bytes contradict the ledger signature that settled
	// it is corrupt, and a run that exited clean over it would hide an
	// unmirrorable only-copy from the operator forever.
	actionSuspect
)

// SyncArchive mirrors the organization's archive tree to the remote store; it
// is a no-op without one configured.
//
// It walks every regular file under the store root and classifies it. Sealed
// bundles with their sidecar beside them and settled configuration-version
// tarballs are evicted through [Env.OffloadFile] (the bundle case is a
// backstop for seal-time eviction: a workspace filtered out of later runs, a
// prior failure, a remote newly pointed at an old archive). A done tarball
// whose local bytes contradict their ledger signature is suspect: it is
// neither evicted nor synced, and it counts a sweep failure so the run exits
// incomplete rather than clean over a corrupt only-copy. The ledger flock
// target and the ledger's log.ndjson are never uploaded — a stale remote log
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

	sweep, err := e.classifyTree(ctx, counters)
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

	// A suspect cold surface (a done tarball whose local bytes contradict the
	// ledger signature that settled them) stays local and unmirrored; each
	// counts a sweep failure so the run exits incomplete over it, exactly as
	// the eviction gate's refusal of size-preserving rot already does.
	for range sweep.suspect {
		counters.failed.Add(1)
		e.remoteTally.fail()
	}

	// The presence obligations are derived before any of this sweep's
	// evictions run: the inventory above predates them too, so a surface this
	// sweep is about to evict (still local here) must not be demanded of the
	// pre-eviction listing.
	obligations := e.evictedObligations(ctx, sweep, counters)

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
		e.verifyEvicted(ctx, inventory, obligations, counters)
		e.pruneRemote(ctx, orgPrefix, inventory, sweep, counters)
	}

	return counters.stats()
}

// evictedObligations maps each surface whose only copy is already the store
// to the size its local proof records (zero when the proof carries none):
// the bundles a sidecar records as evicted (the zip no longer beside it,
// gathered by the walk) and the configuration-version tarballs the ledger
// records done with no local file. It runs before any of the current sweep's
// evictions, so a surface still local — about to evict against the same
// pre-eviction inventory — is never an obligation.
//
// A tarball whose local presence cannot be determined (a stat fault under
// config-versions/) is unverifiable either way. It joins no obligation — it
// may still be local and about to evict this sweep, and demanding it of the
// pre-eviction inventory would report a false hole — but the fault logs an
// error and counts a sweep failure, so the run exits incomplete instead of
// clean over an only-copy check that never ran.
func (e *Env) evictedObligations(
	ctx context.Context,
	sweep *treeSweep,
	counters *syncCounters,
) map[string]int64 {
	obligations := make(map[string]int64, len(sweep.evictedZips))

	for _, zipRel := range sweep.evictedZips {
		obligations[zipRel] = 0
	}

	done := e.ledger.DoneEntriesUnder(store.ConfigVersionsDirName + "/")

	for _, relPath := range slices.Sorted(maps.Keys(done)) {
		if !isConfigTarball(relPath) {
			continue
		}

		present, err := e.store.Exists(relPath)

		switch {
		case err != nil:
			e.logger.LogAttrs(ctx, slog.LevelError, "evicted_obligation_unverifiable",
				slog.String("path", relPath),
				slog.String("error", err.Error()),
			)
			counters.failed.Add(1)

			continue

		case present:
			// A local copy means the tarball is not remote-only: it evicts
			// this sweep or stays canonical.
			continue
		}

		var size int64

		if sig := done[relPath].Signature; sig != nil {
			size = sig.Size
		}

		obligations[relPath] = size
	}

	return obligations
}

// verifyEvicted proves the store still holds every surface whose only copy
// it is (see [Env.evictedObligations]). Everything else in the mirror
// re-derives from local bytes on the next sweep; these alone cannot, so
// their absence is the one gap that is invisible to a local walk and
// unrepairable once noticed late — a re-pointed remote block, a mis-scoped
// lifecycle rule, or a bucket-side delete would otherwise leave every later
// run reporting a complete mirror over a hole in the archive's long-term
// record. Each missing or size-mismatched object logs an error and counts a
// failure, so the run exits incomplete.
//
// The check is a lookup against the inventory listing already in hand — no
// extra requests — comparing sizes where the ledger recorded one (a sidecar
// records its members' digests, not the zip's, so bundles verify by
// presence).
func (e *Env) verifyEvicted(
	ctx context.Context,
	inventory map[string]remote.ObjectInfo,
	obligations map[string]int64,
	counters *syncCounters,
) {
	for _, relPath := range slices.Sorted(maps.Keys(obligations)) {
		info, ok := inventory[e.RemoteKey(relPath)]

		switch {
		case !ok:
			e.logger.LogAttrs(ctx, slog.LevelError, "evicted_object_missing",
				slog.String("path", relPath),
				slog.String("key", e.RemoteKey(relPath)),
				slog.String("detail", "the store does not hold this surface's only copy; "+
					"if the remote was re-pointed, restore or copy the old prefix"),
			)
			counters.failed.Add(1)

		case obligations[relPath] > 0 && info.Size != obligations[relPath]:
			e.logger.LogAttrs(ctx, slog.LevelError, "evicted_object_size_mismatch",
				slog.String("path", relPath),
				slog.String("key", e.RemoteKey(relPath)),
				slog.Int64("remote_bytes", info.Size),
				slog.Int64("recorded_bytes", obligations[relPath]),
			)
			counters.failed.Add(1)
		}
	}
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
// evict, the files to sync, the remote key of every file the mirror must not
// prune, and the evicted bundles whose remote presence the sweep must
// verify.
type treeSweep struct {
	keep        map[string]struct{}
	evict       []string
	sync        []string
	suspect     []string
	evictedZips []string
}

// classifyTree walks the store root and sorts every regular file into the
// sweep's evict or sync list, per the classification ladder on
// [Env.SyncArchive]. A skipped-but-eligible file (an orphan zip, an unproven
// tarball) lands in neither list but still marks its key kept, so the prune
// step cannot delete a remote copy out from under a local file the sweep
// declined to touch; staging temps and ledger-internal files mark nothing.
//
// A bundle sidecar whose zip is no longer beside it is the local record of a
// finished eviction — the zip's only copy is the store — so the walk also
// collects those zips' paths for the sweep's remote-presence verification.
func (e *Env) classifyTree(ctx context.Context, counters *syncCounters) (*treeSweep, error) {
	sweep := &treeSweep{keep: make(map[string]struct{})}

	err := e.walkEligible(ctx, "", func(relPath string) error {
		sweep.keep[e.RemoteKey(relPath)] = struct{}{}

		if isBundleSidecar(relPath) {
			e.recordEvictedZip(ctx, sweep, relPath, counters)
		}

		switch e.classifyFile(ctx, relPath) {
		case actionEvict:
			sweep.evict = append(sweep.evict, relPath)
		case actionSync:
			sweep.sync = append(sweep.sync, relPath)
		case actionSuspect:
			sweep.suspect = append(sweep.suspect, relPath)
		case actionSkip:
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk archive tree: %w", err)
	}

	slices.Sort(sweep.evict)
	slices.Sort(sweep.sync)
	slices.Sort(sweep.suspect)
	slices.Sort(sweep.evictedZips)

	return sweep, nil
}

// recordEvictedZip notes the zip behind one walked sidecar as a finished
// eviction when the zip is no longer beside it, feeding the sweep's
// remote-presence verification. A stat fault leaves the zip's custody
// unknown — it may be local, it may be remote-only — so the zip joins no
// obligation, but the fault logs an error and counts a sweep failure rather
// than silently dropping the only-copy check for the run.
func (e *Env) recordEvictedZip(
	ctx context.Context,
	sweep *treeSweep,
	relPath string,
	counters *syncCounters,
) {
	zipRel := strings.TrimSuffix(relPath, seal.SidecarSuffix)

	present, err := e.store.Exists(zipRel)

	switch {
	case err != nil:
		e.logger.LogAttrs(ctx, slog.LevelError, "evicted_obligation_unverifiable",
			slog.String("path", zipRel),
			slog.String("error", err.Error()),
		)
		counters.failed.Add(1)

	case !present:
		sweep.evictedZips = append(sweep.evictedZips, zipRel)
	}
}

// walkEligible walks the tree under the archive-relative relPrefix (the whole
// archive when empty) and hands each regular, sync-eligible file's
// archive-relative path to fn: the shared skeleton of the sync passes'
// walks, owning the cancellation check, the relativization, and the
// [syncEligible] gate. Symlinked directories are followed (see
// [Env.walkSymlinked]): a subtree an operator relocated behind a symlink is
// a supported layout the ledger's shard discovery already reads through, so
// the sweep must see the same tree the ledger describes. An error from fn,
// the walk, or the cancellation propagates.
func (e *Env) walkEligible(ctx context.Context, relPrefix string, fn func(relPath string) error) error {
	start := filepath.Join(e.store.Root(), filepath.FromSlash(relPrefix))

	return e.walkEligibleFrom(ctx, start, start, make(map[string]struct{}), fn)
}

// walkEligibleFrom walks the physical directory physical for
// [Env.walkEligible], reporting each file at its logical location under
// logical. The two roots differ only beneath a followed symlink, where the
// walk descends the link's resolved target while every reported path stays
// under the link itself, so the archive-relative paths handed to fn remain
// rooted in the archive tree.
func (e *Env) walkEligibleFrom(
	ctx context.Context,
	physical, logical string,
	followed map[string]struct{},
	fn func(relPath string) error,
) error {
	//nolint:wrapcheck // Callers wrap with their pass's context.
	return filepath.WalkDir(physical, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir():
			return nil
		}

		lp, lpErr := logicalWalkPath(physical, logical, p)
		if lpErr != nil {
			return lpErr
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return e.walkSymlinked(ctx, p, lp, followed, fn)
		}

		if !d.Type().IsRegular() {
			return nil
		}

		return e.visitEligible(lp, fn)
	})
}

// walkSymlinked resolves one symlinked walk entry with [os.Stat], mirroring
// the ledger's shard discovery: a subtree relocated behind a symlink still
// holds ledger entries for every file beneath it, and a walk that skipped
// the link would hand the prune a keep set missing all of their remote keys
// — a mass deletion of live mirror content. A link to a regular file
// settles like the file, a link to a directory is walked through the link
// (each resolved target once, breaking link cycles), a dangling link reads
// as absent, and any other stat fault surfaces so a fault cannot silently
// hide a subtree from the sweep.
func (e *Env) walkSymlinked(
	ctx context.Context,
	physical, logical string,
	followed map[string]struct{},
	fn func(relPath string) error,
) error {
	info, err := os.Stat(physical)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat symlinked entry %q: %w", physical, err)
	case info.Mode().IsRegular():
		return e.visitEligible(logical, fn)
	case !info.IsDir():
		return nil
	}

	resolved, err := filepath.EvalSymlinks(physical)
	if err != nil {
		return fmt.Errorf("resolve symlinked directory %q: %w", physical, err)
	}

	if _, ok := followed[resolved]; ok {
		return nil
	}

	followed[resolved] = struct{}{}

	return e.walkEligibleFrom(ctx, resolved, logical, followed, fn)
}

// logicalWalkPath translates a path reported by a physical walk rooted at
// physical to its logical location under logical; the two roots are equal
// everywhere except beneath a followed symlink.
func logicalWalkPath(physical, logical, p string) (string, error) {
	if physical == logical {
		return p, nil
	}

	rel, err := filepath.Rel(physical, p)
	if err != nil {
		return "", fmt.Errorf("relativize %q: %w", p, err)
	}

	return filepath.Join(logical, rel), nil
}

// visitEligible relativizes one regular file's logical path against the
// archive root and hands it through the [syncEligible] gate to fn.
func (e *Env) visitEligible(logical string, fn func(relPath string) error) error {
	rel, err := filepath.Rel(e.store.Root(), logical)
	if err != nil {
		return fmt.Errorf("relativize %q: %w", logical, err)
	}

	relPath := filepath.ToSlash(rel)
	if !syncEligible(relPath) {
		return nil
	}

	return fn(relPath)
}

// syncEligible reports whether a file belongs to the mirror at all. Staging
// temps are partial writes a crash left behind; the ledger's flock target
// carries meaning only as a kernel lock; and the ledger's replay log is
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
// signature's SHA-256; this check is the cheap stat-only screen). Neither of
// the other outcomes is evicted or synced — a synced copy at the eviction key
// carries unproven bytes and could later pass a proper eviction's confirm,
// letting it delete the only local copy — but they differ in weight: a
// tarball with no settled entry at all skips with a warning, while a done
// entry whose signature the local bytes contradict marks the file suspect
// (see [actionSuspect]) — the ledger's proven record now disagrees with the
// file backing it, the same rot the eviction gate refuses when the size is
// preserved, so it must count against the run instead of exiting clean over
// a corrupt, unmirrorable only-copy.
func (e *Env) classifyTarball(ctx context.Context, relPath string) syncAction {
	entry, ok := e.ledger.Entry(relPath)
	if !ok || entry.Status != manifest.StatusDone || entry.Signature == nil {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_tarball_unproven",
			slog.String("path", relPath),
		)

		return actionSkip
	}

	info, err := os.Stat(e.store.AbsPath(relPath))
	if err != nil {
		e.logger.LogAttrs(ctx, slog.LevelError, "sync_tarball_suspect",
			slog.String("path", relPath),
			slog.Int64("recorded_bytes", entry.Signature.Size),
			slog.String("error", err.Error()),
		)

		return actionSuspect
	}

	if info.Size() == entry.Signature.Size {
		return actionEvict
	}

	e.logger.LogAttrs(ctx, slog.LevelError, "sync_tarball_suspect",
		slog.String("path", relPath),
		slog.Int64("recorded_bytes", entry.Signature.Size),
		slog.Int64("local_bytes", info.Size()),
	)

	return actionSuspect
}

// isBundleZip reports a sealed-bundle zip: a .zip directly under a bundles/
// directory.
func isBundleZip(relPath string) bool {
	return strings.HasSuffix(relPath, ".zip") && path.Base(path.Dir(relPath)) == store.BundlesDirName
}

// isBundleSidecar reports a sealed bundle's sidecar index: the sidecar
// suffix over a bundle-zip stem. After eviction the sidecar is the local
// side's only record of the bundle — the presence obligation the sweep
// verifies against the store, and the proof index the prune must never
// delete out from under a remote-only zip.
func isBundleSidecar(relPath string) bool {
	stem, ok := strings.CutSuffix(relPath, seal.SidecarSuffix)

	return ok && isBundleZip(stem)
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

// EagerSync mirrors one just-committed file to the remote store through the
// as-written motion (see eagerSync): the archiver routes writes that bypass
// the archive primitives — today only the organization's remote marker —
// through it, so their mirror failures warn, count into
// [Env.EagerFailures], and defer to the close sweep exactly like every
// other eager upload.
func (e *Env) EagerSync(ctx context.Context, relPath string, res store.WriteResult) {
	e.eagerSync(ctx, relPath, res)
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
		e.remoteTally.fail()

		return
	}

	e.remoteTally.upload(res.Size)
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

			// A file the wind-down declined settled nothing, so it must not
			// advance the settled counter either — the progress line's counts
			// stay a sum of real outcomes, as the evict loop already keeps them.
			if e.syncFile(ctx, relPath, inventory, counters) {
				e.recordSyncProgress(ctx, counters)
			}

			return nil
		})
	}

	//nolint:errcheck // Workers never return an error; Wait is the barrier.
	_ = g.Wait()
}

// syncFile settles one file against the remote store: upload when the remote
// copy is absent or differs, skip when it matches. It reports whether the
// file reached an outcome — false only on the wind-down path, where nothing
// settled and nothing may count.
func (e *Env) syncFile(
	ctx context.Context,
	relPath string,
	inventory map[string]remote.ObjectInfo,
	counters *syncCounters,
) bool {
	needed, size, err := e.uploadNeeded(ctx, relPath, inventory)
	if err == nil && needed {
		err = e.putFile(ctx, relPath, size)
	}

	switch {
	case err != nil && ctx.Err() != nil:
		// A cancellation surfacing mid-upload is the wind-down, not a per-file
		// failure: the worker settles nothing and leaves the file for the next
		// run, matching the evict loop's in-flight guard.
		return false

	case err != nil:
		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_file_error",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)
		counters.failed.Add(1)
		e.remoteTally.fail()

	case needed:
		counters.uploaded.Add(1)
		counters.uploadedBytes.Add(size)
		e.remoteTally.upload(size)

	default:
		counters.skipped.Add(1)
	}

	return true
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

// pruneGuardFloor is the stale-key count below which the disproportion
// guard stays out of the prune's way: steady-state prunes (a deleted
// workspace, loose files a later seal re-shaped) are small, and a small
// delete set is recoverable from bucket versioning even when wrong, so only
// a mass deletion demands the extra proof.
const pruneGuardFloor = 100

// pruneRemote deletes the inventory keys the sweep saw no local file for, so
// the mirror does not accumulate stale copies of files later sealed into
// other forms (a restored stale loose run.json would shadow its newer roll-up
// line) and a locally deleted subtree is forgotten remotely, consistent with
// the ledger's "deleting .ledger forgets" stance. Each pruned key is logged,
// so a deletion is auditable rather than visible only as a count.
//
// Evicted surfaces are exempt by shape (see [evictedSurface]), and so are
// the bundle sidecars beside them: after eviction the sidecar is the only
// index proving a remote-only zip's members, so a local subtree loss must
// not cascade into deleting the proof for bytes only the bucket still holds.
//
// Two guards refuse pruning, each the fingerprint of a local tree that is
// not the tree the mirror mirrors — where "nothing local backs it" means
// loss, not deletion — and each counts a failure so the run exits
// incomplete rather than reporting a clean mirror it knowingly diverged
// from:
//
//   - a run whose ledger opened empty against a non-empty inventory is a
//     fresh or wrong --output pointed at an existing mirror; restore the
//     prefix (ledger included) before re-rooting an archive, or the
//     mirror-only history would be deleted wholesale. This refuses the
//     whole prune.
//   - a delete set of **ledger-unknown** keys past [pruneGuardFloor] that
//     outnumbers the keys the walk matched means most of the mirror has no
//     local trace at all — loss, or a deliberate mass deletion that must be
//     an explicit act. This refuses only the unknown keys.
//
// Provenance splits the stale set for that second guard: a key whose
// relpath the ledger still holds an entry for is a re-shape artifact, not a
// loss — the archive still owns the object; its loose remote copy went
// stale because a seal coalesced or bundled it (the tool's own self-heal
// after an interrupted run produces thousands of these at once) — so known
// keys prune freely at any scale, and only keys the ledger has never heard
// of (a deleted or lost subtree took its shard too) face the disproportion
// test.
func (e *Env) pruneRemote(
	ctx context.Context,
	orgPrefix string,
	inventory map[string]remote.ObjectInfo,
	sweep *treeSweep,
	counters *syncCounters,
) {
	var (
		staleKnown   []string
		staleUnknown []string
		matched      int
	)

	for key := range inventory {
		if _, kept := sweep.keep[key]; kept {
			matched++

			continue
		}

		relPath, ok := strings.CutPrefix(key, orgPrefix)
		if !ok || evictedSurface(relPath) || isBundleSidecar(relPath) {
			continue
		}

		if _, known := e.ledger.Entry(relPath); known {
			staleKnown = append(staleKnown, key)
		} else {
			staleUnknown = append(staleUnknown, key)
		}
	}

	if len(staleKnown)+len(staleUnknown) == 0 {
		return
	}

	if !e.ledger.Tally().Resumed {
		e.logger.LogAttrs(ctx, slog.LevelError, "sync_prune_refused",
			slog.Int("keys", len(staleKnown)+len(staleUnknown)),
			slog.String("detail", "this run opened an empty ledger against a non-empty mirror; "+
				"a fresh --output must not prune an existing mirror's history — restore the "+
				"remote prefix locally (ledger included) before re-rooting the archive"),
		)
		counters.failed.Add(1)

		return
	}

	stale := staleKnown

	switch {
	case len(staleUnknown) > pruneGuardFloor && len(staleUnknown) > matched:
		e.logger.LogAttrs(ctx, slog.LevelError, "sync_prune_refused",
			slog.Int("keys", len(staleUnknown)),
			slog.Int("matched", matched),
			slog.String("detail", "most of the mirror has no local trace (not even a ledger entry); "+
				"refusing a mass deletion — if the local tree is incomplete, restore it from the "+
				"remote prefix; if this is a deliberate mass deletion, remove the keys from the "+
				"bucket by hand"),
		)
		counters.failed.Add(1)

	default:
		stale = append(stale, staleUnknown...)
	}

	if len(stale) == 0 {
		return
	}

	slices.Sort(stale)

	for _, key := range stale {
		e.logger.LogAttrs(ctx, slog.LevelInfo, "sync_prune_key",
			slog.String("key", key),
		)
	}

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
