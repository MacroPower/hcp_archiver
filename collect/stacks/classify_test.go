package stacks_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	tfe "github.com/hashicorp/go-tfe"

	"github.com/MacroPower/tfc_archiver/collect/stacks"
)

func TestConfigTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status tfe.StackConfigurationStatus
		want   bool
	}{
		"completed is terminal": {status: tfe.StackConfigurationStatusCompleted, want: true},
		"failed is terminal":    {status: tfe.StackConfigurationStatusFailed, want: true},
		"pending is live":       {status: tfe.StackConfigurationStatusPending, want: false},
		"queued is live":        {status: tfe.StackConfigurationStatusQueued, want: false},
		"preparing is live":     {status: tfe.StackConfigurationStatusPreparing, want: false},
		"unknown is live":       {status: tfe.StackConfigurationStatus("surprise"), want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, stacks.ConfigTerminalForTest(tc.status))
		})
	}
}

func TestRunTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status tfe.DeploymentRunStatus
		want   bool
	}{
		"succeeded is terminal":    {status: tfe.DeploymentRunStatusSucceeded, want: true},
		"failed is terminal":       {status: tfe.DeploymentRunStatusFailed, want: true},
		"abandoned is terminal":    {status: tfe.DeploymentRunStatusAbandoned, want: true},
		"pending is live":          {status: tfe.DeploymentRunStatusPending, want: false},
		"deploying is live":        {status: tfe.DeploymentRunStatusDeploying, want: false},
		"acquiring lock is live":   {status: tfe.DeploymentRunStatusAcquiringLock, want: false},
		"pre-deploying is live":    {status: tfe.DeploymentRunStatusPreDeploying, want: false},
		"pending operator is live": {status: tfe.DeploymentRunStatusDeployingPendingOperator, want: false},
		"unknown is live":          {status: tfe.DeploymentRunStatus("surprise"), want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, stacks.RunTerminalForTest(tc.status))
		})
	}
}

func TestGenerationName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want       string
		generation int
	}{
		"zero":     {generation: 0, want: "0"},
		"single":   {generation: 7, want: "7"},
		"multiple": {generation: 142, want: "142"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, stacks.GenerationNameForTest(tc.generation))
		})
	}
}

func TestCollectionKeys(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"stacks/st-abc/configurations",
		stacks.ConfigKeyForTest("st-abc"),
	)
	assert.Equal(t,
		"stack-deployment-groups/sdg-xyz/runs",
		stacks.RunKeyForTest("sdg-xyz"),
	)
	assert.NotEqual(t,
		stacks.ConfigKeyForTest("st-abc"),
		stacks.ConfigKeyForTest("st-def"),
		"distinct stacks must map to distinct configuration cursors",
	)
}
