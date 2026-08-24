package manifest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/fsid"
)

// schemaVersion is the on-disk manifest format version.
//
// Versions:
//   - 1: the initial sharded snapshot + org-level log layout.
//   - 2: entries may carry a server-side updated-at (see [Entry.UpdatedAt]).
//
// A snapshot naming a newer version than this build supports refuses to load;
// an older snapshot loads, runs the registered migrations over its records
// (see [WithMigrations]), and is folded back at the current version.
const schemaVersion = 2

// defaultCompactThreshold is the org-level log size past which a flush folds
// the log back into the stale shards' snapshots. It bounds a single run's log
// growth and how much a resume replays, while staying large enough that
// ordinary flushes only append.
const defaultCompactThreshold = 64 << 20

var (
	// ErrCorruptManifest indicates that an existing manifest could not be parsed.
	ErrCorruptManifest = errors.New("manifest is corrupt")

	// ErrLegacyLayout indicates the archive carries ledger state from a
	// pre-release layout this version no longer reads (a per-shard log beside a
	// shard's snapshot). Loading anyway would silently drop the records such a
	// file holds, so the load refuses and names it; deleting the file accepts a
	// re-fetch of whatever it recorded.
	ErrLegacyLayout = errors.New("unsupported pre-release ledger layout")
)

// document is the serialized shape of one shard's snapshot on disk. The run-level
// fields are populated only for the org-root shard; other shards leave them zero.
type document struct {
	LastRunAt      time.Time            `json:"lastRunAt,omitzero"`
	LastRun        *RunRecord           `json:"lastRun,omitempty"`
	HighWaterMarks map[string]time.Time `json:"highWaterMarks,omitempty"`
	Entries        map[string]*Entry    `json:"entries,omitempty"`
	Completed      map[string]bool      `json:"completedCollections,omitempty"`
	Settled        map[string]bool      `json:"settledCollections,omitempty"`
	Version        int                  `json:"version"`
	RunCount       int                  `json:"runCount"`
}

// Ledger is the durable per-object record backing resume, incremental re-run,
// and progress reporting.
//
// It guards its in-memory state with a single mutex, so record methods called
// from many workspace workers are race-free and the live tally never drifts from
// the recorded entries. A second mutex serializes flushes so the log takes one
// writer at a time. Across processes, an exclusive flock taken by [Load]
// and released by [Ledger.Close] (or automatically by the kernel when the
// process dies) enforces the single writer the on-disk format assumes. Create
// instances with [Load].
//
// # Sharded snapshots, one log
//
// The ledger partitions its state into per-workspace, per-stack, per-config-
// version, and org-root shards (see [shardKey]), each persisted as a compacted
// snapshot in a co-located .ledger directory. Every entry is keyed by its
// archive-relative path exactly as before; the path only determines which
// shard the entry lives in, so resume, the newest-first early-stop, and the
// watermarks are unchanged. Changes since the snapshots append to a single
// org-level log whose records carry their shard key, so one flush is one
// fsynced append and one crash tears one prefix of the whole batch; there is
// no shard-to-shard ordering for a crash to land between (see [Ledger.Flush]).
// The log folds back into the stale shards' snapshots once it outgrows the
// threshold or a run finishes, so a re-run's cost tracks the log's delta and
// the shards it touched rather than the whole archive. The per-status tally
// stays global to the ledger, seeded on load from every shard's entries.
type Ledger struct {
	now              func() time.Time
	logger           *slog.Logger
	shards           map[string]*shard
	physShards       map[string]*shard
	rootShard        *shard
	recordOnly       map[string]bool
	counts           map[Status]int
	cumulative       map[Status]int
	dropped          map[string]string
	lockFile         *os.File
	runStartedAt     time.Time
	root             string
	target           string
	migrations       []Migration
	bytes            int64
	retried          int64
	compactThreshold int64
	walBytes         int64
	discardedRecords int
	mu               sync.RWMutex
	flushMu          sync.Mutex
	retryAbsent      bool
	resumed          bool
	compactNext      bool
}

// Option configures a [Ledger] passed to [Load].
//
// The available options are:
//   - [WithClock]
//   - [WithLogger]
//   - [WithMigrations]
//   - [WithRecordOnlyPrefixes]
//   - [WithRetryAbsent]
type Option func(*Ledger)

// Migration repairs one ledger entry recorded by an older schema version.
//
// [Load] runs every registered migration (see [WithMigrations]) over every
// entry of every shard whose loaded snapshot predates the current
// schemaVersion, after the log replays and before the ledger is handed back.
// The fromVersion argument is the version of the shard's loaded snapshot; 0
// means unknown (the shard had no snapshot, only log records, which carry no
// version and may have been written by any release), so a migration must
// condition on the entry's own state alone, never on fromVersion pinpointing
// a release. A migration returns the rewritten entry and whether it changed
// anything; it runs again on any open that still finds an old snapshot, so it
// must be idempotent.
type Migration func(relPath string, fromVersion int, e Entry) (Entry, bool)

// WithMigrations registers migrations [Load] runs over the records of shards
// whose snapshots predate the current schema version. Collector packages
// export their migrations for the load call site to pass here, mirroring how
// [WithRecordOnlyPrefixes] carries package knowledge into the load. It returns
// an [Option].
func WithMigrations(migs ...Migration) Option {
	return func(l *Ledger) {
		l.migrations = append(l.migrations, migs...)
	}
}

// WithClock injects the clock used for every recorded timestamp, defaulting to
// [time.Now]. A nil function leaves the default in place. It returns an
// [Option].
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) {
		if now != nil {
			l.now = now
		}
	}
}

// WithLogger sets the structured logger the load path reports non-fatal
// recovery events to (a ledger log truncated at a torn suffix), overriding
// [slog.Default]. A nil logger keeps the default. It returns an [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(l *Ledger) {
		if logger != nil {
			l.logger = logger
		}
	}
}

// WithRetryAbsent toggles re-probing of [StatusAbsent] objects, so
// [Ledger.ShouldFetch] returns true for an object an operator suspects
// answered 404 spuriously or has since been restored. It returns an [Option].
func WithRetryAbsent(retry bool) Option {
	return func(l *Ledger) {
		l.retryAbsent = retry
	}
}

// WithRecordOnlyPrefixes names archive prefixes whose ledger entries may
// legitimately outlive their local files: an evicted object's entry is the
// only local proof its remote copy exists, so the prefix's subtree can be
// sparse or absent without that absence meaning anything. The log replay
// treats a declared prefix's shard as present and never discards its records
// as an operator deletion (see [Load]). The declaration lives with the
// eviction policy that motivates it and is wired in at the composition root,
// so the two cannot drift apart silently. It returns an [Option].
func WithRecordOnlyPrefixes(prefixes ...string) Option {
	return func(l *Ledger) {
		for _, p := range prefixes {
			l.recordOnly[shardKey(p)] = true
		}
	}
}

// Load reads the sharded manifest under root, or starts empty when none exists.
//
// It first takes an exclusive cross-process lock under the org-root ledger
// directory, so a second archiver on the same root (a scheduled run starting
// while the previous one still runs) fails fast with [ErrLedgerLocked] rather
// than interleaving log appends and racing compaction; every crash-consistency
// guarantee in this package assumes a single writer. The lock is an flock, so
// a crashed process releases it automatically (see [Ledger.Close]); a caller
// that finishes cleanly releases it with [Ledger.Close].
//
// It then discovers and loads every shard's snapshot beneath root and replays
// the org-level log on top, routing each record to its shard. Resume and a
// clean first run are one code path: a first run simply finds no shards. A
// shard whose snapshot cannot be parsed returns [ErrCorruptManifest]; a log
// whose tail was torn by a crash is truncated at its last intact record and
// the loss reported to the logger (see [WithLogger]), since every log record
// is re-derived by re-walking. A per-shard log a pre-release layout wrote is
// refused with [ErrLegacyLayout] rather than read.
//
// A replay that discarded a deleted subtree's records (see [Ledger.replayWAL])
// folds before returning: the discard lives in memory, so the log that still
// carries those records is compacted into the surviving shards' snapshots and
// removed, making the operator's deletion durable before the run can re-create
// the subtree it demanded a re-archive of. A fold that cannot complete fails
// the load.
func Load(root string, opts ...Option) (*Ledger, error) {
	l := &Ledger{
		now:              time.Now,
		logger:           slog.Default(),
		shards:           make(map[string]*shard),
		physShards:       make(map[string]*shard),
		recordOnly:       make(map[string]bool),
		counts:           make(map[Status]int),
		cumulative:       make(map[Status]int),
		dropped:          make(map[string]string),
		root:             root,
		compactThreshold: defaultCompactThreshold,
	}

	for _, opt := range opts {
		opt(l)
	}

	// The lock is taken before any shard is read, so the state loaded below can
	// never be a snapshot another process is concurrently compacting.
	lock, err := acquireLock(l.shardDir(""))
	if err != nil {
		return nil, err
	}

	l.lockFile = lock

	l.rootShard = l.shardAt(l.shardDir(""))
	l.shards[""] = l.rootShard

	dirs, err := discoverShards(root)
	if err != nil {
		_ = l.Close() //nolint:errcheck // The load error takes precedence.

		return nil, err
	}

	// Sorted registration keeps the shard-to-first-key assignment stable across
	// loads; each physical directory's snapshot loads once, however many alias
	// keys reach it (see [Ledger.shardAt]).
	loaded := make(map[*shard]struct{})

	for _, sk := range slices.Sorted(maps.Keys(dirs)) {
		sh, ok := l.shards[sk]
		if !ok {
			sh = l.shardAt(dirs[sk])
			l.shards[sk] = sh
		}

		if _, ok := loaded[sh]; ok {
			continue
		}

		loaded[sh] = struct{}{}

		err = sh.loadSnapshot()
		if err != nil {
			_ = l.Close() //nolint:errcheck // The load error takes precedence.

			return nil, err
		}
	}

	// A per-shard log is a pre-release layout this ledger no longer reads.
	// Refuse it rather than load past it: the records it holds would silently
	// drop, and the safe direction (accepting a re-fetch) is an operator
	// decision, taken by deleting the named file. The org-root shard is
	// excluded because its log file is the org log itself, replayed below.
	for _, sh := range l.physShards {
		if sh == l.rootShard {
			continue
		}

		_, err = os.Stat(sh.logPath())

		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			_ = l.Close() //nolint:errcheck // The load error takes precedence.

			return nil, fmt.Errorf("stat shard log %q: %w", sh.logPath(), err)
		}

		_ = l.Close() //nolint:errcheck // The load error takes precedence.

		return nil, fmt.Errorf("%w: per-shard log %q", ErrLegacyLayout, sh.logPath())
	}

	err = l.replayWAL()
	if err != nil {
		_ = l.Close() //nolint:errcheck // The load error takes precedence.

		return nil, err
	}

	// Migrations run after the replay, so they see each entry's final loaded
	// state, and before the tally seeding below, so counts seed from migrated
	// statuses.
	migrated, foldOld, err := l.runMigrations()
	if err != nil {
		_ = l.Close() //nolint:errcheck // The load error takes precedence.

		return nil, err
	}

	// Validate and seed the cumulative tally from every shard's entries, so a
	// resumed run's counts start from the prior run's settled work rather than
	// from zero. An unrecognized status is rejected rather than seeded under a
	// key Tally never reads, which would report a zero total. The iteration is
	// per physical shard, so an entry counts once however many keys alias it.
	for _, sh := range l.physShards {
		for relPath, e := range sh.entries {
			if e == nil {
				_ = l.Close() //nolint:errcheck // The load error takes precedence.

				return nil, fmt.Errorf("%w: entry %q is null", ErrCorruptManifest, relPath)
			}

			if !e.Status.Valid() {
				_ = l.Close() //nolint:errcheck // The load error takes precedence.

				return nil, fmt.Errorf("%w: entry %q has unknown status %q",
					ErrCorruptManifest, relPath, e.Status)
			}

			l.cumulative[e.Status]++
		}
	}

	// A discard removes records from memory only; the log they came from still
	// holds them, so the discard is not durable until the log is gone. Fold
	// before the ledger is handed back: the run is about to re-create the
	// deleted subtree as it re-fetches it, and a kill before the first fold
	// would leave the next load stating a present subtree and replaying the
	// very records this load discarded. That is the honored deletion silently
	// un-honoring itself, with stale done entries answering every fetch
	// decision with a skip. The fold writes each replayed shard's snapshot and
	// then removes the log, so the surviving records keep their durability
	// while the discarded ones cannot come back. A fold that cannot complete
	// refuses the load rather than running on with the resurrection armed.
	if l.discardedRecords > 0 {
		err = l.fold(true)
		if err != nil {
			_ = l.Close() //nolint:errcheck // The load error takes precedence.

			return nil, fmt.Errorf("retire discarded log: %w", err)
		}
	}

	// Migrated records and old-version snapshots persist before the ledger is
	// handed back, so the next load finds current-version snapshots and the
	// migrations do not re-fire. The flush logs the rewritten entries first;
	// after a crash between the snapshot write and the log removal, the replay
	// then lands the migrated record last-writer-wins rather than regressing a
	// fresh snapshot with a stale pre-migration log record. Shards with no
	// snapshot at all are left to fold naturally (see runMigrations).
	if migrated {
		err = l.Flush()
		if err != nil {
			_ = l.Close() //nolint:errcheck // The load error takes precedence.

			return nil, fmt.Errorf("persist migrated records: %w", err)
		}
	}

	if foldOld {
		err = l.fold(true)
		if err != nil {
			_ = l.Close() //nolint:errcheck // The load error takes precedence.

			return nil, fmt.Errorf("persist migrated snapshots: %w", err)
		}
	}

	return l, nil
}

// runMigrations runs the registered migrations (see [WithMigrations]) over
// every entry of every physical shard whose loaded snapshot predates the
// current schema version. The first result reports whether any entry was
// rewritten; the second whether any shard holds an old-version snapshot that
// must fold back at the current version. Shards with no snapshot at all only
// re-run their idempotent migrations on later opens, so a fresh archive's
// first load writes nothing.
func (l *Ledger) runMigrations() (bool, bool, error) {
	var migrated, foldOld bool

	for _, sh := range l.physShards {
		if sh.loadedVersion >= schemaVersion {
			continue
		}

		if sh.loadedVersion >= 1 {
			sh.stale = true
			foldOld = true
		}

		if len(l.migrations) == 0 {
			continue
		}

		for relPath, e := range sh.entries {
			if e == nil {
				continue // The tally seeding refuses the load with the cause.
			}

			out, changed := *e, false

			for _, mig := range l.migrations {
				if next, ok := mig(relPath, sh.loadedVersion, cloneEntry(out)); ok {
					out, changed = next, true
				}
			}

			if !changed {
				continue
			}

			if !out.Status.Valid() {
				return false, false, fmt.Errorf("%w: migration rewrote entry %q to unknown status %q",
					ErrCorruptManifest, relPath, out.Status)
			}

			stored := cloneEntry(out)
			stored.counted = e.counted
			sh.entries[relPath] = &stored
			sh.dirtyEntries[relPath] = struct{}{}
			migrated = true
		}
	}

	return migrated, foldOld, nil
}

// Close releases the ledger's cross-process lock, so another process (or a
// later [Load] in this one) can open the root. It does not flush; callers
// flush before closing. It is idempotent, and safe on a ledger whose lock was
// already released.
//
// Closing is only needed on a clean finish. A process that crashes while
// holding the lock never calls Close, and needs to have made no promise to:
// the lock is an flock, which the kernel releases the moment the process's
// file descriptors close, so a killed or panicked run leaves no stale lock and
// no recovery step. The lock file itself is left on disk; its presence carries
// no meaning.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lockFile == nil {
		return nil
	}

	f := l.lockFile
	l.lockFile = nil

	// Closing the descriptor releases the flock; no explicit unlock is needed.
	err := f.Close()
	if err != nil {
		return fmt.Errorf("release ledger lock: %w", err)
	}

	return nil
}

// shardDir returns the absolute .ledger directory for a shard key, co-located
// with the subtree it indexes; the empty key is the org root.
func (l *Ledger) shardDir(sk string) string {
	if sk == "" {
		return filepath.Join(l.root, LedgerDirName)
	}

	return filepath.Join(l.root, filepath.FromSlash(sk), LedgerDirName)
}

// discoverShards finds the .ledger directories beneath root and maps each to its
// shard key. It reads the known structural depths with [os.ReadDir] rather than
// walking the whole tree, so discovery costs the number of workspaces and
// stacks, not the number of archived objects; and it never feeds root through a
// glob, so an operator-chosen root containing a glob metacharacter ('*', '?',
// '[') cannot silently hide a shard and re-archive the organization from empty.
//
// Discovery follows symlinked directories (see [readSubdirs]), so one physical
// .ledger directory can surface under several keys (a relocation symlink
// beside its target, say). Every reachable key is returned, aliases included:
// each key must resolve in [Ledger.lookupShard], or the subtree behind the
// alias would re-fetch on every run. Collapsing the aliases onto one shard is
// [Ledger.shardAt]'s job, keyed by physical identity, so two keys over one
// snapshot file never open two shards.
func discoverShards(root string) (map[string]string, error) {
	out := make(map[string]string)

	add := func(dir string) error {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			// A shard directory that fails to stat for any reason other than
			// being gone (a permission change, a flaky network filesystem) must
			// not be silently dropped: doing so hides its persisted entries and
			// re-archives the whole subtree from empty. Only a genuinely removed
			// shard is skipped.
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil
			}

			return fmt.Errorf("stat shard %q: %w", dir, statErr)
		}

		if !info.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(root, filepath.Dir(dir))
		if relErr != nil {
			return fmt.Errorf("relativize shard %q: %w", dir, relErr)
		}

		sk := filepath.ToSlash(rel)
		if sk == "." {
			sk = ""
		}

		out[sk] = dir

		return nil
	}

	// The two fixed-depth shards live at literal paths.
	for _, dir := range []string{
		filepath.Join(root, LedgerDirName),
		filepath.Join(root, configVersionsSegment, LedgerDirName),
	} {
		err := add(dir)
		if err != nil {
			return nil, err
		}
	}

	// Per-workspace and per-stack shards live two levels under each project.
	projectsDir := filepath.Join(root, "projects")

	projects, err := readSubdirs(projectsDir)
	if err != nil {
		return nil, err
	}

	for _, proj := range projects {
		for _, kind := range []string{"workspaces", "stacks"} {
			kindDir := filepath.Join(projectsDir, proj, kind)

			children, childErr := readSubdirs(kindDir)
			if childErr != nil {
				return nil, childErr
			}

			for _, child := range children {
				err = add(filepath.Join(kindDir, child, LedgerDirName))
				if err != nil {
					return nil, err
				}
			}
		}
	}

	return out, nil
}

// readSubdirs returns the immediate subdirectory names of dir, or nil when dir
// does not exist. Symlinks are resolved so a project or workspace directory an
// operator relocated behind a symlink still surfaces the shards beneath it. A
// read error other than a missing directory is returned so a permission or
// filesystem fault does not silently hide shards below it.
func readSubdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		isDir, err := entryIsDir(dir, e)
		if err != nil {
			return nil, err
		}

		if isDir {
			names = append(names, e.Name())
		}
	}

	return names, nil
}

// entryIsDir reports whether e names a directory, resolving a symlinked entry
// with [os.Stat]: [os.ReadDir] does not follow symlinks, so a directory an
// operator relocated behind a symlink would otherwise read as a non-directory,
// hiding every shard beneath it and re-archiving (then overwriting) the
// subtree's ledger history. A dangling link reads as no directory; any other
// stat fault is surfaced, matching discovery's no-silent-drop policy.
func entryIsDir(dir string, e fs.DirEntry) (bool, error) {
	if e.IsDir() {
		return true, nil
	}

	if e.Type()&fs.ModeSymlink == 0 {
		return false, nil
	}

	info, err := os.Stat(filepath.Join(dir, e.Name()))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("stat %q: %w", filepath.Join(dir, e.Name()), err)
	}

	return info.IsDir(), nil
}

// shardFor returns the shard that owns key, creating it if absent. Callers that
// mutate must hold the write lock; a first record into a new workspace creates
// its shard here, routed through [Ledger.shardAt] so an aliased directory joins
// its existing shard.
func (l *Ledger) shardFor(key string) *shard {
	sk := shardKey(key)

	sh, ok := l.shards[sk]
	if !ok {
		sh = l.shardAt(l.shardDir(sk))
		l.shards[sk] = sh
	}

	return sh
}

// lookupShard returns the shard that owns key when it already exists, without
// creating one. Read paths use it so a lookup never materializes a shard.
func (l *Ledger) lookupShard(key string) (*shard, bool) {
	sh, ok := l.shards[shardKey(key)]

	return sh, ok
}

// shardByKey returns the shard for an already-resolved shard key, creating it
// if absent. The WAL replay uses it: a record's shard tag is the shard key
// itself, not an entry key for [shardKey] to route.
func (l *Ledger) shardByKey(sk string) *shard {
	sh, ok := l.shards[sk]
	if !ok {
		sh = l.shardAt(l.shardDir(sk))
		l.shards[sk] = sh
	}

	return sh
}

// shardAt returns the shard backed by the physical directory dir denotes,
// creating it if absent. Every shard creation routes through it, so a key that
// aliases an already-registered directory (a rename symlink an operator left
// beside its target, say) joins the existing shard rather than opening a
// second one over the same snapshot file. Two shards over one file would each
// seed the cumulative tally from the same entries, and whichever folded second
// would capture a snapshot missing the records only the other held, by which
// point the log that carried them was already retired.
//
// Identity comes from [fsid.Canonical], which resolves through the deepest
// existing ancestor, so a symlinked workspace aliases even before its .ledger
// is first created beneath it. Callers hold the write lock; the resolution
// stats the filesystem, which costs a handful of syscalls once per shard
// creation, not per record.
func (l *Ledger) shardAt(dir string) *shard {
	phys := fsid.Canonical(dir)

	sh, ok := l.physShards[phys]
	if !ok {
		sh = newShard(dir)
		l.physShards[phys] = sh
	}

	return sh
}

// walPath returns the absolute path of the ledger's single org-level log,
// which lives beside the org-root shard's snapshot.
func (l *Ledger) walPath() string {
	return filepath.Join(l.shardDir(""), LogFileName)
}

// replayWAL replays the org-level log on top of every shard's loaded
// snapshot, routing each record to its shard by the record's shard tag. A
// shard named only by log records starts from an empty snapshot; every
// replayed shard is marked stale so the next fold captures the records
// durably.
//
// A record whose shard subtree no longer exists on disk is discarded rather
// than replayed: deleting an archived subtree is how an operator demands a
// re-archive, and resurrecting the subtree's entries from the log would
// answer every fetch decision with a skip and freeze the deletion forever.
// The discard is logged and costs a re-fetch (every log record is re-derived
// by re-walking), which is the safe direction. It is a memory-only edit here;
// the discarded records still sit in the log, so [Load] folds the log away
// before the run starts (see there). Only a positively absent
// subtree discards; a stat fault surfaces as an error, matching discovery's
// no-silent-drop policy. Org-root records always replay: their subtree is the
// archive root itself, which holds the log being replayed. So do records
// under a declared record-only prefix (see [WithRecordOnlyPrefixes]), whose
// subtree is legitimately sparse under eviction and whose absence therefore
// carries no deletion signal.
//
// The shard tag is one name for the shard, not the only one: a flush drains
// under the lexically first registered key, which can be a rename alias
// whose link the operator later removes while the physical subtree lives on.
// A record whose tag path is gone therefore falls back to the shard key its
// own payload routes to before it is discarded; an operator deletion still
// discards, because then both names are gone.
func (l *Ledger) replayWAL() error {
	recs, n, err := replayLog(l.walPath(), l.logger)
	if err != nil {
		return err
	}

	present := map[string]bool{"": true}
	maps.Copy(present, l.recordOnly)

	discarded := make(map[string]int)

	for i := range recs {
		sk, ok, presErr := l.replayShardKey(&recs[i], present)
		if presErr != nil {
			return presErr
		}

		if !ok {
			discarded[recs[i].Shard]++

			continue
		}

		sh := l.shardByKey(sk)

		err = sh.applyRecord(&recs[i])
		if err != nil {
			return err
		}

		sh.stale = true
	}

	for _, sk := range slices.Sorted(maps.Keys(discarded)) {
		l.discardedRecords += discarded[sk]

		l.logger.Warn("ledger_records_discarded",
			slog.String("shard", sk),
			slog.Int("records", discarded[sk]),
			slog.String("reason", "shard subtree removed; its objects re-archive"),
		)
	}

	l.walBytes = n

	return nil
}

// replayShardKey resolves the shard key one log record replays under: its
// tag when the tag's subtree survives, the key its own payload routes to
// when only the tag path is gone (a removed rename link), or ok=false when
// both are gone and the record discards as a deliberate deletion. The
// present map memoizes subtree checks across the replay.
func (l *Ledger) replayShardKey(rec *walRecord, present map[string]bool) (string, bool, error) {
	sk := rec.Shard

	ok, err := l.subtreePresent(sk, present)
	if err != nil || ok {
		return sk, ok, err
	}

	fb := recordShardKey(rec)
	if fb == sk {
		return sk, false, nil
	}

	ok, err = l.subtreePresent(fb, present)

	return fb, ok, err
}

// subtreePresent reports whether the shard key's subtree exists on disk,
// memoizing into present. A stat fault other than absence surfaces, matching
// discovery's no-silent-drop policy.
func (l *Ledger) subtreePresent(sk string, present map[string]bool) (bool, error) {
	ok, known := present[sk]
	if known {
		return ok, nil
	}

	_, statErr := os.Stat(filepath.Join(l.root, filepath.FromSlash(sk)))

	switch {
	case statErr == nil:
		ok = true
	case errors.Is(statErr, fs.ErrNotExist):
		ok = false
	default:
		return false, fmt.Errorf("stat shard subtree %q: %w", sk, statErr)
	}

	present[sk] = ok

	return ok, nil
}

// totalEntries returns the number of entries across every physical shard, so
// an aliased shard's entries count once.
func (l *Ledger) totalEntries() int {
	n := 0
	for _, sh := range l.physShards {
		n += len(sh.entries)
	}

	return n
}

// ShouldFetch reports whether the current pass should fetch the object at
// relPath.
//
// An object absent from the ledger and one recorded as [StatusErrored] are
// fetched; settled statuses are not. A recorded absence is fetched only when
// retry-absent is enabled (see [WithRetryAbsent]).
func (l *Ledger) ShouldFetch(relPath string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(relPath)
	if !ok {
		return true
	}

	e, ok := sh.entries[relPath]
	if !ok {
		return true
	}

	if e.Status == StatusAbsent {
		return l.retryAbsent
	}

	return !e.Status.Settled()
}

// Entry returns a copy of the recorded entry for relPath and whether it exists.
func (l *Ledger) Entry(relPath string) (Entry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(relPath)
	if !ok {
		return Entry{}, false
	}

	e, ok := sh.entries[relPath]
	if !ok {
		return Entry{}, false
	}

	return cloneEntry(*e), true
}

// EntryDurable reports whether relPath has an entry all of whose recorded
// state has reached durable storage (the shard's snapshot or the fsynced org
// log), so the record would survive a crash. Custody decisions consult
// it: an eviction may destroy a local only-copy only once the done record
// proving the remote copy is durable, or a crash between the delete and the
// next flush would leave neither the record nor the bytes. An entry a flush
// has drained but not yet appended reads non-durable too (see
// [shard.drainDirty]), so the answer is safe under any interleaving with a
// concurrent flush.
func (l *Ledger) EntryDurable(relPath string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(relPath)
	if !ok {
		return false
	}

	if _, ok := sh.entries[relPath]; !ok {
		return false
	}

	if _, dirty := sh.dirtyEntries[relPath]; dirty {
		return false
	}

	_, inflight := sh.inflightEntries[relPath]

	return !inflight
}

// Now returns the ledger's clock reading, so a caller stamping records of
// its own shares the run's single clock and stays deterministic under
// [WithClock].
func (l *Ledger) Now() time.Time {
	return l.now()
}

// RecordOption configures a single record call.
//
// The available options are:
//   - [WithUpdatedAt]
type RecordOption func(*Entry)

// WithUpdatedAt stamps the server-side updated-at the object reported onto
// its entry (see [Entry.UpdatedAt]). A zero time leaves the entry unstamped.
// It returns a [RecordOption].
func WithUpdatedAt(t time.Time) RecordOption {
	return func(e *Entry) {
		e.UpdatedAt = t
	}
}

// RecordDone records a successful fetch of relPath with its content signature.
// A record without [WithUpdatedAt] clears any server-side updated-at a prior
// record stamped, so the entry never carries a stamp staler than its content.
func (l *Ledger) RecordDone(relPath string, sig Signature, opts ...RecordOption) {
	stored := sig

	l.record(relPath, StatusDone, func(now time.Time, e *Entry) {
		e.FetchedAt = now
		e.UpdatedAt = time.Time{}
		e.Signature = &stored
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false

		for _, opt := range opts {
			opt(e)
		}
	})
}

// RecordAbsent records relPath as gone (a 404), settling it immediately: a
// re-run leaves it alone unless retry-absent is enabled (see
// [WithRetryAbsent]). It keeps the cause text so the entry documents why the
// gap exists; the caller is expected to have confirmed the absence in-run
// before believing it, so a brief consistency blip is not settled as a gap
// from one response.
func (l *Ledger) RecordAbsent(relPath string, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	l.record(relPath, StatusAbsent, func(now time.Time, e *Entry) {
		e.FetchedAt = now
		e.LastError = msg
		e.LastErrorAt = now
		e.Transient = false
	})
}

// RecordErrored records a failed fetch of relPath, noting whether the cause was
// transient rather than terminal so a rate-limit blip is not mistaken for a
// permanent absence.
func (l *Ledger) RecordErrored(relPath string, cause error, transient bool) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	l.record(relPath, StatusErrored, func(now time.Time, e *Entry) {
		e.LastError = msg
		e.LastErrorAt = now
		e.Transient = transient
	})
}

// RecordForbidden records relPath as inaccessible to the archiving identity (an
// HTTP 403). It keeps the cause text so the entry documents the permission gap,
// and unlike [Ledger.RecordAbsent] it is not settled, so a re-run under a
// token with broader access retries it.
func (l *Ledger) RecordForbidden(relPath string, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	l.record(relPath, StatusForbidden, func(now time.Time, e *Entry) {
		e.LastError = msg
		e.LastErrorAt = now
		e.Transient = false
	})
}

// RecordSkipped records relPath as intentionally deferred, a settled state that
// is not mistaken for a gap.
func (l *Ledger) RecordSkipped(relPath string) {
	l.record(relPath, StatusSkipped, func(_ time.Time, e *Entry) {
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false
	})
}

// RecordNotApplicable records relPath as not applicable to this archive, a
// settled state that is not mistaken for a gap.
func (l *Ledger) RecordNotApplicable(relPath string) {
	l.record(relPath, StatusNotApplicable, func(_ time.Time, e *Entry) {
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false
	})
}

// MirrorReference maintains a run-scoped reference gate at key, a ledger-only
// proxy that mirrors whether a cross-shard write the run depends on has settled.
//
// A run's Archive closure writes a few objects into sibling shards outside its
// own subtree: its created-by user and each event actor into users/<id>.json,
// and a config-version tarball into config-versions/<id>.tar.gz. A local write of
// one that fails records an errored entry in that foreign shard, invisible to the
// run walk's retry gate ([Collection.HasUnsettled] scans only the run's own
// shard), so nothing would ever retry it and the object stays uncaptured. A gate
// under the run's own prefix makes that outstanding work visible to the walk
// without moving the foreign write out of its org-global location.
//
// It is idempotent and creates no entry for a reference that never failed:
//   - settled false records [StatusPending] unless the gate is already pending,
//     so a re-walk does not re-dirty an open gate;
//   - settled true clears an existing unsettled gate to [StatusReferenceCleared]
//     and does nothing when no gate exists or it is already settled, so a
//     reference that always succeeded leaves no entry behind and a cleared gate
//     is not re-dirtied on later re-walks. The cleared gate is settled but, like
//     the pending one, stays out of [Ledger.Tally]'s object counts, so a
//     recovered reference never inflates the not-applicable or total tally.
func (l *Ledger) MirrorReference(key string, settled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !settled {
		// Open the gate; leave an already-pending one untouched so a re-walk does
		// not re-dirty it. A gate cleared on a prior run but whose foreign write
		// has since failed again is re-opened here, so a transient cross-shard
		// disagreement self-heals.
		l.openGateLocked(key, StatusPending)

		return
	}

	// Clear an open or absence-marked gate; leave a missing or already-cleared
	// one untouched so a reference that always succeeded never creates or
	// re-dirties an entry. An absence-marked gate clears here once its mirrored
	// target is restored, so the retry-absent trace retires with the absence it
	// stood for. The cleared gate settles the walk but is kept out of the object
	// tally.
	sh, ok := l.lookupShard(key)
	if !ok {
		return
	}

	e, ok := sh.entries[key]
	if !ok {
		return
	}

	if !e.Status.Settled() || e.Status == StatusReferenceAbsent {
		l.recordLocked(key, StatusReferenceCleared, clearGateError)
	}
}

// openGateLocked records the gate at key [StatusPending] unless its current
// status is one of preserve, the statuses whose record already stands for
// outstanding work and must survive the open.
//
// The caller holds the write lock across both the status test and the record,
// which is what keeps the pair atomic: a status a concurrent record writes
// either precedes the test, and is preserved, or follows the record, and wins.
// A decision taken from a status sampled before the lock (a [Ledger.Entry]
// read, say) would instead leave a window for that record to land in and then
// be overwritten by a bare pending gate carrying none of its detail.
func (l *Ledger) openGateLocked(key string, preserve ...Status) {
	if sh, ok := l.lookupShard(key); ok {
		if e, ok := sh.entries[key]; ok && slices.Contains(preserve, e.Status) {
			return
		}
	}

	l.recordLocked(key, StatusPending, clearGateError)
}

// clearGateError resets the failure fields of a reference gate or obligation
// marker, whose status alone carries what the marker mirrors: a gate re-opened
// after an earlier failure would otherwise keep the cause of a fetch it no
// longer describes.
func clearGateError(_ time.Time, e *Entry) {
	e.LastError = ""
	e.LastErrorAt = time.Time{}
	e.Transient = false
}

// MirrorReferenceAbsent marks the reference gate at key settled over an
// absence: every mirrored write settled, at least one as a confirmed 404.
// Unlike a clear, it records even when no gate exists yet: the absence must
// leave a trace in the referencing run's own shard, because it settled in a
// foreign shard the run walk never scans, and a retry-absent run re-opens
// the walk through exactly this trace (see [Ledger.HasRetryableAbsentUnder]).
// It is idempotent: a gate already marked absent is not re-dirtied, and the
// mark retires to [StatusReferenceCleared] once the mirrored target is
// restored (see [Ledger.MirrorReference]).
func (l *Ledger) MirrorReferenceAbsent(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if sh, ok := l.lookupShard(key); ok {
		if e, ok := sh.entries[key]; ok && e.Status == StatusReferenceAbsent {
			return
		}
	}

	l.recordLocked(key, StatusReferenceAbsent, clearGateError)
}

// ReferencePending reports whether a reference gate at key exists and is
// unsettled.
//
// A split-read that both mirrors a cross-shard write and re-derives its source
// (the run-event actors, hydrated only in the list include) consults it to force
// the read while the gate is open. It differs from [Ledger.ShouldFetch], which
// treats an absent path as "fetch": a gate's absence means no outstanding work,
// so this returns false for a gate that was never created or has been cleared.
func (l *Ledger) ReferencePending(key string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(key)
	if !ok {
		return false
	}

	e, ok := sh.entries[key]
	if !ok {
		return false
	}

	return !e.Status.Settled()
}

// record applies status and mutate to the entry at relPath in its shard and
// keeps both tallies in step: the per-run counts swap any status this entry
// already contributed to the run, and the cumulative counts swap the entry's
// prior settled status so they always reflect every entry's current state.
func (l *Ledger) record(relPath string, status Status, mutate func(now time.Time, e *Entry)) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.recordLocked(relPath, status, mutate)
}

// recordLocked is the body of [Ledger.record] with the write lock already held,
// so a caller that must inspect the entry and record atomically (see
// [Ledger.MirrorReference]) can do both under one lock acquisition.
func (l *Ledger) recordLocked(relPath string, status Status, mutate func(now time.Time, e *Entry)) {
	now := l.now()
	sh := l.shardFor(relPath)

	e, ok := sh.entries[relPath]
	if !ok {
		e = &Entry{FirstSeen: now}
		sh.entries[relPath] = e
	}

	if e.counted {
		l.counts[e.Status]--
	}

	// A pre-existing entry already contributes its old status to the cumulative
	// tally; move it to the new status. A brand-new entry has none to move.
	if ok {
		l.cumulative[e.Status]--
	}

	e.Status = status
	e.Attempts++

	mutate(now, e)

	e.counted = true
	l.counts[status]++
	l.cumulative[status]++

	sh.dirtyEntries[relPath] = struct{}{}
}

// RecordDerived stamps the archive-relative paths of the children the listing
// at listPath derived onto the listing's own entry (see
// [Entry.DerivedChildren]), replacing any prior manifest, so
// [Ledger.DerivedSettled] can later prove each child's entry still stands.
// Call it after the listing settles; stamping a path with no entry is a
// no-op, since a manifest without its owner would never be consulted.
func (l *Ledger) RecordDerived(listPath string, children ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sh, ok := l.lookupShard(listPath)
	if !ok {
		return
	}

	e, ok := sh.entries[listPath]
	if !ok {
		return
	}

	e.DerivedChildren = slices.Sorted(slices.Values(children))
	sh.dirtyEntries[listPath] = struct{}{}
}

// DerivedSettled reports whether every child the settled listing at listPath
// recorded (see [Ledger.RecordDerived]) still has a settled entry of its own,
// judged by the same [Ledger.ShouldFetch] predicate the walk retries on. A
// listing with no entry or no recorded manifest reports true, preserving the
// skip for listings settled before manifests were stamped; a child whose
// entry has gone missing reports false, so the gate re-opens the one listing
// that can reach it.
func (l *Ledger) DerivedSettled(listPath string) bool {
	entry, ok := l.Entry(listPath)
	if !ok {
		return true
	}

	return !slices.ContainsFunc(entry.DerivedChildren, l.ShouldFetch)
}

// HighWaterMark returns the recorded watermark for key, or the zero time when
// none is set.
//
// The same keyed store holds both the newest CreatedAt archived for an
// append-mostly collection and the audit trail's Since cursor.
func (l *Ledger) highWaterMark(key string) time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(key)
	if !ok {
		return time.Time{}
	}

	return sh.watermarks[key]
}

// AdvanceHighWaterMark advances the watermark for key toward t, keeping the
// later of the two so the mark only ever moves forward.
func (l *Ledger) advanceHighWaterMark(key string, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sh := l.shardFor(key)

	if t.After(sh.watermarks[key]) {
		sh.watermarks[key] = t
		sh.dirtyWatermarks[key] = struct{}{}
	}
}

// IsCollectionComplete reports whether the append-mostly collection under key
// was walked to its end in a prior run.
//
// It is false until the collection has been fully paged at least once, so an
// interrupted first walk (which leaves a newest-first prefix archived and an
// older tail missing) is not mistaken for a complete collection. A re-run may
// stop early at the newest already-archived element only once this is true.
func (l *Ledger) isCollectionComplete(key string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(key)
	if !ok {
		return false
	}

	return sh.completed[key]
}

// MarkCollectionComplete records that the collection under key has been walked
// to its end, so later re-runs may stop early once they reach already-archived
// history.
//
// It is sticky: an append-mostly collection only grows at its newest end, so
// once its tail is fully archived that tail stays archived.
func (l *Ledger) markCollectionComplete(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sh := l.shardFor(key)
	sh.completed[key] = true
	sh.dirtyCompleted[key] = struct{}{}
}

// IsCollectionSettled reports whether the newest full walk of the collection
// under key settled it: saw only terminal elements and left no unsettled child.
//
// Unlike completion it is not sticky. It is false until a full walk records it,
// and a later walk that reaches a still-running element flips it back to false
// (see [Collection.SetSettled]), so a collection with work still in flight
// re-pages in full rather than stopping at a done boundary that a later,
// out-of-order element sits behind.
func (l *Ledger) isCollectionSettled(key string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(key)
	if !ok {
		return false
	}

	return sh.settled[key]
}

// SetCollectionSettled records whether the newest full walk of the collection
// under key settled it, so a later re-run may stop early only while it stays
// true.
//
// It is deliberately non-monotonic: passing false undoes an earlier true, which
// is how a walk that reaches a still-running element forces the next run to
// re-page the collection until that element settles.
//
// A false write is additionally drained ahead of every entry record in the
// next flushed batch (see [shard.drainDirty] and [walRecordClass]):
// unsettlement guards the entries a walk is about to record, so it must never
// become durable after them. The batch lands in the single org-level log, so
// the order holds across shards; keying the flag on a path prefix of the
// entries it guards remains right so [Collection.HasUnsettled] scans the
// shard that owns them.
func (l *Ledger) setCollectionSettled(key string, settled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sh := l.shardFor(key)
	sh.settled[key] = settled
	sh.dirtySettled[key] = struct{}{}

	if !settled {
		sh.dirtyUnsettled[key] = struct{}{}
	}
}

// HasUnsettledUnder reports whether any entry beneath prefix (a key beginning
// with prefix + "/") is recorded in a status this run still retries.
//
// It scans the shard that owns prefix, so a collection's walk can tell whether
// an errored or forbidden child left below its newest done boundary still needs
// reaching. A prefix whose shard holds no such child returns false.
//
// Under retry-absent (see [WithRetryAbsent]) a recorded absence counts as
// unsettled too, mirroring [Ledger.ShouldFetch]: most absences live below a
// settled collection's newest-first early-stop boundary (expired plan logs,
// plan JSON of old runs), and a boundary gated on the normal settled
// predicate would stop the walk before any of them is re-probed, leaving the
// flag silently inert for exactly the entries it exists for. The widened
// predicate keeps a retry-absent run's collections recorded unsettled while
// absences remain, so the following normal run re-pages them once and then
// settles them again.
func (l *Ledger) hasUnsettledUnder(prefix string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(prefix)
	if !ok {
		return false
	}

	under := prefix + "/"

	for relPath, e := range sh.entries {
		if !strings.HasPrefix(relPath, under) {
			continue
		}

		if !e.Status.Settled() || l.retryableAbsence(e.Status) {
			return true
		}
	}

	return false
}

// retryableAbsence reports whether status records an absence a retry-absent
// run re-probes: a directly recorded 404, or a reference gate mirroring one
// that settled in a foreign shard the walk never scans. The two travel
// together: a widening that saw only the direct form left the flag blind to
// exactly the absences a gate exists to carry across shards.
func (l *Ledger) retryableAbsence(s Status) bool {
	return l.retryAbsent && (s == StatusAbsent || s == StatusReferenceAbsent)
}

// HasRetryableAbsentUnder reports whether any object under prefix is recorded
// [StatusAbsent], or carries a [StatusReferenceAbsent] gate mirroring a
// foreign absence, while retry-absent is enabled (see [WithRetryAbsent]):
// exactly the entries a retry-absent run exists to re-probe. Skip gates over
// metered listings consult it, because an absent object reachable only through
// its listing can be re-probed only if the listing runs again; the gate's
// usual settledness reads the absence as done and would leave the flag inert
// for its own target.
func (l *Ledger) HasRetryableAbsentUnder(prefix string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if !l.retryAbsent {
		return false
	}

	sh, ok := l.lookupShard(prefix)
	if !ok {
		return false
	}

	under := prefix + "/"

	for relPath, e := range sh.entries {
		if strings.HasPrefix(relPath, under) && l.retryableAbsence(e.Status) {
			return true
		}
	}

	return false
}

// MarkSurfaceDropped records that the enumeration of surface failed this run
// for a non-cancellation reason, so the run cannot know that surface's extent
// and must not report a complete archive.
//
// It is the one channel every collector routes a dropped listing through: a
// per-object failure is visible as an errored entry, but a listing that never
// completed records no entries at all, so without this record the gap would be
// invisible to the run's completeness signal. The record is run-scoped (reset
// by [Ledger.StartRun], never persisted): a re-run retries the enumeration
// from scratch, so the drop describes this run, not the archive.
func (l *Ledger) MarkSurfaceDropped(surface string, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.dropped[surface] = msg
}

// DroppedSurfaces returns every surface recorded dropped this run, sorted by
// surface, so the run's close can name what was missed rather than only count
// it. See [Ledger.MarkSurfaceDropped].
func (l *Ledger) DroppedSurfaces() []DroppedSurface {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]DroppedSurface, 0, len(l.dropped))
	for surface, msg := range l.dropped {
		out = append(out, DroppedSurface{Surface: surface, Error: msg})
	}

	slices.SortFunc(out, func(a, b DroppedSurface) int {
		return strings.Compare(a.Surface, b.Surface)
	})

	return out
}

// AddBytes adds n to the run's downloaded-bytes counter.
func (l *Ledger) AddBytes(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.bytes += n
}

// AddRetry counts one in-run re-fetch (a retry of a transient failure or the
// re-probe confirming a 404) so progress can show how much of the run is
// re-work absorbed by retrying rather than surfacing as errored objects.
func (l *Ledger) AddRetry() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.retried++
}

// SetTarget sets the current org, project, or workspace shown by progress.
func (l *Ledger) SetTarget(target string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.target = target
}

// Tally returns a snapshot of the live counters.
//
// The per-status counts are cumulative across runs (every entry counted by its
// current status), so a resumed run reflects the prior run's settled work;
// BytesDownloaded stays per-run.
func (l *Ledger) Tally() Tally {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return Tally{
		Target:           l.target,
		Done:             l.cumulative[StatusDone],
		Absent:           l.cumulative[StatusAbsent],
		Skipped:          l.cumulative[StatusSkipped],
		Errored:          l.cumulative[StatusErrored],
		Forbidden:        l.cumulative[StatusForbidden],
		NotApplicable:    l.cumulative[StatusNotApplicable],
		SurfacesDropped:  len(l.dropped),
		RecordsDiscarded: l.discardedRecords,
		Retried:          l.retried,
		BytesDownloaded:  l.bytes,
		Resumed:          l.resumed,
	}
}

// Failures returns every object currently recorded as [StatusErrored] or
// [StatusForbidden], errored entries first and each class sorted by path, so a
// run summary can name what failed rather than only count it.
func (l *Ledger) Failures() []Failure {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var out []Failure

	for _, sh := range l.physShards {
		for relPath, e := range sh.entries {
			if e.Status != StatusErrored && e.Status != StatusForbidden {
				continue
			}

			out = append(out, Failure{
				RelPath: relPath,
				Error:   e.LastError,
				Status:  e.Status,
			})
		}
	}

	slices.SortFunc(out, func(a, b Failure) int {
		return cmp.Or(
			cmp.Compare(failureRank(a.Status), failureRank(b.Status)),
			strings.Compare(a.RelPath, b.RelPath),
		)
	})

	return out
}

// failureRank orders failure statuses for [Ledger.Failures]: errored entries
// sort before forbidden ones. Ranking each element keeps the comparator free
// of order-dependent branches; out is collected from map iteration, so a
// branchy comparator would have run-to-run nondeterministic code coverage.
func failureRank(s Status) int {
	if s == StatusErrored {
		return 0
	}

	return 1
}

// DoneEntriesUnder returns a copy of every entry recorded [StatusDone] whose
// relpath begins with prefix, keyed by relpath. The remote sweep enumerates
// the proven configuration-version tarballs through it: an evicted tarball
// leaves no local file, so the ledger entry is the only local record that a
// remote copy must exist, and the sweep verifies the store still answers for
// each one.
func (l *Ledger) DoneEntriesUnder(prefix string) map[string]Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make(map[string]Entry)

	for _, sh := range l.physShards {
		for relPath, e := range sh.entries {
			if e.Status == StatusDone && strings.HasPrefix(relPath, prefix) {
				out[relPath] = cloneEntry(*e)
			}
		}
	}

	return out
}

// ErroredUnder returns the relative paths of the errored entries under prefix,
// sorted so a caller's sweep is deterministic. It answers a collector asking
// which of a family of derived names still awaits a retry, so it can settle
// the ones whose upstream source no longer exists (see [Ledger.RecordAbsent]).
func (l *Ledger) ErroredUnder(prefix string) []string {
	l.mu.RLock()

	var out []string

	for _, sh := range l.physShards {
		for relPath, e := range sh.entries {
			if e.Status == StatusErrored && strings.HasPrefix(relPath, prefix) {
				out = append(out, relPath)
			}
		}
	}

	l.mu.RUnlock()

	slices.Sort(out)

	return out
}

// StartRun opens a new run: it advances the run count and start time and resets
// the per-run tally so counts and bytes reflect only the new run. It records
// whether the run resumed prior work, and leaves the cumulative tally intact so
// a resumed run's reported counts carry the prior run's settled objects.
func (l *Ledger) StartRun() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.rootShard.runCount++
	l.rootShard.lastRunAt = now
	l.rootShard.runDirty = true
	l.runStartedAt = now
	l.resumed = l.totalEntries() > 0

	// The same reading per shard: a shard created later in the run keeps its
	// zero, so [Ledger.ResumedUnder] answers for the state the run opened with.
	for _, sh := range l.physShards {
		sh.resumed = len(sh.entries) > 0
	}

	l.counts = make(map[Status]int)
	l.dropped = make(map[string]string)
	l.bytes = 0
	l.retried = 0
	l.target = ""

	for _, sh := range l.physShards {
		for _, e := range sh.entries {
			e.counted = false
		}
	}
}

// ResumedUnder reports whether the shard owning relPath held any entry when
// the current run started (see [Ledger.StartRun]). It is the shard-scoped
// reading of [Tally]'s Resumed: one surviving entry anywhere in the
// organization proves nothing about a subtree whose own ledger directory was
// lost, so a caller weighing a destructive act against one subtree's records
// asks its shard rather than the organization. A path whose shard does not
// exist reports false.
func (l *Ledger) ResumedUnder(relPath string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(relPath)

	return ok && sh.resumed
}

// FinishRun closes the current run, writing its summary into the org-root shard
// and returning a copy for the final progress line. It marks the run for
// compaction, so a finished run leaves each touched shard with a current
// snapshot and no leftover log.
func (l *Ledger) FinishRun() RunRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	totals := make(map[Status]int, len(l.counts))

	// Exclude the reference-gate proxy statuses, mirroring [Ledger.Tally]'s
	// object-only selection: a gate counts a ledger-only proxy, not an archived
	// object, so a run's persisted Totals must not report it as work done.
	for s, n := range l.counts {
		if n != 0 && !s.IsGate() {
			totals[s] = n
		}
	}

	l.rootShard.lastRun = &RunRecord{
		StartedAt:       l.runStartedAt,
		FinishedAt:      l.now(),
		Totals:          totals,
		BytesDownloaded: l.bytes,
	}
	l.rootShard.runDirty = true
	l.compactNext = true

	return cloneRunRecord(*l.rootShard.lastRun)
}

// RunCount returns the number of runs recorded, including one opened by
// [Ledger.StartRun] but not yet finished.
func (l *Ledger) RunCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.rootShard.runCount
}

// LastRunAt returns the start time of the most recent run, or the zero time
// when none has started.
func (l *Ledger) LastRunAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.rootShard.lastRunAt
}

// LastRun returns a copy of the most recent run summary and whether one exists.
func (l *Ledger) LastRun() (RunRecord, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.rootShard.lastRun == nil {
		return RunRecord{}, false
	}

	return cloneRunRecord(*l.rootShard.lastRun), true
}

// Flush persists the state recorded since the last flush durably, so a hard kill
// loses at most that batch.
//
// Every changed shard's delta merges into one batch appended to the single
// org-level log with one fsync, then the log folds back into the shards'
// snapshots when it has outgrown the threshold or a run has just finished. An
// append-only flush costs the recent delta rather than the whole ledger.
// Flushes are serialized, so the log takes one writer at a time.
//
// One log is one fsync domain, so a hard kill mid-flush persists a
// complete-line prefix of the whole batch (see [replayLog]) and the batch's
// class order (see [walRecordClass]) is a total durability order across every
// shard: a guard record is durable before any skip signal it stands behind,
// and a cross-shard proof (a config-version tarball's done entry) is durable
// before the collection settlement that could freeze its referencing run,
// because entries always precede settlements. There is no shard-to-shard
// append order left to reason about, and nothing to tear between.
func (l *Ledger) Flush() error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()

	type pending struct {
		sh *shard
		d  drainedState
	}

	var (
		work []pending
		recs []walRecord
	)

	// Sorted iteration makes the batch layout deterministic: a shard reachable
	// under several alias keys drains on its first key (the drain empties it,
	// so later aliases see nothing dirty), and any of its tags routes back to
	// the same physical shard on replay (see [Ledger.shardAt]).
	for _, sk := range slices.Sorted(maps.Keys(l.shards)) {
		sh := l.shards[sk]
		if !sh.hasDirty() {
			continue
		}

		d := sh.drainDirty()

		for i := range d.recs {
			d.recs[i].Shard = sk
		}

		work = append(work, pending{sh: sh, d: d})
		recs = append(recs, d.recs...)
	}

	compactAll := l.compactNext
	l.compactNext = false

	l.mu.Unlock()

	// Merging the shards' drains re-establishes the class order across the
	// whole batch; the sort is stable, so each shard's within-class ordering
	// (sorted keys) is preserved and the batch layout stays deterministic.
	slices.SortStableFunc(recs, func(a, b walRecord) int {
		return walRecordClass(&a) - walRecordClass(&b)
	})

	if len(recs) > 0 {
		n, err := appendLog(l.walPath(), recs)
		if err != nil {
			// The drained records never reached the log; put them back so a
			// retry re-appends them rather than dropping the delta. Restore a
			// pending compaction request too, so a finished run still folds on
			// the retry rather than leaving the log unfolded.
			l.mu.Lock()

			for _, w := range work {
				w.sh.restoreDirty(w.d)
			}

			if compactAll {
				l.compactNext = true
			}

			l.mu.Unlock()

			return fmt.Errorf("append ledger log: %w", err)
		}

		l.mu.Lock()

		l.walBytes += n

		for _, w := range work {
			w.sh.stale = true
			w.sh.ackDrained(w.d)
		}

		l.mu.Unlock()
	}

	if compactAll || l.walBytes > l.compactThreshold {
		err := l.fold(compactAll)
		if err != nil {
			return err
		}
	}

	return nil
}

// fold captures every stale shard's state in its snapshot and, once no shard
// lags the log, resets the log, so the snapshots alone reflect the ledger.
//
// Snapshots are written before the log is removed, and the log is removed only
// when every stale shard folded: replaying the full log over snapshots that
// already account for it is idempotent (records are last-writer-wins upserts),
// so a crash (or a shard skipped for pending dirty state) between the two
// steps costs a redundant replay, never a lost or regressed record.
func (l *Ledger) fold(compactAll bool) error {
	l.mu.RLock()

	var stale []*shard

	for _, sh := range l.physShards {
		if sh.stale {
			stale = append(stale, sh)
		}
	}

	l.mu.RUnlock()

	var errs []error

	for _, sh := range stale {
		err := l.compactShard(sh)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		// A finished run's compact-all request was consumed by the caller; a
		// shard that failed to fold would otherwise drop it and keep the log
		// until it independently outgrew the threshold. Restore the request so
		// a later flush retries the fold. The other shards were still
		// attempted, so one shard's failure does not strand the rest.
		if compactAll {
			l.mu.Lock()

			l.compactNext = true
			l.mu.Unlock()
		}

		return fmt.Errorf("compact shards: %w", errors.Join(errs...))
	}

	l.mu.RLock()

	clean := true

	for _, sh := range l.physShards {
		if sh.stale {
			clean = false

			break
		}
	}

	l.mu.RUnlock()

	if !clean {
		// A shard re-dirtied mid-fold was skipped; its logged records are not
		// yet in a snapshot, so the log stays until a later flush folds it.
		if compactAll {
			l.mu.Lock()

			l.compactNext = true
			l.mu.Unlock()
		}

		return nil
	}

	// The unlink must be durable: Load discards a deleted subtree's records
	// while replaying this log, and a resurrected log would replay them over
	// the subtree a later run re-creates (see the replay notes on Load). A
	// plain remove leaves the unlink in the page cache until the next append
	// happens to sync the directory.
	err := atomicfile.Remove(l.walPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("truncate ledger log: %w", err)
	}

	l.mu.Lock()

	l.walBytes = 0
	l.mu.Unlock()

	return nil
}

// compactShard captures one shard's replayed and logged state in its snapshot
// via an atomic temp-and-rename, so the snapshot alone reflects the shard.
//
// It folds only a shard with no pending dirty state, so the snapshot it writes
// reflects exactly what the log already holds. A shard re-dirtied since the
// last drain (a worker recorded during the flush) is skipped and folded on a
// later flush, once its newest state has reached the log; without that guard
// the snapshot could capture an in-memory status the log does not yet carry,
// and a crash before the log reset would replay the stale log record over the
// newer snapshot. Such a skip leaves the shard stale, which also holds the
// org-level log in place (see [Ledger.fold]). Building the detached snapshot
// copy holds a read lock, briefly blocking recording workers, but the marshal
// that follows runs unlocked, so a large shard's encode does not stall writers
// on other shards for its duration.
func (l *Ledger) compactShard(sh *shard) error {
	l.mu.RLock()

	if sh.hasDirty() {
		l.mu.RUnlock()

		return nil
	}

	doc := sh.document()

	l.mu.RUnlock()

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal shard %q: %w", sh.dir, err)
	}

	err = atomicfile.WriteFile(sh.snapshotPath(), data)
	if err != nil {
		return fmt.Errorf("write shard %q: %w", sh.dir, err)
	}

	l.mu.Lock()

	sh.stale = false
	l.mu.Unlock()

	return nil
}

// copyStatusCounts returns a shallow copy of a per-status count map, or nil.
func copyStatusCounts(src map[Status]int) map[Status]int {
	if src == nil {
		return nil
	}

	dst := make(map[Status]int, len(src))
	maps.Copy(dst, src)

	return dst
}
