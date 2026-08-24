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

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/fsid"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
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
	// Failed counts failed sweep operations: a file's upload or eviction, the
	// inventory listing, the walk, or a prune batch. Each is logged and the
	// next run's sweep retries.
	Failed int
}

// SyncProgress receives the close sweep's file-settling progress: the total
// files to settle once the tree is classified, then one advance per settled
// file. The archiver's progress reporter satisfies it, so the sweep drives the
// live phase bar without this package importing the progress machinery. The
// bar covers file settling only: the verify and prune passes run after the
// last advance, so the bar sits full (the spinner still live) through that
// tail, and the total stays decoupled from the prune's size on purpose. Only
// the whole-tree close sweep drives the hook: the eager and seal-boundary
// syncs run mid-phase and must not move it.
type SyncProgress interface {
	// SetTotal seeds the denominator: how many files the sweep will settle.
	SetTotal(total int)
	// Advance moves the bar by n settled files.
	Advance(n int)
}

// SyncOption configures one [Env.SyncArchive] sweep.
//
// The available options are:
//   - [WithSyncProgress]
type SyncOption func(*syncCounters)

// WithSyncProgress sets the [SyncProgress] hook the sweep reports its
// file-settling progress through. A nil hook is ignored: the sweep reports
// only through its slog progress lines, the default. It returns a
// [SyncOption].
func WithSyncProgress(p SyncProgress) SyncOption {
	return func(c *syncCounters) {
		c.progress = p
	}
}

// syncCounters is the goroutine-shared accumulator behind [SyncStats]; the
// synced files fan out over an errgroup, so every counter is atomic. It also
// carries the pass's configuration: the optional [SyncProgress] hook.
type syncCounters struct {
	// The close sweep's progress hook, nil for every other pass; every call
	// through it guards against the nil.
	progress      SyncProgress
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

// Sentinel errors the custody gates report when they refuse a transfer: the
// eviction's verify gate, whose refusals [Env.OffloadFile] returns, and the
// rename heal's confirm.
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

	// The rename heal reports errHealUnconfirmable for a copy nothing
	// available can prove: neither object records a digest to compare and the
	// surface's record carries no signature to read it back against. Reporting
	// it leaves the historical copy in place, so a heal never releases an
	// only-copy on size alone. It stays unexported because no caller receives
	// it: the heal logs its own failures and counts nothing (see
	// [Env.healHistoricalKey]).
	errHealUnconfirmable = errors.New("healed copy cannot be proven")
)

// OffloadFile moves the local file at relPath to the remote store: it
// re-proves the local bytes against the record that settled them, uploads
// the file if the store does not already hold its key, re-confirms the
// remote copy, leaves the trace that outlives the file (see
// [Env.writeRemoteStub]), and only then removes the local file.
//
// The gate runs at both ends of the transfer, because after the delete the
// remote copy is the archive's only copy. Before any remote traffic the
// local bytes are verified against their proof (a bundle zip member by member
// against the sidecar digests [seal.Seal] recorded, a configuration-version
// tarball against its ledger signature), so rot that crept in after the
// artifact was proven is refused ([ErrOffloadUnproven]) rather than enshrined
// as the long-term record. The upload then records the file's digests as
// object metadata, and the confirm compares size plus every digest both sides
// carry, so a foreign or stale object at the key refuses the delete
// ([ErrRemoteCopyMismatch]); an object without recorded digests (an upload by
// an older build) still gates on size.
//
// Every state is derived from what is observable (the local file, its
// recorded proof, and a metadata probe), so any crash point self-heals on
// the next sweep with no persisted flags: an interrupted upload re-uploads
// (an aborted parted upload is not an object), an upload that finished
// before the local delete is found by the probe, matched digest for digest,
// and evicted without re-uploading, a stub written but not yet followed by
// its delete leaves the file local and canonical for the next sweep to evict
// and overwrite the stub, and a finished eviction is a no-op.
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

	err = e.writeRemoteStub(relPath, local.Size(), digests.SHA256)
	if err != nil {
		return err
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

// writeRemoteStub leaves the trace that outlives an evicted
// configuration-version tarball, recording the size and digest the readers
// need to list the object without the ledger or the network (see
// [store.RemoteStub]). A sealed bundle is skipped: its sidecar index already
// survives the zip and says the same thing.
//
// It runs before the local delete, not after, because the file is the only
// thing that leads a later sweep back to this path: a crash between the two
// leaves a stub beside a live tarball, which the next sweep re-evicts and
// rewrites, while a delete-first crash would leave a path nothing ever walks
// again. A write fault under config-versions/ (a permission problem, a
// full disk) therefore aborts the eviction and counts a sweep failure, which
// is the right trade on this side of a delete and the deliberate opposite of
// [Env.backfillRemoteStubs], which repairs history rather than making it.
func (e *Env) writeRemoteStub(relPath string, size int64, digest string) error {
	if !isConfigTarball(relPath) {
		return nil
	}

	stub := store.RemoteStub{
		Version: store.RemoteStubVersion,
		Size:    size,
		SHA256:  digest,
	}

	_, err := e.store.WriteJSON(store.RemoteStubPath(relPath), stub)
	if err != nil {
		return fmt.Errorf("write eviction stub: %w", err)
	}

	return nil
}

// backfillRemoteStubs writes the eviction stub for every remote-only tarball
// that has no stub file, so a tarball evicted without one gains its read-model
// trace on the next sweep rather than staying invisible.
//
// The obligations map supplies the set to repair: it already names every
// configuration-version tarball the ledger records done with no local file. It
// also carries the sweep's evicted bundle zips, which have their sidecars for
// a trace and are filtered out.
//
// The signature that settled each tarball supplies the stub's contents, and a
// tarball whose record carries none is left alone, with a warning naming the
// gap. Its size is unknowable here, and a stub claiming zero bytes would put a
// size in the listing that no gzip tarball has ever had, the same lie the read
// side's own guards exist to refuse.
//
// A failure here logs and counts nothing. No bytes are at risk: the mirror
// holds the tarball and the ledger still proves it, so only the listing stays
// short, which is a read-model gap rather than a custody one, and the next
// sweep retries.
//
// It runs before the sweep verifies those same obligations against the store,
// so a tarball the verify is about to find missing still gains a stub saying
// its bytes are mirrored. That is the right order: the verify counts its own
// failure and the run exits incomplete either way, while repairing only after
// a clean verify would let one transient fault withhold the trace forever.
func (e *Env) backfillRemoteStubs(ctx context.Context, obligations map[string]int64) {
	for _, relPath := range slices.Sorted(maps.Keys(obligations)) {
		if !isConfigTarball(relPath) {
			continue
		}

		present, err := e.store.Exists(store.RemoteStubPath(relPath))

		switch {
		case err != nil:
			// A probe that cannot answer leaves the repair undone; saying so is
			// the only way a permanently inert backfill (a permission problem
			// over the whole directory) is ever noticed.
			e.logRemoteStubGap(ctx, relPath, err.Error())

			continue

		case present:
			continue
		}

		entry, ok := e.ledger.Entry(relPath)
		if !ok || entry.Signature == nil {
			e.logRemoteStubGap(ctx, relPath, "ledger record carries no signature to size a stub from")

			continue
		}

		err = e.writeRemoteStub(relPath, entry.Signature.Size, entry.Signature.Hash)
		if err != nil {
			e.logRemoteStubGap(ctx, relPath, err.Error())
		}
	}
}

// logRemoteStubGap reports a repair the backfill could not make, leaving one
// remote-only tarball absent from listings.
func (e *Env) logRemoteStubGap(ctx context.Context, relPath, reason string) {
	e.logger.LogAttrs(ctx, slog.LevelWarn, "remote_stub_backfill_error",
		slog.String("path", relPath),
		slog.String("reason", reason),
	)
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

		// The record that proves the eviction must itself survive a crash
		// before the local bytes may be destroyed (see [Ledger.EntryDurable]);
		// the classify pass already screens for this, so tripping here means a
		// caller reached the delete site around it.
		if !e.ledger.EntryDurable(relPath) {
			return remote.Digests{}, fmt.Errorf("%w: tarball %q done record is not yet durable",
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
// escalates it to a full content read-back (see [Env.confirmRemoteContent]);
// size is a screen here, never the custody gate.
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
// object is streamed back into a hasher, and only a SHA-256 matching the local
// proof lets the custody transfer proceed. It is the eviction confirm's
// escalation path when no recorded digest exists to compare, the strongest
// verification available and paid in one read of the object.
//
// The stream comes back through [remote.Client.Download], whose resume is what
// keeps this hash honest by delivering every byte exactly once. A transient
// blip mid-read-back then costs a re-request of the remainder rather than
// hashing a duplicated prefix and reporting a healthy object as a mismatch,
// which would block its eviction permanently.
func (e *Env) confirmRemoteContent(ctx context.Context, key string, size int64, wantSHA256 string) error {
	h := sha256.New()

	n, err := e.remote.Download(ctx, key, size, h)
	if err != nil {
		return fmt.Errorf("read back remote copy %q: %w", key, err)
	}

	if n != size {
		return fmt.Errorf("%w: %q served %d of %d bytes on read-back (needs manual inspection)",
			ErrRemoteCopyMismatch, key, n, size)
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
// target and the ledger's log.ndjson are never uploaded: a stale remote log
// replayed onto a restored tree could resurrect old ledger state, and the
// post-compaction snapshot.json is the durable record. Nor is an eviction
// stub, which points at the mirror from outside it and would be wrong the
// moment a restore brought its object back. Everything else syncs
// incrementally, gated by one upfront listing inventory: an absent key or a
// size change uploads; a size match compares the local MD5 against the
// store's recorded digest (from the listing, or one Head on backends whose
// listings omit it); with no digest recorded a size match is trusted.
//
// After the uploads, any remote-only tarball missing its stub is given one
// from the ledger (see [Env.backfillRemoteStubs]), which repairs the read
// model without counting against the run, and remote keys nothing local backs
// anymore are pruned, so the mirror tracks local deletions and files later
// sealed into other forms; evicted surfaces (bundle zips,
// configuration-version tarballs) are exempt by shape, being remote-only by
// design. Per-file failures are logged and counted, never fatal: local disk
// stays canonical and the next run re-sweeps. A context cancellation stops the
// sweep early, leaving the rest to the next run. A [WithSyncProgress] hook
// receives the settle total once the tree is classified and one advance per
// settled file.
func (e *Env) SyncArchive(ctx context.Context, opts ...SyncOption) SyncStats {
	if e.remote == nil {
		return SyncStats{}
	}

	counters := &syncCounters{}
	for _, opt := range opts {
		opt(counters)
	}

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

	// The settle total is exactly the files the evict and sync passes drive
	// through recordSyncProgress; suspect files never settle, so they stay out
	// of the denominator as they stay out of the advances.
	if counters.progress != nil {
		counters.progress.SetTotal(len(sweep.evict) + len(sweep.sync))
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
		e.backfillRemoteStubs(ctx, obligations)
		e.verifyEvicted(ctx, inventory, obligations, sweep.aliases, counters)
		e.pruneRemote(ctx, orgPrefix, inventory, sweep, counters)
	}

	return counters.stats()
}

// evictedObligations maps each surface whose only copy is already the store
// to the size its local proof records (zero when the proof carries none):
// the bundles a sidecar records as evicted (the zip no longer beside it,
// gathered by the walk) and the configuration-version tarballs the ledger
// records done with no local file. It runs before any of the current sweep's
// evictions, so a surface still local (about to evict against the same
// pre-eviction inventory) is never an obligation.
//
// A tarball whose local presence cannot be determined (a stat fault under
// config-versions/) is unverifiable either way. It joins no obligation, since
// it may still be local and about to evict this sweep and demanding it of the
// pre-eviction inventory would report a false hole, but the fault logs an
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
// unrepairable once noticed late: a re-pointed remote block, a mis-scoped
// lifecycle rule, or a bucket-side delete would otherwise leave every later
// run reporting a complete mirror over a hole in the archive's long-term
// record. Each missing or size-mismatched object logs an error and counts a
// failure, so the run exits incomplete.
//
// The check is a lookup against the inventory listing already in hand, with no
// extra requests, comparing sizes where the ledger recorded one (a sidecar
// records its members' digests, not the zip's, so bundles verify by
// presence).
//
// An eviction that ran before a workspace rename uploaded its zip under the
// name then in force, while the sidecar now walks under the physical name;
// the sweep's rename aliases bridge the two, so a miss at the current key
// retries under each alias-rewritten key before it is declared a hole. A
// credit at a historical key is then healed onto the current name (see
// [Env.healHistoricalKey]), so the mirror converges instead of depending on
// the rename link forever.
func (e *Env) verifyEvicted(
	ctx context.Context,
	inventory map[string]remote.ObjectInfo,
	obligations map[string]int64,
	aliases map[string]string,
	counters *syncCounters,
) {
	for _, relPath := range slices.Sorted(maps.Keys(obligations)) {
		var histKey string

		info, ok := inventory[e.RemoteKey(relPath)]
		if !ok {
			info, histKey, ok = e.aliasedInventoryLookup(ctx, inventory, aliases, relPath)
		}

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

		case histKey != "":
			e.healHistoricalKey(ctx, histKey, relPath)

		default:
			e.releaseHistoricalKeys(ctx, inventory, aliases, relPath)
		}
	}
}

// releaseHistoricalKeys retries the cleanup half of a heal whose delete
// failed on an earlier sweep: the obligation now hits its current key
// directly, which would otherwise skip the alias path forever and strand the
// historical copy as an unpruneable duplicate (its eviction shape exempts it
// from the prune by design). The satisfied current-key copy is the proof
// that makes each release safe; a failed delete warns and retries next
// sweep, for as long as the rename links that spell the historical keys
// survive.
func (e *Env) releaseHistoricalKeys(
	ctx context.Context,
	inventory map[string]remote.ObjectInfo,
	aliases map[string]string,
	relPath string,
) {
	for _, candidate := range aliasRewrites(aliases, relPath) {
		key := e.RemoteKey(candidate)

		if _, leftover := inventory[key]; !leftover {
			continue
		}

		_, err := e.remote.Delete(ctx, []string{key})
		if err != nil {
			if ctx.Err() == nil {
				e.logger.LogAttrs(ctx, slog.LevelWarn, "evicted_object_heal_error",
					slog.String("path", relPath),
					slog.String("key", key),
					slog.String("error", err.Error()),
				)
			}

			continue
		}

		e.logger.LogAttrs(ctx, slog.LevelInfo, "evicted_object_historical_released",
			slog.String("path", relPath),
			slog.String("key", key),
		)
	}
}

// aliasedInventoryLookup retries one obligation's inventory lookup under
// every rename-alias rewriting of its path: an obligation walked at
// owner-name relPath may have been uploaded under the alias name in force
// when the eviction ran. A hit logs and returns the historical key, so the
// credit is visible in the run's record and the heal knows its source.
func (e *Env) aliasedInventoryLookup(
	ctx context.Context,
	inventory map[string]remote.ObjectInfo,
	aliases map[string]string,
	relPath string,
) (remote.ObjectInfo, string, bool) {
	for _, candidate := range aliasRewrites(aliases, relPath) {
		key := e.RemoteKey(candidate)

		info, hit := inventory[key]
		if !hit {
			continue
		}

		e.logger.LogAttrs(ctx, slog.LevelInfo, "evicted_object_at_historical_key",
			slog.String("path", relPath),
			slog.String("key", key),
		)

		return info, key, true
	}

	return remote.ObjectInfo{}, "", false
}

// aliasRewrites returns every historical spelling of relPath reachable by
// rewriting owner prefixes back to their alias names, in deterministic
// breadth-first order, excluding relPath itself. The rewrites compose:
// stacked renames (a project rename above a workspace rename) minted keys
// only the combination of both old names spells, which no single
// substitution reaches.
//
// One round per alias bounds the walk, which is the semantic bound as much as
// the safe one: no genuine historical key spells the same rename alias twice,
// so a rename stacked over N aliases is reachable within N rounds. The seen set
// cannot do it alone. An alias nested beneath its own owner ([fsid.WalkFiles]
// records that pair for a link to its own ancestor, a shape its contract
// supports) re-exposes the owner prefix in every substitution's result, minting
// a strictly longer, never-before-seen candidate each pass and never repeating.
func aliasRewrites(aliases map[string]string, relPath string) []string {
	sortedAliases := slices.Sorted(maps.Keys(aliases))
	seen := map[string]struct{}{relPath: {}}
	queue := []string{relPath}

	var out []string

	for round := 0; round < len(sortedAliases) && len(queue) > 0; round++ {
		level := queue
		queue = nil

		for _, cur := range level {
			for _, alias := range sortedAliases {
				rest, ok := strings.CutPrefix(cur, aliases[alias]+"/")
				if !ok {
					continue
				}

				candidate := alias + "/" + rest
				if _, dup := seen[candidate]; dup {
					continue
				}

				seen[candidate] = struct{}{}
				out = append(out, candidate)
				queue = append(queue, candidate)
			}
		}
	}

	return out
}

// healHistoricalKey converges a credited only-copy onto its current name: a
// copy from the historical key, proven to carry the credited content (see
// [Env.healCopy]), and only then the historical key's delete. Until the copy
// lands, the credit depends on the rename link surviving on disk (an operator
// tidying the stale link away would turn an intact archive into a permanent
// false hole), so the heal trades that standing dependency for one converged
// object. Every step fails safe: a fault leaves the historical copy in place,
// logs, and counts nothing, since the credit already succeeded. A failed copy
// retries here next sweep; a failed delete retries through
// [Env.releaseHistoricalKeys], where the next sweep's direct hit at the
// current key proves the release safe.
func (e *Env) healHistoricalKey(ctx context.Context, histKey, relPath string) {
	dst := e.RemoteKey(relPath)

	err := e.healCopy(ctx, histKey, dst, relPath)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.LogAttrs(ctx, slog.LevelWarn, "evicted_object_heal_error",
				slog.String("path", relPath),
				slog.String("key", histKey),
				slog.String("error", err.Error()),
			)
		}

		return
	}

	// The historical copy is deleted only behind the confirmed fresh one, the
	// same confirm-then-release order every custody transfer here follows.
	_, err = e.remote.Delete(ctx, []string{histKey})
	if err != nil {
		if ctx.Err() == nil {
			e.logger.LogAttrs(ctx, slog.LevelWarn, "evicted_object_heal_error",
				slog.String("path", relPath),
				slog.String("key", histKey),
				slog.String("error", err.Error()),
			)
		}

		return
	}

	e.logger.LogAttrs(ctx, slog.LevelInfo, "evicted_object_healed",
		slog.String("path", relPath),
		slog.String("from", histKey),
	)
}

// healCopy duplicates the historical only-copy onto its current key and
// returns only once the fresh copy is proven to carry the credited content
// (see [Env.confirmHealedCopy]), so its caller's delete runs behind a confirm
// rather than behind a copy that merely reported success.
//
// The historical key is read for its recorded digests before the copy: the
// inventory listing that credited the obligation carries no metadata, and
// those digests are half of what can confirm the fresh copy, the surface's
// ledger signature being the other half. A surface with neither is not copied
// at all: no proof exists to release the historical copy behind, so the sweep
// leaves the object where it is instead of paying a copy every run for a
// convergence it can never finish.
//
// A copy that lands but fails its confirm is removed again. Leaving it would
// put an unproven object at the key the next sweep looks for the obligation
// at, and that sweep's direct hit would release the historical copy through
// [Env.releaseHistoricalKeys] on the strength of the very object this confirm
// refused.
func (e *Env) healCopy(ctx context.Context, histKey, dst, relPath string) error {
	src, err := e.remote.Head(ctx, histKey)
	if err != nil {
		return fmt.Errorf("read historical copy: %w", err)
	}

	want := e.recordedDigest(relPath)

	if want == "" && src.SHA256 == "" && len(src.MD5) == 0 {
		return fmt.Errorf("%w: %q records no digest at its historical key and its record carries "+
			"no signature to read a copy back against", errHealUnconfirmable, relPath)
	}

	err = e.remote.Copy(ctx, histKey, dst)
	if err != nil {
		return fmt.Errorf("copy onto current key: %w", err)
	}

	err = e.confirmHealedCopy(ctx, relPath, dst, src, want)
	if err != nil {
		e.discardHealedCopy(ctx, relPath, dst)

		return err
	}

	return nil
}

// recordedDigest returns the SHA-256 the ledger recorded for the surface at
// relPath, empty when its record carries none: a sealed bundle's sidecar
// records its members' digests rather than the zip's, and an entry can
// predate signatures entirely.
func (e *Env) recordedDigest(relPath string) string {
	entry, ok := e.ledger.Entry(relPath)
	if !ok || entry.Signature == nil {
		return ""
	}

	return entry.Signature.Hash
}

// discardHealedCopy removes a healed copy its confirm refused, so the next
// sweep meets the same pre-heal state this one did: the historical copy
// credited through the rename link, and nothing unproven standing at the
// current key. A delete that fails only warns, since the confirm's own
// failure is what the caller reports, and the object it leaves behind is the
// one the heal would have left in any case before this cleanup existed.
func (e *Env) discardHealedCopy(ctx context.Context, relPath, dst string) {
	_, err := e.remote.Delete(ctx, []string{dst})
	if err == nil || ctx.Err() != nil {
		return
	}

	e.logger.LogAttrs(ctx, slog.LevelWarn, "evicted_object_heal_error",
		slog.String("path", relPath),
		slog.String("key", dst),
		slog.String("error", fmt.Errorf("discard unconfirmed copy: %w", err).Error()),
	)
}

// confirmHealedCopy proves the object now at dst carries the content credited
// at the historical key, wantSHA256 being the surface's recorded signature or
// empty when its record carries none. Both keys hold copies of a surface with
// no local bytes left to hash, and releasing the one the archive has been
// living on because the other is the same size is the rule the eviction gate
// refuses to break (see [Env.offloadFile]); the heal is that same custody
// transfer one key over, so it reads the same ladder from the remote side:
//
//   - a source recording a digest carries it to the destination, where a
//     streamed copy re-verifies the bytes against it at commit and a
//     server-side copy never touches them. Comparing what the two objects
//     record then proves the current key holds that verified copy, egress-free.
//   - a digestless source streams unverified, so the confirm escalates to the
//     record that settled the surface: the fresh copy is read back whole and
//     hashed against its signature (see [Env.confirmRemoteContent]), the same
//     escalation an eviction pays when it meets a digestless remote copy, and
//     paid once for a heal that then converges permanently.
//
// Size is the screen both rungs open with, never the gate on its own.
func (e *Env) confirmHealedCopy(
	ctx context.Context,
	relPath, dst string,
	src remote.ObjectInfo,
	wantSHA256 string,
) error {
	head, err := e.remote.Head(ctx, dst)
	if err != nil {
		return fmt.Errorf("read healed copy: %w", err)
	}

	if head.Size != src.Size {
		return fmt.Errorf("%w: healed copy %q holds %d bytes, want %d (needs manual inspection)",
			ErrRemoteCopyMismatch, dst, head.Size, src.Size)
	}

	if (src.SHA256 != "" && head.SHA256 != "") || (len(src.MD5) > 0 && len(head.MD5) > 0) {
		return confirmRemoteCopy(head, src.Size, remote.Digests{SHA256: src.SHA256, MD5: src.MD5}, dst)
	}

	if wantSHA256 == "" {
		return fmt.Errorf("%w: neither copy of %q records a comparable digest and its record "+
			"carries no signature to read the healed copy back against", errHealUnconfirmable, relPath)
	}

	e.logger.LogAttrs(ctx, slog.LevelWarn, "evicted_object_heal_readback",
		slog.String("path", relPath),
		slog.String("key", dst),
		slog.String("detail", "the copied object records no digest; "+
			"reading it back whole to prove its content before the historical copy is released"),
	)

	return e.confirmRemoteContent(ctx, dst, head.Size, wantSHA256)
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
// prune, the evicted bundles whose remote presence the sweep must verify,
// the workspace subtrees whose sealed-form machinery the walk still sees,
// and the rename-alias prefix pairs the walk declined to follow, which the
// verification uses to credit an only-copy uploaded under a pre-rename name.
type treeSweep struct {
	keep        map[string]struct{}
	sealed      map[string]struct{}
	aliases     map[string]string
	evict       []string
	sync        []string
	suspect     []string
	evictedZips []string
}

// markSealed records the workspace owning relPath as one whose seal machinery
// is still on disk, the local evidence [Env.pruneRemote] reads as a re-shape.
func (s *treeSweep) markSealed(relPath string) {
	if ws, ok := workspacePrefix(relPath); ok {
		s.sealed[ws] = struct{}{}
	}
}

// reshaped reports whether the walk found local evidence that the archive
// itself moved the object at relPath out of its loose form, rather than the
// file being lost from under a surviving ledger entry:
//
//   - its workspace still holds sealed-form files (a bundle, a sidecar a
//     finished eviction left behind, a roll-up). A seal writes the coalesced
//     form and only then removes its loose sources, all within the one
//     workspace subtree, so that machinery is what a re-shape leaves and what
//     a lost subtree cannot fake.
//   - it lives under a rename alias the walk declined to follow, so its
//     subtree walked under the owner's name instead: the bytes are local,
//     spelled differently.
//
// Everything else is unbacked, whatever the ledger remembers about it.
func (s *treeSweep) reshaped(relPath string) bool {
	if ws, ok := workspacePrefix(relPath); ok {
		if _, sealed := s.sealed[ws]; sealed {
			return true
		}
	}

	for alias := range s.aliases {
		// Strictly beneath: the alias directory itself is not reshaped by its
		// own record, and an empty alias (which UnderPrefix reads as
		// everything) must not match.
		if alias != "" && relPath != alias && pathkit.UnderPrefix(relPath, alias) {
			return true
		}
	}

	return false
}

// classifyTree walks the store root and sorts every regular file into the
// sweep's evict or sync list, per the classification ladder on
// [Env.SyncArchive]. A skipped-but-eligible file (an orphan zip, an unproven
// tarball) lands in neither list but still marks its key kept, so the prune
// step cannot delete a remote copy out from under a local file the sweep
// declined to touch; staging temps and ledger-internal files mark nothing.
//
// A bundle sidecar whose zip is no longer beside it is the local record of a
// finished eviction (the zip's only copy is the store), so the walk also
// collects those zips' paths for the sweep's remote-presence verification.
//
// Every sealed-form file also marks its workspace as one a seal has re-shaped
// (see [treeSweep.reshaped]), which is what lets the prune tell a loose key a
// seal superseded from one whose local file was lost.
func (e *Env) classifyTree(ctx context.Context, counters *syncCounters) (*treeSweep, error) {
	sweep := &treeSweep{
		keep:   make(map[string]struct{}),
		sealed: make(map[string]struct{}),
	}

	aliases, err := e.walkEligible(ctx, "", func(relPath string) error {
		sweep.keep[e.RemoteKey(relPath)] = struct{}{}

		if store.InSealedForm(relPath) {
			sweep.markSealed(relPath)
		}

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

	sweep.aliases = aliases

	slices.Sort(sweep.evict)
	slices.Sort(sweep.sync)
	slices.Sort(sweep.suspect)
	slices.Sort(sweep.evictedZips)

	return sweep, nil
}

// recordEvictedZip notes the zip behind one walked sidecar as a finished
// eviction when the zip is no longer beside it, feeding the sweep's
// remote-presence verification. A stat fault leaves the zip's custody
// unknown (it may be local, it may be remote-only), so the zip joins no
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
// [syncEligible] gate. The traversal is [fsid.WalkFiles]: relocation
// symlinks are followed (a subtree an operator moved behind a symlink is a
// supported layout the ledger's shard discovery already reads through), and
// every file reports under its physical name, so the walked paths agree
// with the API-derived names the ledger and remote keys use. The intra-tree
// rename links the walk declined are returned as archive-relative alias to
// owner prefix pairs, so a caller reconciling names minted before a rename
// can rewrite between them. An error from fn, the walk, or the cancellation
// propagates.
func (e *Env) walkEligible(
	ctx context.Context,
	relPrefix string,
	fn func(relPath string) error,
) (map[string]string, error) {
	start := filepath.Join(e.store.Root(), filepath.FromSlash(relPrefix))

	aliases, err := fsid.WalkFiles(ctx, start, func(logical string) error {
		return e.visitEligible(logical, fn)
	})
	if err != nil {
		//nolint:wrapcheck // Callers wrap with their pass's context.
		return nil, err
	}

	rel := make(map[string]string, len(aliases))

	for alias, owner := range aliases {
		aliasRel, aliasErr := filepath.Rel(e.store.Root(), alias)
		ownerRel, ownerErr := filepath.Rel(e.store.Root(), owner)

		if aliasErr != nil || ownerErr != nil {
			return nil, fmt.Errorf("relativize alias %q: %w", alias, errors.Join(aliasErr, ownerErr))
		}

		rel[filepath.ToSlash(aliasRel)] = filepath.ToSlash(ownerRel)
	}

	return rel, nil
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
// carries meaning only as a kernel lock; the ledger's replay log is
// dangerous to mirror, since a stale remote log.ndjson restored over a newer
// snapshot would replay resurrected ledger state; and an eviction stub points
// at the mirror from outside it, so a copy inside the mirror is wrong: a
// restore brings the tarball itself back, and the restored stub would claim it
// is still evicted. None is uploaded or protected from pruning; the stub has
// no key of its own to prune in any case.
func syncEligible(relPath string) bool {
	base := path.Base(relPath)

	if atomicfile.IsTemp(base) {
		return false
	}

	if _, ok := store.RemoteStubTarget(relPath); ok {
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
// the other outcomes is evicted or synced (a synced copy at the eviction key
// carries unproven bytes and could later pass a proper eviction's confirm,
// letting it delete the only local copy), but they differ in weight: a
// tarball with no settled entry at all skips with a warning, while a done
// entry whose signature the local bytes contradict marks the file suspect
// (see [actionSuspect]). In that case the ledger's proven record disagrees
// with the file backing it, the same rot the eviction gate refuses when the
// size is preserved, so it must count against the run instead of exiting
// clean over a corrupt, unmirrorable only-copy.
func (e *Env) classifyTarball(ctx context.Context, relPath string) syncAction {
	entry, ok := e.ledger.Entry(relPath)
	if !ok || entry.Status != manifest.StatusDone || entry.Signature == nil {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_tarball_unproven",
			slog.String("path", relPath),
		)

		return actionSkip
	}

	// The done record is the tarball's only local proof once the file is
	// gone, so the eviction waits for it to be durable: after a failed final
	// flush the record lives only in memory, and deleting the file would
	// leave the next run neither the record nor the bytes. The skip is not a
	// counted failure, since the failed flush already marks the run
	// incomplete, and the file evicts on a later sweep once a flush has landed.
	if !e.ledger.EntryDurable(relPath) {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "sync_tarball_record_not_durable",
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

// isBundleZip reports a sealed-bundle zip. The shape is owned by
// [store.IsBundleZip], which the read side keys its fetch eligibility on, so
// the sweep and the readers cannot disagree about what a bundle zip is.
func isBundleZip(relPath string) bool {
	return store.IsBundleZip(relPath)
}

// isBundleSidecar reports a sealed bundle's sidecar index: the sidecar
// suffix over a bundle-zip stem. After eviction the sidecar is the local
// side's only record of the bundle: the presence obligation the sweep
// verifies against the store, and the proof index the prune must never
// delete out from under a remote-only zip.
func isBundleSidecar(relPath string) bool {
	stem, ok := strings.CutSuffix(relPath, seal.SidecarSuffix)

	return ok && isBundleZip(stem)
}

// isConfigTarball reports an org-wide configuration-version tarball. The shape
// is owned by [store.IsConfigTarball], which the read side keys its eviction
// stubs on, so the sweep and the readers cannot disagree about what a tarball
// is.
func isConfigTarball(relPath string) bool {
	return store.IsConfigTarball(relPath)
}

// RecordOnlyLedgerPrefixes names the archive prefixes whose ledger entries
// may legitimately outlive their local files, for the composition root to
// declare to [manifest.WithRecordOnlyPrefixes]. It lives beside the eviction
// predicates because eviction is the reason: an evicted configuration-version
// tarball leaves no local copy of its bytes, and its ledger entry is the only
// local proof the remote copy exists, so the subtree's absence carries no
// deletion signal. Sealed bundles are evicted too, but each leaves its sidecar
// behind, so a workspace subtree with evicted bundles never reads as gone.
//
// The stub an eviction leaves ([store.RemoteStubSuffix]) does not replace
// this declaration. It is a read-model trace, not a proof: it is unverified,
// an operator can delete it, and an archive evicted by an older build has
// none, so the subtree must stay legitimately sparse by declaration rather
// than by whatever happens to be sitting in it.
func RecordOnlyLedgerPrefixes() []string {
	return []string{store.ConfigVersionsDirName}
}

// underWorkspaceSubtree reports whether relPath lives inside a workspace's
// subtree (projects/<project>/workspaces/<workspace>/...), the scope whose
// mirror converges at the workspace's seal boundary rather than as written.
func underWorkspaceSubtree(relPath string) bool {
	_, ok := workspacePrefix(relPath)

	return ok
}

// workspacePrefix returns the archive-relative prefix of the workspace subtree
// holding relPath (projects/<project>/workspaces/<workspace>), reporting false
// for a path that lives outside any workspace. It is the seal's scope: a seal
// writes its bundles and roll-ups into the same workspace whose loose sources
// it removes, so the prefix is the unit local re-shape evidence is grouped by.
func workspacePrefix(relPath string) (string, bool) {
	const (
		minSegments  = 5 // projects/<p>/workspaces/<ws>/<file>
		prefixLength = 4
	)

	segs := strings.Split(relPath, "/")
	if len(segs) < minSegments || segs[0] != "projects" || segs[2] != "workspaces" {
		return "", false
	}

	return strings.Join(segs[:prefixLength], "/"), true
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
// the archive primitives (today only the organization's remote marker)
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
// ([eagerScope]). The upload is unconditional: the bytes just changed, so the
// remote copy is stale by definition and no inventory probe is needed.
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
// under a .ledger/ directory are skipped whole, because a mid-run shard
// snapshot is stale by construction and the close sweep mirrors the
// post-compaction one. Bundle zips are skipped too (an evicted zip is already
// gone, and an orphan without its sidecar must never be uploaded). Files
// settle sequentially: workspaces seal concurrently, so the workspace
// goroutines are the parallelism, exactly as they are for bundle eviction.
//
// There is no prune: deletion reconciliation stays global at the close
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

	_, err := e.walkEligible(ctx, relPrefix, func(relPath string) error {
		if sealBoundarySkip(relPath) {
			return nil
		}

		e.syncFile(ctx, relPath, inventory, counters)

		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist) && subtreeAbsent(e.store.AbsPath(relPrefix)):
			// A subtree that never materialized (or was removed whole) has
			// nothing to mirror. The root's own absence is required: a mid-walk
			// ENOENT under a still-present root is a directory removed after
			// being listed, and swallowing it would silently truncate the sync
			// of every file the walk had not yet reached, so it falls through
			// to the counted warning below.
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

// subtreeAbsent reports whether nothing exists at absPath, the reading that
// lets [Env.SyncSubtree] tell a subtree that never materialized from a
// directory that vanished mid-walk. Any other stat outcome, a fault included,
// counts as present, so an unreadable root is surfaced rather than swallowed.
func subtreeAbsent(absPath string) bool {
	_, err := os.Stat(absPath)

	return errors.Is(err, fs.ErrNotExist)
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
			// advance the settled counter either: the progress line's counts
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
// file reached an outcome, false only on the wind-down path, where nothing
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
// order: size, then the local MD5 against the digest the inventory records
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
// A key the mirror must never hold at all, one whose path the [syncEligible]
// gate refuses, is stale whatever the local tree holds, and prunes at any
// scale: nothing here ever uploaded it, and the eligible tree is disjoint
// from those paths, so no local file backs such a key and neither the
// presence fence nor the disproportion guard below has anything to say about
// it. That is what makes the gate's promise real for the dangerous one, the
// ledger's replay log: a log.ndjson an out-of-band copy seeded into the
// mirror is deleted rather than left for a later disaster recovery to restore
// beside a newer snapshot and replay into resurrected ledger state.
//
// Two guards refuse pruning, each the fingerprint of a local tree that is
// not the tree the mirror mirrors (where "nothing local backs it" means loss,
// not deletion), and each counts a failure so the run exits
// incomplete rather than reporting a clean mirror it knowingly diverged
// from:
//
//   - a run whose ledger opened empty against a non-empty inventory is a
//     fresh or wrong archive.path pointed at an existing mirror; restore the
//     whole prefix, data files and ledger together, before re-rooting an
//     archive, or the mirror-only history would be deleted wholesale. This
//     refuses the whole prune.
//   - a delete set of **unbacked** keys past [pruneGuardFloor] that
//     outnumbers the keys the walk matched means most of the mirror has no
//     local trace at all: loss, or a deliberate mass deletion that must be
//     an explicit act. This refuses only the unbacked keys.
//
// Provenance splits the stale set for that second guard, and clearing it
// takes both halves: a ledger entry for the key's relpath (the archive did
// own the object) and local evidence that the archive re-shaped it rather
// than lost it (see [treeSweep.reshaped]), which is its workspace's seal
// machinery still on disk, or a rename alias spelling its subtree under
// another name. Such a key is a re-shape artifact whose loose remote copy a
// seal superseded, and the tool's own self-heal after an interrupted run
// produces thousands at once, so they prune freely at any scale.
//
// The ledger entry alone proves nothing. A ledger restored ahead of the data
// files it describes (the very order the refusal above asks for) leaves every
// key ledger-known and locally absent, which is the mass loss these guards
// exist to catch. So a key the ledger remembers but nothing local explains
// faces the disproportion test alongside the keys the ledger never heard of
// (a deleted or lost subtree took its shard too).
func (e *Env) pruneRemote(
	ctx context.Context,
	orgPrefix string,
	inventory map[string]remote.ObjectInfo,
	sweep *treeSweep,
	counters *syncCounters,
) {
	var (
		staleReshaped   []string
		staleUnbacked   []string
		staleIneligible []string
		matched         int
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

		// A path the mirror gate refuses has no local counterpart that could
		// back it: the local file of the same name is the ledger's own log or
		// flock target, a staging temp, or an eviction stub, none of which this
		// tool ever uploads. So the fence below is skipped rather than left to
		// protect a copy the mirror must not hold.
		if !syncEligible(relPath) {
			staleIneligible = append(staleIneligible, key)

			continue
		}

		// A last physical fence before a key is deletable: the stat resolves
		// through any symlink, so a live file the walk reported under another
		// name can never lose its remote copy; deleting is the one direction
		// doubt must not take. A key whose local presence cannot be determined
		// is left in place and counted, so the run exits incomplete instead of
		// clean over a prune it could not prove safe.
		present, presentErr := e.store.Exists(relPath)

		switch {
		case presentErr != nil:
			e.logger.LogAttrs(ctx, slog.LevelError, "sync_prune_unverifiable",
				slog.String("key", key),
				slog.String("error", presentErr.Error()),
			)
			counters.failed.Add(1)

			continue

		case present:
			continue
		}

		if _, known := e.ledger.Entry(relPath); known && sweep.reshaped(relPath) {
			staleReshaped = append(staleReshaped, key)
		} else {
			staleUnbacked = append(staleUnbacked, key)
		}
	}

	if len(staleReshaped)+len(staleUnbacked)+len(staleIneligible) == 0 {
		return
	}

	if !e.ledger.Tally().Resumed {
		e.logger.LogAttrs(ctx, slog.LevelError, "sync_prune_refused",
			slog.Int("keys", len(staleReshaped)+len(staleUnbacked)+len(staleIneligible)),
			slog.String("detail", "this run opened an empty ledger against a non-empty mirror; "+
				"a fresh archive.path must not prune an existing mirror's history. Restore the whole "+
				"remote prefix locally, data files and ledger together, before re-rooting the "+
				"archive"),
		)
		counters.failed.Add(1)

		return
	}

	stale := slices.Concat(staleReshaped, staleIneligible)

	switch {
	case len(staleUnbacked) > pruneGuardFloor && len(staleUnbacked) > matched:
		e.logger.LogAttrs(ctx, slog.LevelError, "sync_prune_refused",
			slog.Int("keys", len(staleUnbacked)),
			slog.Int("matched", matched),
			slog.String("detail", "most of the mirror has no local file backing it, and no seal or "+
				"rename explains where those files went; refusing a mass deletion. If the local "+
				"tree is incomplete, restore it from the remote prefix (the data files, not the "+
				"ledger alone); if this is a deliberate mass deletion, remove the keys from the "+
				"bucket by hand"),
		)
		counters.failed.Add(1)

	default:
		stale = append(stale, staleUnbacked...)
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
// that proved the eviction: after eviction the remote copy is the only copy,
// and gating its survival on local metadata would let a local loss (a wiped
// .ledger, a deleted workspace subtree taking its sidecars with it) cascade
// into deleting the archive's only bytes. The cost is that a deliberately
// deleted workspace leaves its bundles in the bucket, cleaned up by hand.
func evictedSurface(relPath string) bool {
	return isBundleZip(relPath) || isConfigTarball(relPath)
}

// syncProgressInterval is how many settled files pass between progress lines.
const syncProgressInterval = 1000

// recordSyncProgress counts one settled file, moves the sweep's [SyncProgress]
// hook, and emits a progress line every [syncProgressInterval]th one. It is
// called exactly once per settled file, so the monotonic settled counter it
// advances lands each decade marker exactly once and the hook's advances sum
// to the total the sweep seeded. The slog line complements the hook: it
// carries the per-category breakdown the progress views do not.
func (e *Env) recordSyncProgress(ctx context.Context, counters *syncCounters) {
	if counters.progress != nil {
		counters.progress.Advance(1)
	}

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
