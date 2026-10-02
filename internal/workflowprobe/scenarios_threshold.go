package workflowprobe

import (
	"fmt"
	"net/http"
	"sync"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/threshold"
)

func createThresholdTemplate(
	harness *harness,
	body any,
) (domain.ThresholdTemplate, error) {
	var template domain.ThresholdTemplate
	err := harness.call(http.MethodPut, "/v1/threshold-templates", body, &template)
	return template, err
}

func addThresholdTemplateVersion(
	harness *harness,
	templateID string,
	body any,
) (domain.ThresholdTemplate, error) {
	var template domain.ThresholdTemplate
	err := harness.call(
		http.MethodPatch,
		"/v1/threshold-templates/"+templateID+"/versions",
		body,
		&template,
	)
	return template, err
}

func disableThresholdTemplate(harness *harness, templateID string) error {
	return harness.call(
		http.MethodPatch,
		"/v1/threshold-templates/"+templateID+"/disable",
		nil,
		nil,
	)
}

func applyThresholdTemplate(
	harness *harness,
	planID string,
	body threshold.ApplyInput,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.call(
		http.MethodPatch,
		"/v1/plans/"+planID+"/threshold-template",
		body,
		&plan,
	)
	return plan, err
}

func createDraftPlan(
	harness *harness,
	zone string,
	minimumRatio float64,
	maximumLatency int,
	expectedNodes int,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                zone,
		MinimumSuccessRatio: minimumRatio,
		MaximumLatencyMS:    maximumLatency,
		ExpectedNodes:       expectedNodes,
	}, &plan)
	return plan, err
}

func runThresholdTemplateVersioning() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	// Invalid thresholds are rejected before anything is stored.
	_, err = createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "bad template",
		MinimumSuccessRatio: 1.5,
		MaximumLatencyMS:    80,
	})
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}
	// Global scope must not carry zones; a zones scope must list zones.
	_, err = createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "bad global scope",
		MinimumSuccessRatio: 0.9,
		MaximumLatencyMS:    80,
		Scope: domain.TemplateScopeInput{
			Type:  "global",
			Zones: []string{"edge.example"},
		},
	})
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}
	_, err = createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "empty zones scope",
		MinimumSuccessRatio: 0.9,
		MaximumLatencyMS:    80,
		Scope:               domain.TemplateScopeInput{Type: "zones"},
	})
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	// Global template v1 with strict thresholds; scope defaults to global.
	standard, err := createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "standard tier",
		Description:         "shared strict thresholds",
		MinimumSuccessRatio: 1,
		MaximumLatencyMS:    80,
	})
	if err != nil {
		return err
	}
	if standard.LatestVersion != 1 || standard.State != domain.ThresholdTemplateActive {
		return ensure(false, "template was not created active with version 1")
	}
	if standard.Versions[0].Scope.Type != domain.TemplateScopeGlobal {
		return ensure(false, "template scope did not default to global")
	}
	// Duplicate name is rejected.
	_, err = createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "standard tier",
		MinimumSuccessRatio: 0.5,
		MaximumLatencyMS:    200,
	})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeConflict)); err != nil {
		return err
	}

	// A plan without a template keeps the inline request thresholds.
	inlinePlan, err := createDraftPlan(harness, "edge.example", 0.5, 500, 1)
	if err != nil {
		return err
	}
	if inlinePlan.ThresholdSource != domain.ThresholdFromRequest ||
		inlinePlan.ThresholdTemplate != nil {
		return ensure(false, "template-free plan must record request thresholds only")
	}

	// Apply v1: thresholds are snapshotted and the exact version is locked.
	applied, err := applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      standard.ID,
		ExpectedVersion: inlinePlan.Version,
	})
	if err != nil {
		return err
	}
	if applied.MinimumSuccessRatio != 1 || applied.MaximumLatencyMS != 80 {
		return ensure(false, "apply did not snapshot template thresholds")
	}
	if applied.ThresholdSource != domain.ThresholdFromTemplate ||
		applied.ThresholdTemplate == nil ||
		applied.ThresholdTemplate.TemplateID != standard.ID ||
		applied.ThresholdTemplate.Version != 1 {
		return ensure(false, "plan did not lock template version 1")
	}

	// Replaying the same application is a no-op and must not move the version.
	replay, err := applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      standard.ID,
		TemplateVersion: 1,
		ExpectedVersion: applied.Version,
	})
	if err != nil {
		return err
	}
	if replay.Version != applied.Version ||
		replay.MaximumLatencyMS != 80 ||
		replay.ThresholdTemplate.Version != 1 {
		return ensure(false, "idempotent template replay changed the plan")
	}

	// Publish relaxed v2 and v3.
	relaxed, err := addThresholdTemplateVersion(harness, standard.ID, domain.ThresholdTemplateVersionInput{
		MinimumSuccessRatio: 0.5,
		MaximumLatencyMS:    400,
	})
	if err != nil {
		return err
	}
	if relaxed.LatestVersion != 2 || len(relaxed.Versions) != 2 {
		return ensure(false, "second version was not appended as version 2")
	}
	relaxed, err = addThresholdTemplateVersion(harness, standard.ID, domain.ThresholdTemplateVersionInput{
		MinimumSuccessRatio: 0.25,
		MaximumLatencyMS:    600,
	})
	if err != nil {
		return err
	}
	if relaxed.LatestVersion != 3 {
		return ensure(false, "third version was not numbered 3")
	}

	// The locked plan is untouched by later template versions.
	var stillLocked domain.RolloutPlan
	if err := harness.call(http.MethodGet, "/v1/plans/"+inlinePlan.ID, nil, &stillLocked); err != nil {
		return err
	}
	if stillLocked.MinimumSuccessRatio != 1 ||
		stillLocked.MaximumLatencyMS != 80 ||
		stillLocked.ThresholdTemplate.Version != 1 {
		return ensure(false, "later template version silently changed the locked plan")
	}

	// Move to latest (v3) on the draft plan.
	updated, err := applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      standard.ID,
		ExpectedVersion: stillLocked.Version,
	})
	if err != nil {
		return err
	}
	if updated.ThresholdTemplate.Version != 3 ||
		updated.MinimumSuccessRatio != 0.25 ||
		updated.MaximumLatencyMS != 600 {
		return ensure(false, "plan did not move to latest template version")
	}
	// Re-selecting an explicit older version is allowed while draft.
	rolledBack, err := applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      standard.ID,
		TemplateVersion: 1,
		ExpectedVersion: updated.Version,
	})
	if err != nil {
		return err
	}
	if rolledBack.ThresholdTemplate.Version != 1 || rolledBack.MaximumLatencyMS != 80 {
		return ensure(false, "explicit old template version could not be reselected")
	}

	// Missing template, missing version and stale plan version are deterministic.
	_, err = applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      "tpl_missing",
		ExpectedVersion: rolledBack.Version,
	})
	if err := expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound)); err != nil {
		return err
	}
	_, err = applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      standard.ID,
		TemplateVersion: 99,
		ExpectedVersion: rolledBack.Version,
	})
	if err := expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound)); err != nil {
		return err
	}
	_, err = applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      standard.ID,
		ExpectedVersion: rolledBack.Version - 1,
	})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
		return err
	}
	// Malformed apply body is a validation error.
	_, err = applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		ExpectedVersion: rolledBack.Version,
	})
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	// Zone-scoped template applies only to plans in its zone list.
	scoped, err := createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "east tier",
		MinimumSuccessRatio: 0.9,
		MaximumLatencyMS:    120,
		Scope: domain.TemplateScopeInput{
			Type:  "zones",
			Zones: []string{"east.example"},
		},
	})
	if err != nil {
		return err
	}
	if scoped.Versions[0].Scope.Type != domain.TemplateScopeZones ||
		len(scoped.Versions[0].Scope.Zones) != 1 ||
		scoped.Versions[0].Scope.Zones[0] != "east.example" {
		return ensure(false, "scoped template did not store its normalized zone list")
	}
	_, err = applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      scoped.ID,
		ExpectedVersion: rolledBack.Version,
	})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateScopeMismatch)); err != nil {
		return err
	}

	// Disabling blocks new applications, new versions and repeated disable;
	// it never changes plans that already locked a version.
	if err := disableThresholdTemplate(harness, scoped.ID); err != nil {
		return err
	}
	_, err = applyThresholdTemplate(harness, inlinePlan.ID, threshold.ApplyInput{
		TemplateID:      scoped.ID,
		ExpectedVersion: rolledBack.Version,
	})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateDisabled)); err != nil {
		return err
	}
	_, err = addThresholdTemplateVersion(harness, scoped.ID, domain.ThresholdTemplateVersionInput{
		MinimumSuccessRatio: 0.1,
		MaximumLatencyMS:    10,
	})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateDisabled)); err != nil {
		return err
	}
	if err := disableThresholdTemplate(harness, scoped.ID); err == nil {
		return ensure(false, "second disable must fail, got success")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateDisabled)); err != nil {
		return err
	}
	if err := disableThresholdTemplate(harness, "tpl_missing"); err == nil {
		return ensure(false, "disable on missing template must 404, got success")
	} else if err := expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound)); err != nil {
		return err
	}

	// Concurrent applications racing on the same plan version: one wins,
	// one receives a deterministic version conflict, no partial state.
	if err := assertConcurrentApply(harness, standard.ID, inlinePlan.ID, rolledBack.Version); err != nil {
		return err
	}

	// A locked version keeps governing a plan through activation even after
	// the template publishes a loose v2 and is then disabled.
	if err := assertLockedThresholdSurvives(harness); err != nil {
		return err
	}

	// Listing returns every template with stable pagination metadata.
	var page domain.ThresholdTemplatePage
	if err := harness.call(http.MethodGet, "/v1/threshold-templates?limit=100", nil, &page); err != nil {
		return err
	}
	if len(page.Items) < 3 {
		return ensure(false, fmt.Sprintf("expected at least 3 templates, got %d", len(page.Items)))
	}
	return nil
}

func assertConcurrentApply(
	harness *harness,
	templateID string,
	planID string,
	expectedVersion uint64,
) error {
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, results[index] = applyThresholdTemplate(harness, planID, threshold.ApplyInput{
				TemplateID:      templateID,
				ExpectedVersion: expectedVersion,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	successes, conflicts := 0, 0
	for _, resultErr := range results {
		switch {
		case resultErr == nil:
			successes++
		default:
			if failure, ok := resultErr.(*apiFailure); ok &&
				failure.Status == http.StatusConflict &&
				failure.Code == string(domain.CodeVersionConflict) {
				conflicts++
				continue
			}
			return fmt.Errorf("unexpected concurrent apply result: %v", resultErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		return fmt.Errorf("concurrent apply: want 1 success and 1 conflict, got %d success and %d conflict", successes, conflicts)
	}
	return nil
}

func assertLockedThresholdSurvives(harness *harness) error {
	template, err := createThresholdTemplate(harness, domain.ThresholdTemplateCreateInput{
		Name:                "locked tier",
		MinimumSuccessRatio: 1,
		MaximumLatencyMS:    50,
	})
	if err != nil {
		return err
	}
	// Draft starts with deliberately loose inline thresholds.
	plan, err := createDraftPlan(harness, "locked.example", 0, 600000, 1)
	if err != nil {
		return err
	}
	applied, err := applyThresholdTemplate(harness, plan.ID, threshold.ApplyInput{
		TemplateID:      template.ID,
		ExpectedVersion: plan.Version,
	})
	if err != nil {
		return err
	}

	// Publish a loose v2 and disable the template; the plan must stay on v1.
	if _, err := addThresholdTemplateVersion(harness, template.ID, domain.ThresholdTemplateVersionInput{
		MinimumSuccessRatio: 0,
		MaximumLatencyMS:    600000,
	}); err != nil {
		return err
	}
	if err := disableThresholdTemplate(harness, template.ID); err != nil {
		return err
	}

	var revision domain.ZoneRevision
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/revisions", domain.RevisionCreateInput{
		Serial:    5001,
		Authority: "ns1.locked.example",
		Entries: []domain.ZoneEntry{
			{Owner: "locked.example", Type: domain.EntryA, Value: "192.0.2.20"},
		},
	}, &revision); err != nil {
		return err
	}

	var active domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/activate", map[string]any{
		"expected_version": applied.Version + 1,
	}, &active); err != nil {
		return err
	}
	if active.MinimumSuccessRatio != 1 || active.MaximumLatencyMS != 50 ||
		active.ThresholdTemplate == nil || active.ThresholdTemplate.Version != 1 {
		return ensure(false, "activated plan lost its locked template thresholds")
	}
	// Applying to an activated plan is rejected as well.
	_, err = applyThresholdTemplate(harness, plan.ID, threshold.ApplyInput{
		TemplateID:      template.ID,
		ExpectedVersion: active.Version,
	})
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	if _, err := enrollNode(harness, "resolver-locked-01", "north"); err != nil {
		return err
	}
	group, err := allocate(harness, plan.ID, "locked-lease-0001", "north", 1)
	if err != nil {
		return err
	}
	// Probe latency 200 passes the loose inline/v2 thresholds but violates
	// the locked v1 limit of 50ms.
	if _, err := submitProbe(harness, group.LeaseIDs[0], "locked-probe-0001", true, revision.Serial, 200); err != nil {
		return err
	}
	var assessment domain.RolloutAssessment
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/assessments", domain.AssessmentInput{
		RequestKey: "locked-assess-0001",
	}, &assessment); err != nil {
		return err
	}
	if assessment.Verdict != domain.VerdictReviewRequired {
		return ensure(false, "locked threshold was not used after template disable")
	}
	foundLatencyReason := false
	for _, reason := range assessment.Reasons {
		if reason == "latency_limit_exceeded" {
			foundLatencyReason = true
		}
	}
	if !foundLatencyReason || assessment.MaximumLatencyMS != 200 {
		return ensure(false, "assessment did not report the locked latency violation")
	}
	return nil
}
