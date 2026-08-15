package workspace_test

import (
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

func TestArchiveConfigurationVersionWithIngress(t *testing.T) {
	t.Parallel()

	cv := &tfe.ConfigurationVersion{
		ID:     "cv-1",
		Status: tfe.ConfigurationUploaded,
		IngressAttributes: &tfe.IngressAttributes{
			ID:        "ia-1",
			Branch:    "main",
			CommitSHA: "abc123",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	// The tarball is deduped org-wide and downloaded separately; settle it so the
	// test isolates on the record read and its split into two files.
	f.preSettle(st.ConfigVersionTarball("cv-1"))

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	f.attrs(t, st.RunFile("proj", "ws", "run-1", "config-version.json"), "configuration-versions", "cv-1")

	ingPath := st.RunFile("proj", "ws", "run-1", "config-version-ingress.json")
	ing := f.attrs(t, ingPath, "ingress-attributes", "ia-1")
	assert.Equal(t, "abc123", ing["commit-sha"])
	assert.Equal(t, "main", ing["branch"])
}

func TestArchiveConfigurationVersionVCSLess(t *testing.T) {
	t.Parallel()

	// A VCS-less upload has no ingress attributes: the record is archived and the
	// ingress path settles a not-applicable gap rather than a null file.
	cv := &tfe.ConfigurationVersion{ID: "cv-2", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.ConfigVersionTarball("cv-2"))

	run := &tfe.Run{ID: "run-2", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-2"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	f.attrs(t, st.RunFile("proj", "ws", "run-2", "config-version.json"), "configuration-versions", "cv-2")

	ingPath := st.RunFile("proj", "ws", "run-2", "config-version-ingress.json")
	assert.Equal(t, manifest.StatusNotApplicable, f.status(ingPath))

	exists, err := st.Exists(ingPath)
	require.NoError(t, err)
	assert.False(t, exists, "a VCS-less config version writes no ingress file")
}

func TestArchiveConfigurationVersionRetryGate(t *testing.T) {
	t.Parallel()

	cv := &tfe.ConfigurationVersion{
		ID:                "cv-3",
		IngressAttributes: &tfe.IngressAttributes{ID: "ia-3", CommitSHA: "def456"},
	}

	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-3", func(w http.ResponseWriter, _ *http.Request) {
		hits++

		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	cvPath := st.RunFile("proj", "ws", "run-3", "config-version.json")
	ingPath := st.RunFile("proj", "ws", "run-3", "config-version-ingress.json")

	// The record settled on an earlier pass but its ingress write did not: the
	// read must still happen because the gate opens on either derived file, and
	// the already-settled record must not regress.
	f.preSettle(cvPath)
	f.preSettle(st.ConfigVersionTarball("cv-3"))

	run := &tfe.Run{ID: "run-3", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-3"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, 1, hits, "the record is re-read while the ingress gap remains")
	f.attrs(t, ingPath, "ingress-attributes", "ia-3")
	assert.Equal(t, manifest.StatusDone, f.status(cvPath), "the settled record is left untouched")
}

func TestArchiveConfigurationVersionTarball(t *testing.T) {
	t.Parallel()

	// The tarball is streamed from its download endpoint, not preSettled: this
	// locks in byte-identity for the newly-streamed blob end to end.
	const tarball = "\x1f\x8b\x08 fake tar.gz bytes \x00\x01\x02"

	cv := &tfe.ConfigurationVersion{ID: "cv-1", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})
	mux.HandleFunc("/api/v2/configuration-versions/cv-1/download", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, tarball)
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	tarballPath := st.ConfigVersionTarball("cv-1")
	assert.Equal(t, manifest.StatusDone, f.status(tarballPath))

	got, err := os.ReadFile(st.AbsPath(tarballPath))
	require.NoError(t, err)
	assert.Equal(t, tarball, string(got), "the tarball streams to disk byte-identically")
}

func TestArchiveConfigurationVersionTarballAbsent(t *testing.T) {
	t.Parallel()

	// An expired configuration version's tarball 404s on a normal lifecycle. The
	// streaming DoRaw path discards the SDK's typed error, so this guards that
	// the status translation still classifies the 404 terminal: the in-run
	// re-probe confirms it and the object settles absent rather than
	// re-fetching and re-erroring forever.
	cv := &tfe.ConfigurationVersion{ID: "cv-1", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})
	mux.HandleFunc("/api/v2/configuration-versions/cv-1/download", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	tarballPath := st.ConfigVersionTarball("cv-1")
	assert.Equal(t, manifest.StatusAbsent, f.status(tarballPath),
		"the confirmed 404 settles absent in one run")

	exists, err := st.Exists(tarballPath)
	require.NoError(t, err)
	assert.False(t, exists, "an absent tarball writes no file")
}

func TestArchiveConfigurationVersionRecordAbsenceBlip(t *testing.T) {
	t.Parallel()

	// An eventual-consistency 404 on a just-listed configuration version must not
	// settle the derived files absent from one response: the shared record read
	// carries the same confirming re-probe as the archive primitives, so the
	// second answer wins and the split archives normally.
	cv := &tfe.ConfigurationVersion{
		ID:                "cv-4",
		IngressAttributes: &tfe.IngressAttributes{ID: "ia-4", CommitSHA: "0ddba11"},
	}

	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-4", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.ConfigVersionTarball("cv-4"))

	run := &tfe.Run{ID: "run-4", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-4"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, 2, hits, "the 404 is re-probed once before it is believed")
	f.attrs(t, st.RunFile("proj", "ws", "run-4", "config-version.json"), "configuration-versions", "cv-4")
	f.attrs(t, st.RunFile("proj", "ws", "run-4", "config-version-ingress.json"), "ingress-attributes", "ia-4")
}

func TestArchiveConfigurationVersionRecordAbsent(t *testing.T) {
	t.Parallel()

	// A repeated 404 is a confirmed absence: both derived files settle absent in
	// one run, and recording the outcome replays no extra probe.
	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-5", func(w http.ResponseWriter, _ *http.Request) {
		hits++

		w.WriteHeader(http.StatusNotFound)
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.ConfigVersionTarball("cv-5"))

	run := &tfe.Run{ID: "run-5", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-5"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, 2, hits, "one confirming re-probe, and the recording adds none")
	assert.Equal(t, manifest.StatusAbsent, f.status(st.RunFile("proj", "ws", "run-5", "config-version.json")))
	assert.Equal(t, manifest.StatusAbsent, f.status(st.RunFile("proj", "ws", "run-5", "config-version-ingress.json")))
}

func TestArchivePolicyChecksListAbsenceBlip(t *testing.T) {
	t.Parallel()

	// The policy-check list reports its failure through the direct-recording path
	// rather than a primitive's confirmed fetch, so the list itself must re-probe
	// a 404 before believing it: one blip then a clean answer archives the checks
	// instead of settling policy-checks.json absent.
	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-6/policy-checks", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		writeJSONAPI(t, w, marshalJSONAPI(t, []*tfe.PolicyCheck{}))
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-6"}
	require.NoError(t, f.collector.ArchivePolicyChecks(t.Context(), "proj", "ws", run))

	assert.Equal(t, 2, hits, "the 404 is re-probed once before it is believed")
	assert.Equal(t, manifest.StatusDone, f.status(st.RunFile("proj", "ws", "run-6", "policy-checks.json")))
}

func TestArchivePlanJSON(t *testing.T) {
	t.Parallel()

	// The structured plan is streamed from json-output, not preSettled, locking in
	// byte-identity for the newly-streamed blob end to end. The embedded NUL proves
	// the stream is byte-exact, arbitrary bytes and all, rather than re-encoded.
	const planBody = "structured plan output\x00\x01 bytes"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1/json-output", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, planBody)
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	// Settle the log so the test isolates on the structured JSON stream.
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.log"))

	run := &tfe.Run{ID: "run-1", Plan: &tfe.Plan{ID: "plan-1"}}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	jsonPath := st.RunFile("proj", "ws", "run-1", "plan.json")
	assert.Equal(t, manifest.StatusDone, f.status(jsonPath))

	got, err := os.ReadFile(st.AbsPath(jsonPath))
	require.NoError(t, err)
	assert.Equal(t, planBody, string(got), "the structured plan streams to disk byte-identically")
}

func TestArchivePlanJSONAbsent(t *testing.T) {
	t.Parallel()

	// A plan whose structured output has expired 404s; the in-run re-probe
	// confirms it, so the object settles absent rather than re-fetching and
	// re-erroring forever (the section-3 regression guard for the plan.json
	// blob).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1/json-output", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.log"))

	run := &tfe.Run{ID: "run-1", Plan: &tfe.Plan{ID: "plan-1"}}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	jsonPath := st.RunFile("proj", "ws", "run-1", "plan.json")
	assert.Equal(t, manifest.StatusAbsent, f.status(jsonPath),
		"the confirmed 404 settles absent in one run")

	exists, err := st.Exists(jsonPath)
	require.NoError(t, err)
	assert.False(t, exists, "an absent structured plan writes no file")
}

func TestArchivePlanSummary(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t, http.NewServeMux())
	st := f.store

	// The plan summary is value-in-hand; settle the heavy log and JSON so no fetch
	// is attempted and the test isolates on the summary write.
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.log"))
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.json"))

	run := &tfe.Run{
		ID:   "run-1",
		Plan: &tfe.Plan{ID: "plan-1", HasChanges: true, ResourceAdditions: 3, ResourceDestructions: 1},
	}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	attrs := f.attrs(t, st.RunFile("proj", "ws", "run-1", "plan-summary.json"), "plans", "plan-1")
	assert.Equal(t, true, attrs["has-changes"])
	assert.InDelta(t, 3, attrs["resource-additions"], 0)
	assert.InDelta(t, 1, attrs["resource-destructions"], 0)
}

func TestArchivePlanLog(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.Plan{
			ID:         "plan-1",
			LogReadURL: "http://" + r.Host + "/artifact/plan-1-log",
		}))
	})
	mux.HandleFunc("/artifact/plan-1-log", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, "\x02plan output\x03")
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	// Settle the structured JSON so the test isolates on the log download.
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.json"))

	run := &tfe.Run{ID: "run-1", Plan: &tfe.Plan{ID: "plan-1"}}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	logPath := st.RunFile("proj", "ws", "run-1", "plan.log")
	assert.Equal(t, manifest.StatusDone, f.status(logPath))

	got, err := os.ReadFile(st.AbsPath(logPath))
	require.NoError(t, err)
	assert.Equal(t, "plan output", string(got), "the log downloads whole with its STX/ETX framing trimmed")
}

func TestArchiveApplyLog(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/applies/apply-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.Apply{
			ID:         "apply-1",
			LogReadURL: "http://" + r.Host + "/artifact/apply-1-log",
		}))
	})
	mux.HandleFunc("/artifact/apply-1-log", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, "\x02apply output\x03")
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-1", Apply: &tfe.Apply{ID: "apply-1"}}
	require.NoError(t, f.collector.ArchiveApply(t.Context(), "proj", "ws", run))

	logPath := st.RunFile("proj", "ws", "run-1", "apply.log")
	assert.Equal(t, manifest.StatusDone, f.status(logPath))

	got, err := os.ReadFile(st.AbsPath(logPath))
	require.NoError(t, err)
	assert.Equal(t, "apply output", string(got), "the log downloads whole with its STX/ETX framing trimmed")
}

func TestArchiveApplySummary(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t, http.NewServeMux())
	st := f.store

	f.preSettle(st.RunFile("proj", "ws", "run-1", "apply.log"))

	run := &tfe.Run{
		ID:    "run-1",
		Apply: &tfe.Apply{ID: "apply-1", ResourceAdditions: 5, ResourceChanges: 2},
	}
	require.NoError(t, f.collector.ArchiveApply(t.Context(), "proj", "ws", run))

	attrs := f.attrs(t, st.RunFile("proj", "ws", "run-1", "apply-summary.json"), "applies", "apply-1")
	assert.InDelta(t, 5, attrs["resource-additions"], 0)
	assert.InDelta(t, 2, attrs["resource-changes"], 0)
}

func TestArchiveTFPolicyOutcomes(t *testing.T) {
	t.Parallel()

	hydrated := &tfe.Run{
		ID:                  "run-1",
		TFPolicyEvaluations: []*tfe.TFPolicyEvaluation{{ID: "eval-1", Status: tfe.TFPolicyEvaluationStatusPassed}},
	}
	outcome := &tfe.TFPolicySetOutcome{ID: "out-1", PolicySetName: "prod-guardrails"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, hydrated))
	})
	mux.HandleFunc("/api/v2/tf-policy-evaluations/eval-1/tf-policy-set-outcomes",
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSONAPI(t, w, marshalJSONAPI(t, []*tfe.TFPolicySetOutcome{outcome}))
		})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveTFPolicyOutcomes(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	// The hydrated evaluations are archived as their own file, not dropped as bare
	// id refs, alongside their aggregated set outcomes.
	assert.Equal(t, []string{"eval-1"}, f.dataIDs(t, st.RunFile("proj", "ws", "run-1", "tf-policy-evaluations.json")))
	assert.Equal(t, []string{"out-1"}, f.dataIDs(t, st.RunFile("proj", "ws", "run-1", "tf-policy-outcomes.json")))
}

func TestArchiveTFPolicyOutcomesNoEvaluations(t *testing.T) {
	t.Parallel()

	// The common no-policy run has no native evaluations: both files settle a
	// not-applicable gap rather than writing an empty {"data":[]} outcomes list.
	hydrated := &tfe.Run{ID: "run-2"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, hydrated))
	})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveTFPolicyOutcomes(t.Context(), "proj", "ws", &tfe.Run{ID: "run-2"}))

	evalPath := st.RunFile("proj", "ws", "run-2", "tf-policy-evaluations.json")
	outPath := st.RunFile("proj", "ws", "run-2", "tf-policy-outcomes.json")

	assert.Equal(t, manifest.StatusNotApplicable, f.status(evalPath))
	assert.Equal(t, manifest.StatusNotApplicable, f.status(outPath))

	for _, p := range []string{evalPath, outPath} {
		exists, err := st.Exists(p)
		require.NoError(t, err)
		assert.False(t, exists, "%s should not be written for a no-policy run", p)
	}
}

func TestArchiveComments(t *testing.T) {
	t.Parallel()

	// Each pass lists the comment ids the API answers on that pass; the archive
	// must reflect the last of them, since a comment can be left on a run long
	// after it finished and the archiver would otherwise never look again.
	tests := map[string]struct {
		passes [][]string
		want   []string
	}{
		"the first pass archives the run's comments": {
			passes: [][]string{{"comment-1"}},
			want:   []string{"comment-1"},
		},
		"a comment left on a run archived with none is still captured": {
			passes: [][]string{{}, {"comment-2"}},
			want:   []string{"comment-2"},
		},
		"a comment left on a run archived with one joins it": {
			passes: [][]string{{"comment-1"}, {"comment-1", "comment-2"}},
			want:   []string{"comment-1", "comment-2"},
		},
		"an unchanged list leaves the archive as it was": {
			passes: [][]string{{"comment-1"}, {"comment-1"}},
			want:   []string{"comment-1"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var hits int

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/runs/run-1/comments", func(w http.ResponseWriter, _ *http.Request) {
				ids := tc.passes[min(hits, len(tc.passes)-1)]
				hits++

				comments := make([]*tfe.Comment, len(ids))
				for i, id := range ids {
					comments[i] = &tfe.Comment{ID: id, Body: "body of " + id}
				}

				writeJSONAPI(t, w, marshalJSONAPI(t, comments))
			})

			f := newWSFixture(t, mux)
			run := &tfe.Run{ID: "run-1"}

			for range tc.passes {
				require.NoError(t, f.collector.ArchiveComments(t.Context(), "proj", "ws", run))
			}

			assert.Equal(t, len(tc.passes), hits, "comments are re-listed on every pass, never settled once")
			assert.Equal(t, tc.want, f.dataIDs(t, f.store.RunFile("proj", "ws", "run-1", "comments.json")))
		})
	}
}
