package collect

import "go.jacobcolvin.com/hcp_archiver/manifest"

// PageSlotState classifies what [Env.Object] will do with a resumable walk's
// page slot, so a walk deciding whether a slot's contribution comes from a
// fresh write or a read-back of the stored file asks the ledger once instead
// of re-deriving its settlement rules.
type PageSlotState int

const (
	// PageSlotMustWrite marks a slot with no settled entry: the walk's write
	// goes through, so the freshly listed events are the slot's contribution.
	PageSlotMustWrite PageSlotState = iota
	// PageSlotShortCircuited marks a settled slot whose write [Env.Object]
	// skips: the stored file is the record, so the slot's contribution must be
	// read back from it rather than taken from a re-list the write never
	// persisted.
	PageSlotShortCircuited
	// PageSlotSettledAbsent marks a settled absence with no file behind it,
	// contributing nothing and never read back. It is reachable only on a
	// ledger opened without the migration that unsettles such slots (a
	// pre-v0.4 record; see the audit collector's LedgerMigration), kept as a
	// defensive backstop.
	PageSlotSettledAbsent
)

// PageSlot classifies the resumable-walk page slot at relPath (see
// [PageSlotState]).
func (e *Env) PageSlot(relPath string) PageSlotState {
	entry, ok := e.Entry(relPath)
	if !ok || e.ShouldFetch(relPath) {
		return PageSlotMustWrite
	}

	if entry.Status == manifest.StatusAbsent {
		return PageSlotSettledAbsent
	}

	return PageSlotShortCircuited
}
