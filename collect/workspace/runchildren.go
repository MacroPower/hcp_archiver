package workspace

import (
	"context"
	"fmt"
	"io"

	"github.com/hashicorp/go-tfe"
)

// archiveRunChildren archives every immutable child of a terminal run: its
// configuration version, plan and apply artifacts, cost estimate, comments, run
// events, policy checks, task stages, native Terraform policy outcomes, and the
// user that created it.
func (c *Collector) archiveRunChildren(ctx context.Context, project, ws string, run *tfe.Run) error {
	steps := []func(context.Context, string, string, *tfe.Run) error{
		c.archiveConfigurationVersion,
		c.archivePlan,
		c.archiveApply,
		c.archiveCostEstimate,
		c.archiveComments,
		c.archiveRunEvents,
		c.archivePolicyChecks,
		c.archiveTaskStages,
		c.archiveTFPolicyOutcomes,
	}

	for _, step := range steps {
		err := step(ctx, project, ws, run)
		if err != nil {
			return err
		}
	}

	// The run's created-by is hydrated by the pager but renders as a bare id ref;
	// archive that user directly (the only capture of who queued the run).
	return c.archiveUser(ctx, run.CreatedBy)
}

// archiveConfigurationVersion archives the run's configuration-version record,
// its ingress attributes, and, deduplicated org-wide by id, its tarball.
//
// The record's ingress relation is hydrated by the include but renders as a bare
// id ref, so the record is read once and split into config-version.json (kept
// byte-identical to before) and config-version-ingress.json (the commit sha,
// branch, and PR that survive even after the tarball expires). The read is gated
// on either derived file, so if the record settles but the ingress write errors a
// later pass still re-reads while the gap remains.
func (c *Collector) archiveConfigurationVersion(ctx context.Context, project, ws string, run *tfe.Run) error {
	if run.ConfigurationVersion == nil {
		return nil
	}

	st := c.env.Store()
	cvID := run.ConfigurationVersion.ID
	cvPath := st.RunFile(project, ws, run.ID, "config-version.json")
	ingPath := st.RunFile(project, ws, run.ID, "config-version-ingress.json")

	err := c.archiveConfigVersionRecord(ctx, cvPath, ingPath, cvID)
	if err != nil {
		return err
	}

	return c.blob(ctx, st.ConfigVersionTarball(cvID), func(ctx context.Context) (io.ReadCloser, error) {
		return c.env.Client().OpenConfigurationVersion(ctx, cvID)
	})
}

// archiveConfigVersionRecord reads the configuration version once, only while
// either derived file is still unsettled, and splits it into config-version.json
// and config-version-ingress.json.
//
// One read feeds both files, so a read failure is routed through each path's
// self-gating Object: a settled path is left untouched, an unsettled one records
// the error so a re-run retries.
func (c *Collector) archiveConfigVersionRecord(ctx context.Context, cvPath, ingPath, cvID string) error {
	if !c.env.ShouldFetch(cvPath) && !c.env.ShouldFetch(ingPath) {
		return nil
	}

	cv, err := doRead(ctx, c, cvPath,
		func(ctx context.Context, tc *tfe.Client) (*tfe.ConfigurationVersion, error) {
			return tc.ConfigurationVersions.ReadWithOptions(ctx, cvID, &tfe.ConfigurationVersionReadOptions{
				Include: []tfe.ConfigVerIncludeOpt{tfe.ConfigVerIngressAttributes},
			})
		})
	if err != nil {
		recErr := c.recordErrored(ctx, cvPath, err)
		if recErr != nil {
			return recErr
		}

		return c.recordErrored(ctx, ingPath, err)
	}

	err = c.object(ctx, cvPath, cv)
	if err != nil {
		return err
	}

	return c.archiveConfigVersionIngress(ctx, ingPath, cv)
}

// archiveConfigVersionIngress archives the hydrated ingress attributes at
// ingPath, or settles a not-applicable gap when the configuration version has
// none (a VCS-less upload), so a re-run neither refetches nor writes a null file.
func (c *Collector) archiveConfigVersionIngress(
	ctx context.Context,
	ingPath string,
	cv *tfe.ConfigurationVersion,
) error {
	if cv.IngressAttributes == nil {
		c.env.NotApplicable(ingPath)

		return nil
	}

	return c.object(ctx, ingPath, cv.IngressAttributes)
}

// archivePlan archives the run's plan summary, its plan log, and, when the
// Terraform version offers it, the structured plan JSON.
//
// The plan is already hydrated on the run by the pager's include but renders as a
// bare id ref on run.json, so its summary attributes (resource counts, change
// flags) are archived directly. The structured plan.json (the plans/{id}/json-output
// artifact) is streamed alongside; a run that produced none answers an empty body
// that settles a not-applicable gap.
func (c *Collector) archivePlan(ctx context.Context, project, ws string, run *tfe.Run) error {
	if run.Plan == nil {
		return nil
	}

	st := c.env.Store()
	planID := run.Plan.ID

	err := c.object(ctx, st.RunFile(project, ws, run.ID, "plan-summary.json"), run.Plan)
	if err != nil {
		return err
	}

	// The log streams whole from its signed URL rather than through the SDK's
	// chunked log reader, whose request-per-few-kilobytes pacing makes a large
	// finished log take minutes; its STX/ETX framing is trimmed on the fly (see
	// [tfeclient.Client.OpenPlanLog]).
	err = c.blob(ctx, st.RunFile(project, ws, run.ID, "plan.log"),
		func(ctx context.Context) (io.ReadCloser, error) {
			return c.env.Client().OpenPlanLog(ctx, planID)
		})
	if err != nil {
		return err
	}

	return c.blob(ctx, st.RunFile(project, ws, run.ID, "plan.json"),
		func(ctx context.Context) (io.ReadCloser, error) {
			return c.env.Client().OpenPlanJSON(ctx, planID)
		})
}

// archiveApply archives the run's apply summary and its apply log.
//
// Like the plan, the apply is hydrated on the run but renders as a bare id ref,
// so its summary attributes (resource counts) are archived directly alongside the
// log.
func (c *Collector) archiveApply(ctx context.Context, project, ws string, run *tfe.Run) error {
	if run.Apply == nil {
		return nil
	}

	st := c.env.Store()
	applyID := run.Apply.ID

	err := c.object(ctx, st.RunFile(project, ws, run.ID, "apply-summary.json"), run.Apply)
	if err != nil {
		return err
	}

	// Streams whole from its signed URL for the same reason as the plan log.
	return c.blob(ctx, st.RunFile(project, ws, run.ID, "apply.log"),
		func(ctx context.Context) (io.ReadCloser, error) {
			return c.env.Client().OpenApplyLog(ctx, applyID)
		})
}

// archiveCostEstimate archives the run's cost-estimate attributes and its
// human-readable log.
func (c *Collector) archiveCostEstimate(ctx context.Context, project, ws string, run *tfe.Run) error {
	if run.CostEstimate == nil {
		return nil
	}

	st := c.env.Store()
	estimate := run.CostEstimate
	estimateID := estimate.ID

	err := c.object(ctx, st.RunFile(project, ws, run.ID, "cost-estimate.json"), estimate)
	if err != nil {
		return err
	}

	return c.logBlob(ctx, st.RunFile(project, ws, run.ID, "cost-estimate.log"),
		func(ctx context.Context, tc *tfe.Client) (io.Reader, error) {
			return tc.CostEstimates.Logs(ctx, estimateID)
		})
}

// archiveComments archives the run's comments, which come from their own list
// endpoint because the run's comment relation is not sideloadable.
func (c *Collector) archiveComments(ctx context.Context, project, ws string, run *tfe.Run) error {
	runID := run.ID

	return objectOne(ctx, c, c.env.Store().RunFile(project, ws, run.ID, "comments.json"),
		func(ctx context.Context, tc *tfe.Client) ([]*tfe.Comment, error) {
			l, e := tc.Comments.List(ctx, runID)
			if e != nil {
				return nil, fmt.Errorf("list comments: %w", e)
			}

			return l.Items, nil
		})
}

// archiveRunEvents archives the run's actor-attributed events and each event's
// hydrated actor.
//
// The actor is hydrated by the include but renders as a bare id ref on the event,
// so it is archived directly at users/<id>.json. The events are captured from the
// list read so their actors can be looped after; a settled re-walk skips the read
// and leaves the slice empty, a harmless no-op since the actors were archived when
// the events were first written.
func (c *Collector) archiveRunEvents(ctx context.Context, project, ws string, run *tfe.Run) error {
	runID := run.ID

	var events []*tfe.RunEvent

	err := objectOne(ctx, c, c.env.Store().RunFile(project, ws, run.ID, "run-events.json"),
		func(ctx context.Context, tc *tfe.Client) ([]*tfe.RunEvent, error) {
			l, e := tc.RunEvents.List(ctx, runID, &tfe.RunEventListOptions{
				Include: []tfe.RunEventIncludeOpt{tfe.RunEventActor, tfe.RunEventComment},
			})
			if e != nil {
				return nil, fmt.Errorf("list run events: %w", e)
			}

			events = l.Items

			return l.Items, nil
		})
	if err != nil {
		return err
	}

	for _, ev := range events {
		uErr := c.archiveUser(ctx, ev.Actor)
		if uErr != nil {
			return uErr
		}
	}

	return nil
}

// archivePolicyChecks archives the run's Sentinel policy checks and a log per
// check.
func (c *Collector) archivePolicyChecks(ctx context.Context, project, ws string, run *tfe.Run) error {
	st := c.env.Store()
	relPath := st.RunFile(project, ws, run.ID, "policy-checks.json")
	runID := run.ID

	checks, err := paginateAll(ctx, c,
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.PolicyCheck, *tfe.Pagination, error) {
			l, e := tc.PolicyChecks.List(ctx, runID, &tfe.PolicyCheckListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list policy checks: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		// Record the list failure against the ledger like every other archived
		// object rather than aborting the run walk, so a re-run retries it and
		// one poisoned run never strands the older runs behind it. Only a
		// cancellation propagates.
		return c.recordErrored(ctx, relPath, err)
	}

	writeErr := c.object(ctx, relPath, checks)
	if writeErr != nil {
		return writeErr
	}

	for _, pc := range checks {
		checkID := pc.ID

		logErr := c.logBlob(ctx, st.RunFile(project, ws, run.ID, "policy-check-"+pc.ID+".log"),
			func(ctx context.Context, tc *tfe.Client) (io.Reader, error) {
				return tc.PolicyChecks.Logs(ctx, checkID)
			})
		if logErr != nil {
			return logErr
		}
	}

	return nil
}

// archiveTaskStages archives the run's task stages resolved into task results
// and policy evaluations.
func (c *Collector) archiveTaskStages(ctx context.Context, project, ws string, run *tfe.Run) error {
	runID := run.ID

	return listObject(ctx, c, c.env.Store().RunFile(project, ws, run.ID, "task-stages.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.TaskStage, *tfe.Pagination, error) {
			l, e := tc.TaskStages.List(ctx, runID, &tfe.TaskStageListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list task stages: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
}

// archiveTFPolicyOutcomes archives the native Terraform policy-evaluation
// metadata and, read per evaluation on the run, its aggregated set outcomes.
//
// The run include hydrates the evaluations, but the run renders them as bare id
// refs, so one run read is split into tf-policy-evaluations.json (the hydrated
// evaluations) and tf-policy-outcomes.json (the per-evaluation outcomes). The
// read is gated on either derived file. A run with no native policy evaluations
// settles both as not-applicable rather than writing an empty outcomes list.
func (c *Collector) archiveTFPolicyOutcomes(ctx context.Context, project, ws string, run *tfe.Run) error {
	st := c.env.Store()
	evalPath := st.RunFile(project, ws, run.ID, "tf-policy-evaluations.json")
	outPath := st.RunFile(project, ws, run.ID, "tf-policy-outcomes.json")
	runID := run.ID

	if !c.env.ShouldFetch(evalPath) && !c.env.ShouldFetch(outPath) {
		return nil
	}

	evaluated, err := doRead(ctx, c, outPath, func(ctx context.Context, tc *tfe.Client) (*tfe.Run, error) {
		return tc.Runs.ReadWithOptions(ctx, runID, &tfe.RunReadOptions{
			Include: []tfe.RunIncludeOpt{tfe.RunTFPolicyEvaluation},
		})
	})
	if err != nil {
		// The run read feeds both files, so record both errored; each self-gates so
		// a settled path is left untouched.
		recErr := c.recordErrored(ctx, evalPath, err)
		if recErr != nil {
			return recErr
		}

		return c.recordErrored(ctx, outPath, err)
	}

	if len(evaluated.TFPolicyEvaluations) == 0 {
		c.env.NotApplicable(evalPath)
		c.env.NotApplicable(outPath)

		return nil
	}

	// The evaluations are in hand, so archive them regardless of whether the
	// outcomes pagination below succeeds.
	err = c.object(ctx, evalPath, evaluated.TFPolicyEvaluations)
	if err != nil {
		return err
	}

	return c.archiveTFPolicyOutcomeSet(ctx, outPath, evaluated.TFPolicyEvaluations)
}

// archiveTFPolicyOutcomeSet paginates each evaluation's set outcomes and archives
// them aggregated at outPath.
//
// The run read already succeeded and the evaluations were written, so a
// pagination failure here records only outPath errored (via the self-gating
// Object) rather than stranding the evaluations behind it.
func (c *Collector) archiveTFPolicyOutcomeSet(
	ctx context.Context,
	outPath string,
	evals []*tfe.TFPolicyEvaluation,
) error {
	var out []*tfe.TFPolicySetOutcome

	for _, eval := range evals {
		evalID := eval.ID

		items, listErr := paginateAll(
			ctx,
			c,
			func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.TFPolicySetOutcome, *tfe.Pagination, error) {
				l, e := tc.TFPolicyEvaluationOutcomes.List(
					ctx,
					evalID,
					&tfe.TFPolicyEvaluationListOptions{ListOptions: o},
				)
				if e != nil {
					return nil, nil, fmt.Errorf("list tf policy outcomes: %w", e)
				}

				return l.Items, l.Pagination, nil
			},
		)
		if listErr != nil {
			return c.recordErrored(ctx, outPath, listErr)
		}

		out = append(out, items...)
	}

	return c.object(ctx, outPath, out)
}
