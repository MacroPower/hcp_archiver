package collect

// Test hooks exposing unexported helpers to the external test package.
var (
	// EagerScope exposes eagerScope for tests.
	EagerScope = eagerScope
	// UnderWorkspaceSubtree exposes underWorkspaceSubtree for tests.
	UnderWorkspaceSubtree = underWorkspaceSubtree
)
