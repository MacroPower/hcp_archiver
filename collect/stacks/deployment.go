package stacks

import (
	"context"
	"fmt"
	"io"

	tfe "github.com/hashicorp/go-tfe"

	"github.com/MacroPower/tfc_archiver/tfeclient"
)

// collectDeployments archives every named deployment of a stack. A named
// deployment exposes only its metadata and latest-run reference (the API has no
// full run list for it), so each is refreshed as mutable metadata.
func (c *Collector) collectDeployments(ctx context.Context, project string, stack *tfe.Stack) error {
	deployments, err := tfeclient.Paginate(ctx, c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.StackDeployment, *tfe.Pagination, error) {
			l, e := tc.StackDeployments.List(ctx, stack.ID, &tfe.StackDeploymentListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stack deployments: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return wrap(err)
	}

	for _, deployment := range deployments {
		depFile := c.env.Store().StackDeploymentFile(project, stack.Name, deployment.Name, "deployment.json")

		err = c.env.Mutable(ctx, depFile, func(_ context.Context) (any, error) {
			return deployment, nil
		})
		if err != nil {
			return wrap(err)
		}
	}

	return nil
}

// collectStates archives every stack state generation. The archived file is the
// full state description; new generations append and existing ones are never
// re-fetched.
func (c *Collector) collectStates(ctx context.Context, project string, stack *tfe.Stack) error {
	states, err := tfeclient.Paginate(ctx, c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.StackState, *tfe.Pagination, error) {
			l, e := tc.StackStates.List(ctx, stack.ID, &tfe.StackStateListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stack states: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return wrap(err)
	}

	for _, state := range states {
		stateFile := c.env.Store().StackStateFile(project, stack.Name, generationName(state.Generation))

		err = c.env.Blob(ctx, stateFile, func(ctx context.Context) (io.Reader, error) {
			return c.stateDescription(ctx, state.ID)
		})
		if err != nil {
			return wrap(err)
		}
	}

	return nil
}

// stateDescription opens the full description of a stack state for streaming to
// disk.
func (c *Collector) stateDescription(ctx context.Context, stateID string) (io.Reader, error) {
	var r io.ReadCloser

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		r, e = tc.StackStates.Description(ctx, stateID)
		if e != nil {
			return fmt.Errorf("read stack state description: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}

	return r, nil
}
