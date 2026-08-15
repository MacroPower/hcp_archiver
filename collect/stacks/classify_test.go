package stacks_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	tfe "github.com/hashicorp/go-tfe/v2"

	"go.jacobcolvin.com/hcp_archiver/collect/stacks"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
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

			assert.Equal(t, tc.want, tfeclient.StackConfigurationTerminal(tc.status))
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

			assert.Equal(t, tc.want, tfeclient.DeploymentRunTerminal(tc.status))
		})
	}
}

func TestStateTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status string
		want   bool
	}{
		"completed is terminal": {status: "completed", want: true},
		"empty is terminal":     {status: "", want: true},
		"pending is live":       {status: "pending", want: false},
		"uploading is live":     {status: "uploading", want: false},
		"unknown is live":       {status: "surprise", want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, stacks.StateTerminalForTest(tc.status))
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

func TestArchivePrefixes(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())

	cfgFile := st.StackConfigurationFile("proj", "mystack", "cfg-1", "configuration.json")
	runFile := st.StackRunFile("proj", "mystack", "cfg-1", "grp-1", "run-1", "run.json")
	stepFile := st.StackStepFile("proj", "mystack", "cfg-1", "grp-1", "run-1", "step-1", "plan-description.json")

	// The configurations prefix is the real archive directory, a genuine path
	// prefix of every configuration entry down to a deeply nested run step.
	configPrefix := stacks.ConfigArchivePrefixForTest(st, "proj", "mystack")

	assert.Equal(t, "projects/proj/stacks/mystack/configurations", configPrefix)
	assert.True(t, strings.HasPrefix(cfgFile, configPrefix+"/"), "a config entry sits under the configurations prefix")
	assert.True(t, strings.HasPrefix(stepFile, configPrefix+"/"), "a nested step sits under the configurations prefix")

	// The runs prefix nests the group's run entries and their steps, so the
	// collection handle opened on it routes every flag to the stack shard.
	runPrefix := stacks.RunArchivePrefixForTest(st, "proj", "mystack", "cfg-1", "grp-1")

	assert.Equal(t, "projects/proj/stacks/mystack/configurations/cfg-1/deployment-groups/grp-1/runs", runPrefix)
	assert.True(t, strings.HasPrefix(runFile, runPrefix+"/"), "a run entry sits under the runs prefix")
	assert.True(t, strings.HasPrefix(stepFile, runPrefix+"/"), "a run's step sits under the runs prefix")
}
