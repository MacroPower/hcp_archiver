package archiver

// Test hooks exposing unexported helpers to the external test package.
var (
	// ResolveOrgs exposes resolveOrgs for tests.
	ResolveOrgs = resolveOrgs
	// ProjectNameFor exposes projectNameFor for tests.
	ProjectNameFor = projectNameFor
	// LogFailures exposes (*Archiver).logFailures for tests.
	LogFailures = (*Archiver).logFailures
	// OrgIncomplete exposes orgIncomplete for tests.
	OrgIncomplete = orgIncomplete
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
