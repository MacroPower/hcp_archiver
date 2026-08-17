package orgscope

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
)

// variableSetScopeInclude hydrates the workspace and project scopes a variable
// set applies to, so those relations serialize with their ids.
const variableSetScopeInclude = string(tfe.VariableSetWorkspaces) + "," + string(tfe.VariableSetProjects)

// collectVariableSets archives each variable set with its variables, stored
// exactly as the API returns them (a sensitive value reads back empty from the
// API; nothing is redacted locally).
func (c *Collector) collectVariableSets(ctx context.Context) error {
	return enumerate(ctx, c, "variable sets",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.VariableSet, *tfe.Pagination, error) {
			l, e := tc.VariableSets.List(ctx, c.org, &tfe.VariableSetListOptions{
				ListOptions: o,
				Include:     variableSetScopeInclude,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list variable sets: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		c.archiveVariableSet,
	)
}

// archiveVariableSet archives one variable set's definition and its variables.
func (c *Collector) archiveVariableSet(ctx context.Context, set *tfe.VariableSet) error {
	err := c.mutableValue(ctx, c.env.Store().VariableSetFile(set.ID, "variable-set.json"), set)
	if err != nil {
		return err
	}

	return c.collectVariableSetVariables(ctx, set.ID)
}

// collectVariableSetVariables archives the variables of one variable set.
func (c *Collector) collectVariableSetVariables(ctx context.Context, setID string) error {
	relPath := c.env.Store().VariableSetFile(setID, "variables.json")

	return archiveList(ctx, c, relPath, "variable set variables",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.VariableSetVariable, *tfe.Pagination, error) {
			l, e := tc.VariableSetVariables.List(ctx, setID, &tfe.VariableSetVariableListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list variable set variables: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectPolicySets archives each policy set with its current and newest
// version metadata and its parameters.
func (c *Collector) collectPolicySets(ctx context.Context) error {
	return enumerate(ctx, c, "policy sets",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.PolicySet, *tfe.Pagination, error) {
			l, e := tc.PolicySets.List(ctx, c.org, &tfe.PolicySetListOptions{
				ListOptions: o,
				Include: []tfe.PolicySetIncludeOpt{
					tfe.PolicySetCurrentVersion,
					tfe.PolicySetNewestVersion,
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list policy sets: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		c.archivePolicySet,
	)
}

// archivePolicySet archives one policy set's definition, its current and newest
// version metadata, and its parameters.
func (c *Collector) archivePolicySet(ctx context.Context, set *tfe.PolicySet) error {
	err := c.mutableValue(ctx, c.env.Store().PolicySetFile(set.ID, "policy-set.json"), set)
	if err != nil {
		return err
	}

	err = c.archivePolicySetVersions(ctx, set)
	if err != nil {
		return err
	}

	return c.collectPolicySetParameters(ctx, set.ID)
}

// archivePolicySetVersions archives the hydrated current and newest version
// metadata already carried on the listed policy set.
//
// The parent renders both version relations as bare id refs, so each is archived
// directly as its own primary object to keep the sideloaded attributes. When the
// current and newest ids are equal the two files carry identical content; the
// role each names is the information worth keeping.
func (c *Collector) archivePolicySetVersions(ctx context.Context, set *tfe.PolicySet) error {
	st := c.env.Store()

	if set.CurrentVersion != nil {
		err := c.mutableValue(ctx, st.PolicySetFile(set.ID, "current-version.json"), set.CurrentVersion)
		if err != nil {
			return err
		}
	}

	if set.NewestVersion != nil {
		err := c.mutableValue(ctx, st.PolicySetFile(set.ID, "newest-version.json"), set.NewestVersion)
		if err != nil {
			return err
		}
	}

	return nil
}

// collectPolicySetParameters archives the parameters of one policy set,
// stored exactly as the API returns them.
func (c *Collector) collectPolicySetParameters(ctx context.Context, setID string) error {
	relPath := c.env.Store().PolicySetFile(setID, "parameters.json")

	return archiveList(ctx, c, relPath, "policy set parameters",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.PolicySetParameter, *tfe.Pagination, error) {
			l, e := tc.PolicySetParameters.List(ctx, setID, &tfe.PolicySetParameterListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list policy set parameters: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectPolicies archives each policy's metadata alongside its raw Sentinel or
// OPA source.
func (c *Collector) collectPolicies(ctx context.Context) error {
	return enumerate(ctx, c, "policies",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Policy, *tfe.Pagination, error) {
			l, e := tc.Policies.List(ctx, c.org, &tfe.PolicyListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list policies: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		c.archivePolicy,
	)
}

// archivePolicy archives one policy's metadata and its raw source.
func (c *Collector) archivePolicy(ctx context.Context, policy *tfe.Policy) error {
	err := c.mutableValue(ctx, c.env.Store().Policy(policy.ID, "json"), policy)
	if err != nil {
		return err
	}

	return c.collectPolicySource(ctx, policy)
}

// collectPolicySource archives the raw source of the policy's current revision
// as immutable bytes, under the name [Collector.policySourcePath] picks,
// stamping the entry with the revision's server-side updated-at so later runs
// baseline against it (see [collect.Env.RevisionPath]).
func (c *Collector) collectPolicySource(ctx context.Context, policy *tfe.Policy) error {
	relPath := c.policySourcePath(policy)

	err := c.env.Bytes(ctx, relPath, func(ctx context.Context) ([]byte, error) {
		var src []byte

		derr := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			var e error

			src, e = tc.Policies.Download(ctx, policy.ID)
			if e != nil {
				return fmt.Errorf("download policy source: %w", e)
			}

			return nil
		})
		if derr != nil {
			return nil, fmt.Errorf("policy source: %w", derr)
		}

		return src, nil
	}, collect.WithUpdatedAt(policy.UpdatedAt))
	if err != nil {
		return fmt.Errorf("archive policy source: %w", err)
	}

	return nil
}

// policySourcePath returns the path the policy's current source revision is
// archived under, picking the file extension from the policy kind.
//
// A policy's source is replaceable: an upload rewrites the content the same
// policy id downloads and moves the policy's updated-at, while the id and every
// other name it is known by stay put. One file per policy would therefore freeze
// the first revision an archive ever saw (the source is archived as immutable
// bytes, fetched once and never re-read) beside mutable metadata that keeps
// reporting the policy changed. Each revision instead earns a name of its own:
// the source keeps the plain <id>.<ext> name until an updated-at newer than the
// plain capture's recorded revision arrives, and every revision observed after
// that lands beside it as <id>.<updated-at>.<ext>. The stamps sort lexically,
// so the last name is the current source and no revision is overwritten by the
// one that replaced it.
//
// The choice between the names is [collect.Env.RevisionPath] over the
// server-side updated-at the plain capture stamped onto its ledger entry. Two
// uploads inside one second share a stamp, so the archive keeps whichever
// revision it reached first; the second is captured only once a later upload
// moves the stamp again.
func (c *Collector) policySourcePath(policy *tfe.Policy) string {
	st := c.env.Store()
	ext := policyExt(policy.Kind)

	// The revision stamp composes into the extension rather than into the id, so
	// the store stays the one place a policy id is sanitized into a path segment.
	return c.env.RevisionPath(
		st.Policy(policy.ID, ext),
		st.Policy(policy.ID, policyRevision(policy.UpdatedAt)+"."+ext),
		policy.UpdatedAt,
	)
}

// policyRevisionLayout formats a policy's updated-at into the stamp naming one
// revision of its source. It carries no colons and, paired with a literal "Z",
// denotes UTC, so the names stay filesystem-safe and sort by revision, the same
// shape the store stamps onto state-version filenames.
const policyRevisionLayout = "20060102T150405"

// policyRevision returns the stamp naming the source revision an updated-at
// identifies (see [policyRevisionLayout]).
func policyRevision(updatedAt time.Time) string {
	return updatedAt.UTC().Format(policyRevisionLayout) + "Z"
}

// policyExt returns the source-file extension for a policy of the given kind.
//
// Sentinel policies carry a .sentinel extension and OPA policies a .rego one; an
// unrecognized or unset kind falls back to .policy so the source is still
// captured under a stable name.
func policyExt(kind tfe.PolicyKind) string {
	switch kind {
	case tfe.Sentinel:
		return "sentinel"
	case tfe.OPA:
		return "rego"
	case tfe.TFPolicy:
		return "tf"
	default:
		return "policy"
	}
}
