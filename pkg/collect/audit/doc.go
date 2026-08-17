// Package audit archives an organization's audit surface, which requires an
// elevated token (an organization owner or an audit token) and is gathered
// only when its scope toggle is on.
//
// It captures the organization's audit configuration (whether and how auditing
// is enabled) and then the audit-trail entries. It walks their windowed pages
// forward from the recorded point in time and keys resume on that time cursor.
// The trail covers only HCP's retention window, so older activity is
// unrecoverable regardless of when the archive runs.
//
// These entries are plain JSON rather than jsonapi objects and page by a time
// cursor rather than by the usual page number, so the package follows a distinct
// serialization and pagination path from the rest of the organization surface.
package audit
