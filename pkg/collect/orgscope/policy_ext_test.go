package orgscope_test

import (
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect/orgscope"
)

func TestPolicyExt(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		kind tfe.PolicyKind
		want string
	}{
		"sentinel": {
			kind: tfe.Sentinel,
			want: "sentinel",
		},
		"opa": {
			kind: tfe.OPA,
			want: "rego",
		},
		"tfpolicy": {
			kind: tfe.TFPolicy,
			want: "tf",
		},
		"empty falls back": {
			kind: tfe.PolicyKind(""),
			want: "policy",
		},
		"unknown falls back": {
			kind: tfe.PolicyKind("wasm"),
			want: "policy",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, orgscope.PolicyExt(tc.kind))
		})
	}
}
