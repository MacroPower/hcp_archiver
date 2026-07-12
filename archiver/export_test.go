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
)

// DefaultProjectName exposes defaultProjectName for tests.
const DefaultProjectName = defaultProjectName
