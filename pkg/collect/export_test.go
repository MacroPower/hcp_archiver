package collect

// Test hooks exposing unexported helpers to the external test package.
var (
	// AliasRewrites exposes aliasRewrites for tests.
	AliasRewrites = aliasRewrites
	// EagerScope exposes eagerScope for tests.
	EagerScope = eagerScope
	// UnderWorkspaceSubtree exposes underWorkspaceSubtree for tests.
	UnderWorkspaceSubtree = underWorkspaceSubtree
)
