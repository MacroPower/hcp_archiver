package archiver

import "go.jacobcolvin.com/hcp_archiver/remote"

// Test hooks exposing unexported helpers to the external test package.
var (
	// SyncOrg exposes (*Archiver).syncOrg for tests.
	SyncOrg = (*Archiver).syncOrg
	// WriteRemoteMarker exposes (*Archiver).writeRemoteMarker for tests.
	WriteRemoteMarker = (*Archiver).writeRemoteMarker
	// PromoteRemoteMarker exposes (*Archiver).promoteRemoteMarker for tests.
	PromoteRemoteMarker = (*Archiver).promoteRemoteMarker
	// ResolveOrgs exposes resolveOrgs for tests.
	ResolveOrgs = resolveOrgs
	// ProjectNameFor exposes projectNameFor for tests.
	ProjectNameFor = projectNameFor
	// LogFailures exposes (*Archiver).logFailures for tests.
	LogFailures = (*Archiver).logFailures
	// OrgIncomplete exposes orgIncomplete for tests.
	OrgIncomplete = orgIncomplete
	// RunOutcome exposes runOutcome for tests.
	RunOutcome = runOutcome
	// FilterProjects exposes filterProjects for tests.
	FilterProjects = filterProjects
	// FilterWorkspaces exposes filterWorkspaces for tests.
	FilterWorkspaces = filterWorkspaces
	// UnmatchedFilter exposes unmatchedFilter, instantiated for plain names, for
	// tests.
	UnmatchedFilter = unmatchedFilter[string]
)

// DefaultProjectName exposes defaultProjectName for tests.
const DefaultProjectName = defaultProjectName

// SetRemote injects the remote client [Archiver.Run] would normally build, so
// a test can drive the close sweep without AWS credentials.
func (a *Archiver) SetRemote(c *remote.Client) {
	a.remote = c
}
