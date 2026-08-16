package audit

import (
	"path"
	"regexp"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// pageNamePattern matches the leaf name pageName builds: a fixed-width UTC
// cursor stamp, the zero-padded page number, and the .json extension.
var pageNamePattern = regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}Z-p\d{9}\.json$`)

// isPageName reports whether leaf names an audit-trail page slot (see
// pageName). The audit-trail directory also holds the organization audit
// configuration, which is not a page slot.
func isPageName(leaf string) bool {
	return pageNamePattern.MatchString(leaf)
}

// LedgerMigration returns the [manifest.Migration] repairing audit-trail page
// slots a pre-v0.4 release settled absent by routing a terminal list result
// through [collect.Env.Object]. Pass it to [manifest.Load] via
// [manifest.WithMigrations].
//
// The rewrite is unconditional for a matching slot because it is safe by
// construction: current releases never record a page slot absent (a list
// result that cannot become a page routes through the dropped-surface record
// instead), no file was ever written for such a slot, and the halt paths of
// the wedged walk never advanced the watermark, so the next walk regenerates
// the same page name and writes the slot fresh. Unsettling it to errored
// costs at most one re-list. A slot the re-list no longer feeds stays
// errored, a visible record of the never-written page rather than a silent
// gap.
//
// Only page slots match: the audit configuration lives in the same directory
// and is a mutable object that can legitimately settle absent (an
// unentitled organization answers 404), which the migration must not churn.
func LedgerMigration(st *store.Store) manifest.Migration {
	prefix := st.AuditTrailDir() + "/"

	return func(relPath string, _ int, e manifest.Entry) (manifest.Entry, bool) {
		if e.Status != manifest.StatusAbsent {
			return e, false
		}

		if !strings.HasPrefix(relPath, prefix) || !isPageName(path.Base(relPath)) {
			return e, false
		}

		e.Status = manifest.StatusErrored
		e.Transient = false
		e.LastError = "page slot settled absent by a pre-v0.4 release; no file was written"

		return e, true
	}
}
