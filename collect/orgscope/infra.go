package orgscope

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe/v2"
)

// collectRunTasks archives the organization run-task definitions, stored
// exactly as the API returns them.
func (c *Collector) collectRunTasks(ctx context.Context) error {
	return archiveList(ctx, c, c.env.Store().RunTasks(), "run tasks",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.RunTask, *tfe.Pagination, error) {
			l, e := tc.RunTasks.List(ctx, c.org, &tfe.RunTaskListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list run tasks: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectAgentPools archives each agent pool with its allowed and excluded
// workspaces and allowed projects, serialized as ids.
func (c *Collector) collectAgentPools(ctx context.Context) error {
	return enumerate(ctx, c, "agent pools",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.AgentPool, *tfe.Pagination, error) {
			l, e := tc.AgentPools.List(ctx, c.org, &tfe.AgentPoolListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list agent pools: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		func(ctx context.Context, pool *tfe.AgentPool) error {
			return c.mutableValue(ctx, c.env.Store().AgentPool(pool.ID), pool)
		},
	)
}

// collectTokenTTLPolicies archives the organization's per-token-type max-TTL
// governance.
func (c *Collector) collectTokenTTLPolicies(ctx context.Context) error {
	return archiveList(ctx, c, c.env.Store().TokenTTLPolicies(), "token ttl policies",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.OrganizationTokenTTLPolicy, *tfe.Pagination, error) {
			l, e := tc.OrganizationTokenTTLPolicies.List(ctx, c.org, &tfe.OrganizationTokenTTLPolicyListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list token ttl policies: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectReservedTagKeys archives the organization's reserved tag-key
// governance.
func (c *Collector) collectReservedTagKeys(ctx context.Context) error {
	return archiveList(ctx, c, c.env.Store().ReservedTagKeys(), "reserved tag keys",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.ReservedTagKey, *tfe.Pagination, error) {
			l, e := tc.ReservedTagKeys.List(ctx, c.org, &tfe.ReservedTagKeyListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list reserved tag keys: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectHYOKConfigurations archives each hold-your-own-key configuration with
// its OIDC configuration and customer key versions, only when the scope toggle
// is on.
func (c *Collector) collectHYOKConfigurations(ctx context.Context) error {
	if !c.hyok {
		return nil
	}

	return enumerate(ctx, c, "hyok configurations",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.HYOKConfiguration, *tfe.Pagination, error) {
			l, e := tc.HYOKConfigurations.List(ctx, c.org, &tfe.HYOKConfigurationsListOptions{
				ListOptions: o,
				Include: []tfe.HYOKConfigurationsIncludeOpt{
					tfe.HYOKConfigurationsIncludeOIDCConfiguration,
					tfe.HYOKConfigurationsIncludeHYOKCustomerKeyVersions,
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list hyok configurations: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		c.archiveHYOKConfiguration,
	)
}

// archiveHYOKConfiguration archives one HYOK configuration, its OIDC
// configuration, and its customer key versions, each already hydrated on the
// listed configuration.
//
// The parent renders its OIDC and key-version relations as bare id refs, so each
// sub-object is archived directly as its own primary object to keep the
// sideloaded attributes the include already paid for.
func (c *Collector) archiveHYOKConfiguration(ctx context.Context, config *tfe.HYOKConfiguration) error {
	st := c.env.Store()

	err := c.mutableValue(ctx, st.HYOKConfigurationFile(config.ID, "hyok-configuration.json"), config)
	if err != nil {
		return err
	}

	// A HYOK configuration always has exactly one OIDC configuration; an all-nil
	// choice means it was not hydrated this pass, a transient state HYOK's
	// Mutable re-enumeration retries next run. Skip it without a ledger record
	// rather than settling a not-applicable gap that would never be filled.
	oidc := oidcConcrete(config.OIDCConfiguration)
	if oidc != nil {
		err = c.mutableValue(ctx, st.HYOKConfigurationFile(config.ID, "oidc-configuration.json"), oidc)
		if err != nil {
			return err
		}
	}

	for _, kv := range config.KeyVersions {
		err = c.mutableValue(ctx, st.HYOKKeyVersionFile(config.ID, kv.ID), kv)
		if err != nil {
			return err
		}
	}

	return nil
}

// oidcConcrete unwraps a HYOK OIDC polymorphic choice to its single non-nil
// concrete configuration (one of AWS, GCP, Azure, or Vault), or nil when the
// choice is unset. Each concrete type carries its own jsonapi primary tag, so
// archiving it directly keeps its attributes rather than a bare id ref.
func oidcConcrete(c *tfe.OIDCConfigurationTypeChoice) any {
	switch {
	case c == nil:
		return nil
	case c.AWSOIDCConfiguration != nil:
		return c.AWSOIDCConfiguration
	case c.GCPOIDCConfiguration != nil:
		return c.GCPOIDCConfiguration
	case c.AzureOIDCConfiguration != nil:
		return c.AzureOIDCConfiguration
	case c.VaultOIDCConfiguration != nil:
		return c.VaultOIDCConfiguration
	default:
		return nil
	}
}
