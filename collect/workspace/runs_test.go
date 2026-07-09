package workspace_test

import (
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"

	"github.com/MacroPower/tfc_archiver/collect/workspace"
)

func TestRunTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status tfe.RunStatus
		want   bool
	}{
		"applied is terminal":               {status: tfe.RunApplied, want: true},
		"planned and finished is terminal":  {status: tfe.RunPlannedAndFinished, want: true},
		"discarded is terminal":             {status: tfe.RunDiscarded, want: true},
		"errored is terminal":               {status: tfe.RunErrored, want: true},
		"canceled is terminal":              {status: tfe.RunCanceled, want: true},
		"policy soft failed is terminal":    {status: tfe.RunPolicySoftFailed, want: true},
		"force canceled is terminal":        {status: tfe.RunStatus("force_canceled"), want: true},
		"pending is not terminal":           {status: tfe.RunPending, want: false},
		"planning is not terminal":          {status: tfe.RunPlanning, want: false},
		"planned is not terminal":           {status: tfe.RunPlanned, want: false},
		"applying is not terminal":          {status: tfe.RunApplying, want: false},
		"planned and saved is not terminal": {status: tfe.RunPlannedAndSaved, want: false},
		"confirmed is not terminal":         {status: tfe.RunConfirmed, want: false},
		"empty status is not terminal":      {status: tfe.RunStatus(""), want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, workspace.RunTerminal(tc.status))
		})
	}
}

func TestHasNextPage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		page *tfe.Pagination
		want bool
	}{
		"nil pagination has no next":    {page: nil, want: false},
		"zero next page has no next":    {page: &tfe.Pagination{NextPage: 0}, want: false},
		"positive next page has a next": {page: &tfe.Pagination{NextPage: 2}, want: true},
		"current before total has next": {page: &tfe.Pagination{NextPage: 5}, want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, workspace.HasNextPage(tc.page))
		})
	}
}
