// Package namefilter owns the archive's allow-list semantics for named objects.
//
// The configuration narrows a run by naming the projects or workspaces to
// archive, and both lists mean the same thing: the listing is admitted whole
// when the operator names nothing, and narrowed to exactly the named entries
// otherwise. A zero configuration therefore archives everything visible, so a
// collector with no opinion about scope can carry a zero-value filter and never
// call the constructor at all.
//
// A filter matches on the same name the archive path is keyed on, not on an
// object id, so a caller holding only an id resolves it to a display name
// before asking. Resolution belongs to the caller because the collectors differ
// in how they get there: some carry a name from the listing that yielded the
// object, others read the parent to learn it. A caller that cannot resolve a
// name holds an id the filter can only reject, so the caller decides for itself
// whether that exclusion is worth reporting.
package namefilter
