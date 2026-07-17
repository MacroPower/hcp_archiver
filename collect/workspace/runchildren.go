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
	// archive that user directly (the only capture of who queued the run). It lands
	// in the org-root users/ shard, outside this run's subtree, so a failed write
	// is invisible to the runs walk's retry gate; mirror it into a run-scoped
	// reference gate so the run is re-walked until the user is captured.
	err := c.archiveUser(ctx, run.CreatedBy)
	if err != nil {
		return err
	}

	if run.CreatedBy == nil {
		return nil
	}

	st := c.env.Store()
	c.env.Reference(st.RunFile(project, ws, run.ID, "created-by.ref"), st.User(run.CreatedBy.ID))

	return nil
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

	tarballPath := st.ConfigVersionTarball(cvID)

	err = c.blob(ctx, tarballPath, func(ctx context.Context) (io.ReadCloser, error) {
		return c.env.Client().OpenConfigurationVersion(ctx, cvID)
	})
	if err != nil {
		return err
	}

	// The tarball is deduped org-wide under config-versions/, outside this run's
	// subtree, so a failed write is invisible to the runs walk's retry gate. Its
	// own Blob re-attempts on any re-walk; the gate supplies only the visibility
	// half, so mirror the tarball's settlement into a run-scoped reference gate.
	c.env.Reference(st.RunFile(project, ws, run.ID, "config-version-tarball.ref"), tarballPath)

	return nil
}

// archiveConfigVersionRecord reads the configuration version once, only while
// either derived file is still unsettled, and splits it into config-version.json
// and config-version-ingress.json.
//
// One read feeds both files, so it runs confirmed (a 404 is re-probed before it
// is believed) and a failure is routed through each path's self-gating
// recordErrored: a settled path is left untouched, an unsettled one records the
// outcome so a re-run retries.
func (c *Collector) archiveConfigVersionRecord(ctx context.Context, cvPath, ingPath, cvID string) error {
	if !c.env.ShouldFetch(cvPath) && !c.env.ShouldFetch(ingPath) {
		return nil
	}

	cv, err := readConfirmed(ctx, c, cvPath,
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
// so it is archived directly at users/<id>.json, a sibling shard outside this
// run's subtree, and the only capture of who acted on a run. Two facts make a
// plain settle-and-skip read unsafe here: that user write can fail invisibly to
// the runs walk's retry gate (it scans only the run's own shard), and the actors
// live only in the list include, not on the run, so a settled re-walk that
// skipped the read could never re-derive them. So the read is gated on the union
// of run-events.json being unsettled and a run-scoped actors reference gate being
// open, and runs via the non-self-gating readConfirmed: it re-derives the actors
// even after run-events.json is Done, and the gate clears once every actor is
// captured.
//
// The events file settles Done last, after the actor writes and the gate, so this
// skip signal is never persisted ahead of the writes it stands in for. A crash
// after the actors and gate but before it settles leaves it unsettled, forcing a
// later run to re-read; settling it first would let a flush persist Done with the
// gate not yet written, stranding an event actor behind an already-settled events
// file on an otherwise settled run.
func (c *Collector) archiveRunEvents(ctx context.Context, project, ws string, run *tfe.Run) error {
	st := c.env.Store()
	eventsPath := st.RunFile(project, ws, run.ID, "run-events.json")
	actorsGate := st.RunFile(project, ws, run.ID, "run-events-actors.ref")
	runID := run.ID

	if !c.env.ShouldFetch(eventsPath) && !c.env.ReferencePending(actorsGate) {
		return nil
	}

	events, err := readConfirmed(ctx, c, eventsPath,
		func(ctx context.Context, tc *tfe.Client) ([]*tfe.RunEvent, error) {
			l, e := tc.RunEvents.List(ctx, runID, &tfe.RunEventListOptions{
				Include: []tfe.RunEventIncludeOpt{tfe.RunEventActor, tfe.RunEventComment},
			})
			if e != nil {
				return nil, fmt.Errorf("list run events: %w", e)
			}

			return l.Items, nil
		})
	if err != nil {
		// Unlike the cross-shard actor writes, eventsPath is in the runs walk's own
		// shard, so an errored entry is already visible to HasUnsettledUnder and needs
		// no gate; recordErrored records it without regressing a settled events file.
		return c.recordErrored(ctx, eventsPath, err)
	}

	var actorPaths []string

	for _, ev := range events {
		uErr := c.archiveUser(ctx, ev.Actor)
		if uErr != nil {
			return uErr
		}

		if ev.Actor != nil {
			actorPaths = append(actorPaths, st.User(ev.Actor.ID))
		}
	}

	// Mirror the actor writes (the org-root users/ shard) into the run-scoped gate:
	// a failed actor write keeps the gate open so a later run re-reads and captures
	// it. Zero actors leaves no paths, so the gate clears and the read stops
	// recurring once run-events.json is Done.
	c.env.Reference(actorsGate, actorPaths...)

	// Settle run-events.json Done last, after the actors and the gate, so the skip
	// signal never lands ahead of them. The self-gating object still leaves an
	// already-Done events file untouched on a pure re-read (only the gate open).
	return c.object(ctx, eventsPath, events)
}

// archivePolicyChecks archives the run's Sentinel policy checks and a log per
// check.
func (c *Collector) archivePolicyChecks(ctx context.Context, project, ws string, run *tfe.Run) error {
	st := c.env.Store()
	relPath := st.RunFile(project, ws, run.ID, "policy-checks.json")
	logsGate := st.RunFile(project, ws, run.ID, "policy-check-logs.ref")
	runID := run.ID

	// Skip the metered PolicyChecks.List once this run's checks and every log have
	// settled, so a long-running sibling that keeps the runs walk re-running does
	// not re-list every already-archived run each pass. The checks file settles
	// Done even with a log still errored, because logBlob records the failure and
	// returns nil, so ShouldFetch(policy-checks.json) alone would skip the list and
	// strand the errored log forever. Gate the skip on a run-scoped reference over
	// the log paths too: while any log is unsettled the gate stays open and this
	// re-lists to retry it, and once every log settles Done or Absent the gate
	// clears and the list is skipped.
	//
	// A retry-absent run widens the gate once more: an absent log settled the
	// gate on the run that recorded it, and the log is reachable only through
	// this listing, so without the widening the flag would never re-probe its
	// own target while still disabling the runs walk's early stop over it. The
	// cost — one extra list for a run whose subtree holds any absence — is
	// bounded per pass and converges.
	if !c.env.ShouldFetch(relPath) && !c.env.ReferencePending(logsGate) &&
		!c.env.HasRetryableAbsentUnder(st.RunDir(project, ws, run.ID)) {
		return nil
	}

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

	logPaths := make([]string, 0, len(checks))

	for _, pc := range checks {
		checkID := pc.ID
		logPath := st.RunFile(project, ws, run.ID, "policy-check-"+pc.ID+".log")
		logPaths = append(logPaths, logPath)

		logErr := c.logBlob(ctx, logPath,
			func(ctx context.Context, tc *tfe.Client) (io.Reader, error) {
				return tc.PolicyChecks.Logs(ctx, checkID)
			})
		if logErr != nil {
			return logErr
		}
	}

	// Mirror the per-check log settlement into the run-scoped gate before
	// policy-checks.json settles Done: an errored log keeps the gate open so a
	// later run re-lists and re-fetches it, while zero checks leaves no paths so
	// the gate clears.
	c.env.Reference(logsGate, logPaths...)

	// Settle policy-checks.json Done last, after every log and the gate, so the
	// skip signal above never lands ahead of the logs it stands for.
	return c.object(ctx, relPath, checks)
}

// archiveTaskStages archives the run's task stages as listed; their task-result
// and policy-evaluation relations stay bare id refs.
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

	evaluated, err := readConfirmed(ctx, c, outPath, func(ctx context.Context, tc *tfe.Client) (*tfe.Run, error) {
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
