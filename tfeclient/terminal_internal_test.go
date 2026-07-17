package tfeclient

import (
	"go/constant"
	"go/types"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	tfe "github.com/hashicorp/go-tfe"
)

// TestTerminalityTablesAreComplete fails when a go-tfe upgrade introduces a
// status constant no table mentions, forcing a deliberate terminal/live
// decision instead of a silent non-terminal default: the default is the safe
// direction, but an undecided status left there forever means the object is
// re-fetched on every run and its collection never settles.
func TestTerminalityTablesAreComplete(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{Mode: packages.NeedTypes}

	pkgs, err := packages.Load(cfg, "github.com/hashicorp/go-tfe")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors)

	scope := pkgs[0].Types.Scope()

	tables := map[string]func(value string) bool{
		"RunStatus": func(v string) bool {
			_, ok := runTerminality[tfe.RunStatus(v)]

			return ok
		},
		"StateVersionStatus": func(v string) bool {
			_, ok := stateVersionTerminality[tfe.StateVersionStatus(v)]

			return ok
		},
		"StackConfigurationStatus": func(v string) bool {
			_, ok := stackConfigurationTerminality[tfe.StackConfigurationStatus(v)]

			return ok
		},
		"DeploymentRunStatus": func(v string) bool {
			_, ok := deploymentRunTerminality[tfe.DeploymentRunStatus(v)]

			return ok
		},
	}

	seen := map[string]int{}

	for _, name := range scope.Names() {
		obj := scope.Lookup(name)

		c, ok := obj.(*types.Const)
		if !ok {
			continue
		}

		named, ok := c.Type().(*types.Named)
		if !ok {
			continue
		}

		mentioned, tracked := tables[named.Obj().Name()]
		if !tracked {
			continue
		}

		seen[named.Obj().Name()]++

		value := constant.StringVal(c.Val())
		assert.True(t, mentioned(value),
			"go-tfe defines %s %s = %q, which no terminality table mentions: decide whether it is terminal",
			named.Obj().Name(), name, value)
	}

	for typeName := range tables {
		assert.Positive(t, seen[typeName],
			"no %s constants were found; the completeness sweep is no longer scanning anything", typeName)
	}
}

func TestTerminalPolarity(t *testing.T) {
	t.Parallel()

	// The tables' shared polarity: unknown and empty statuses read as live,
	// except the state-version empty status, which predates the attribute.
	assert.False(t, RunTerminal(tfe.RunStatus("surprise")))
	assert.False(t, RunTerminal(tfe.RunStatus("")))
	assert.False(t, StackConfigurationTerminal(tfe.StackConfigurationStatus("surprise")))
	assert.False(t, DeploymentRunTerminal(tfe.DeploymentRunStatus("surprise")))
	assert.False(t, StateVersionTerminal(tfe.StateVersionStatus("surprise")))
	assert.True(t, StateVersionTerminal(tfe.StateVersionStatus("")),
		"a server that predates the status attribute has no pending versions")

	// The documented spot decisions that motivated the tables.
	assert.True(t, RunTerminal(tfe.RunPolicySoftFailed), "no override advances a soft-failed plan-only run")
	assert.False(t, RunTerminal(tfe.RunPolicyOverride), "a user override moves an overridable run on")
	assert.True(t, RunTerminal(tfe.RunStatus("force_canceled")), "documented final state with no SDK constant")
	assert.False(t, DeploymentRunTerminal(tfe.DeploymentRunStatusDeployingPendingOperator),
		"an operator-gated run can sit for days and still advance")
}
