package registry

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// collectModules enumerates the organization's registry modules through the
// paginator and archives each one, hydrating the no-code configuration through
// the no-code-modules include.
func (c *Collector) collectModules(ctx context.Context) error {
	modules, err := tfeclient.Paginate(
		ctx,
		c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.RegistryModule, *tfe.Pagination, error) {
			list, e := tc.RegistryModules.List(ctx, c.org, &tfe.RegistryModuleListOptions{
				ListOptions: o,
				Include:     []tfe.RegistryModuleListIncludeOpt{tfe.IncludeNoCodeModules},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list registry modules: %w", e)
			}

			return list.Items, list.Pagination, nil
		},
	)
	if err != nil {
		return c.listFailed(ctx, "modules", err)
	}

	for _, mod := range modules {
		archiveErr := c.archiveModule(ctx, mod)
		if archiveErr != nil {
			return archiveErr
		}
	}

	return nil
}

// archiveModule writes a module's mutable metadata, its hydrated no-code
// modules, and, when detail is enabled and the module is private, its last
// commits and per-version metadata.
func (c *Collector) archiveModule(ctx context.Context, mod *tfe.RegistryModule) error {
	st := c.env.Store()
	path := st.RegistryModuleFile(mod.Namespace, mod.Name, mod.Provider, moduleFile)

	err := c.env.Mutable(ctx, path, func(_ context.Context) (any, error) {
		return mod, nil
	})
	if err != nil {
		return wrap("archive registry module", err)
	}

	for _, ncm := range mod.RegistryNoCodeModule {
		ncmErr := c.archiveNoCodeModule(ctx, mod, ncm)
		if ncmErr != nil {
			return ncmErr
		}
	}

	if !c.detail || mod.RegistryName == tfe.PublicRegistry {
		return nil
	}

	return c.archiveModuleDetail(ctx, mod)
}

// archiveNoCodeModule writes a no-code module's mutable metadata and, under
// detail, the beta per-version variable options for the version it resolves to.
func (c *Collector) archiveNoCodeModule(
	ctx context.Context,
	mod *tfe.RegistryModule,
	ncm *tfe.RegistryNoCodeModule,
) error {
	st := c.env.Store()
	base := st.RegistryNoCodeModule(ncm.ID)

	err := c.env.Mutable(ctx, base, func(_ context.Context) (any, error) {
		return ncm, nil
	})
	if err != nil {
		return wrap("archive no-code module", err)
	}

	if !c.detail {
		return nil
	}

	varsPath := noCodeVariablesPath(base)

	version := resolveNoCodeVersion(ncm.VersionPin, mod.VersionStatuses)
	if version == "" {
		c.env.Skip(varsPath)

		return nil
	}

	fetch := func(ctx context.Context) (any, error) {
		var out *tfe.RegistryModuleVariableList

		doErr := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			list, e := tc.RegistryNoCodeModules.ReadVariables(ctx, ncm.ID, version, nil)
			if e != nil {
				return fmt.Errorf("read no-code module variables: %w", e)
			}

			out = list

			return nil
		})

		return out, wrap("fetch no-code module variables", doErr)
	}

	// The resolved version tracks a movable pin ("latest" or an operator-changed
	// concrete pin), so the variable options are re-read and overwritten when
	// they change rather than frozen on first capture.
	return wrap("archive no-code module variables", c.env.Mutable(ctx, varsPath, fetch))
}

// archiveModuleDetail writes a private module's last commits and the frozen
// metadata of each version reported in its version statuses.
func (c *Collector) archiveModuleDetail(ctx context.Context, mod *tfe.RegistryModule) error {
	st := c.env.Store()
	id := tfe.NewPrivateRegistryModuleID(c.org, mod.Name, mod.Provider)
	commitsPath := st.RegistryModuleFile(mod.Namespace, mod.Name, mod.Provider, moduleCommitsFile)

	fetch := func(ctx context.Context) (any, error) {
		var out []*tfe.Commit

		doErr := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			list, e := tc.RegistryModules.ListCommits(ctx, id)
			if e != nil {
				return fmt.Errorf("list registry module commits: %w", e)
			}

			out = list.Items

			return nil
		})

		return out, wrap("fetch registry module commits", doErr)
	}

	err := wrap("archive registry module commits", c.env.Mutable(ctx, commitsPath, fetch))
	if err != nil {
		return err
	}

	for _, vs := range mod.VersionStatuses {
		// A non-concrete or empty version cannot address a per-version read and
		// would only record a permanent error; skip it, matching how the no-code
		// path resolves a concrete version before reading.
		if !isConcreteVersion(vs.Version) {
			continue
		}

		versionErr := c.archiveModuleVersion(ctx, mod, id, vs.Version)
		if versionErr != nil {
			return versionErr
		}
	}

	return nil
}

// archiveModuleVersion writes the frozen metadata of a single module version.
func (c *Collector) archiveModuleVersion(
	ctx context.Context,
	mod *tfe.RegistryModule,
	id tfe.RegistryModuleID,
	version string,
) error {
	st := c.env.Store()
	path := st.RegistryModuleFile(mod.Namespace, mod.Name, mod.Provider, moduleVersionFilename(version))

	fetch := func(ctx context.Context) (any, error) {
		var out *tfe.RegistryModuleVersion

		doErr := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			rmv, e := tc.RegistryModules.ReadVersion(ctx, id, version)
			if e != nil {
				return fmt.Errorf("read registry module version: %w", e)
			}

			out = rmv

			return nil
		})

		return out, wrap("fetch registry module version", doErr)
	}

	return wrap("archive registry module version", c.env.Object(ctx, path, fetch))
}
