package workflowprobe

import (
	"fmt"
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

// runThresholdTemplateVersioning 通过真实 HTTP 路径验证：
// 模板创建与多版本、计划在创建时和应用后锁定具体版本、
// 模板停用与并发应用的确定结果，以及未使用模板的计划继续走内联阈值。
func runThresholdTemplateVersioning() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	if _, err := enrollNode(harness, "resolver-tpl-01", "core"); err != nil {
		return err
	}
	if _, err := enrollNode(harness, "resolver-tpl-02", "core"); err != nil {
		return err
	}

	var template domain.ThresholdTemplateView
	if err := harness.call(http.MethodPut, "/v1/threshold-templates", domain.ThresholdTemplateCreateInput{
		Name:                "gold-tier",
		Scope:               "core-dns",
		MinimumSuccessRatio: 1,
		MaximumLatencyMS:    100,
	}, &template); err != nil {
		return err
	}
	if template.LatestVersion != 1 || template.State != domain.ThresholdTemplateActive ||
		len(template.Versions) != 1 || template.Versions[0].MaximumLatencyMS != 100 {
		return ensure(false, "template was not created with its first version")
	}

	var v2 domain.ThresholdTemplateVersion
	if err := harness.call(http.MethodPatch, "/v1/threshold-templates/"+template.ID+"/versions",
		domain.ThresholdVersionCreateInput{
			MinimumSuccessRatio: 0.9,
			MaximumLatencyMS:    200,
			Scope:               "core-dns",
		}, &v2); err != nil {
		return err
	}
	if v2.Version != 2 {
		return ensure(false, "new template version was not numbered 2")
	}
	// 重复出版相同规则必须被拒绝。
	err = harness.call(http.MethodPatch, "/v1/threshold-templates/"+template.ID+"/versions",
		domain.ThresholdVersionCreateInput{
			MinimumSuccessRatio: 0.9,
			MaximumLatencyMS:    200,
			Scope:               "core-dns",
		}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeDuplicateTemplate)); err != nil {
		return err
	}

	// 创建计划时显式锁定 v1（不随最新版本漂移）。
	var locked domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                "locked.example",
		ExpectedNodes:       2,
		ThresholdTemplateID: template.ID,
		ThresholdVersion:    1,
	}, &locked); err != nil {
		return err
	}
	if locked.ThresholdTemplateID != template.ID || locked.ThresholdVersion != 1 ||
		locked.MinimumSuccessRatio != 1 || locked.MaximumLatencyMS != 100 {
		return ensure(false, "plan did not snapshot template v1 thresholds")
	}

	// 内联阈值与模板引用不能混用。
	err = harness.call(http.MethodPut, "/v1/plans", map[string]any{
		"zone":                  "mixed.example",
		"expected_nodes":        1,
		"maximum_latency_ms":    100,
		"threshold_template_id": template.ID,
	}, nil)
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}
	// 引用不存在的模板版本必须在事务内被拒绝。
	err = harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                "missing.example",
		ExpectedNodes:       1,
		ThresholdTemplateID: "tpl_does_not_exist",
	}, nil)
	if err := expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound)); err != nil {
		return err
	}

	// 草稿计划通过应用接口切换到 v2，版本号提升并锁定。
	var applied domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+locked.ID+"/threshold",
		domain.ApplyThresholdInput{TemplateID: template.ID, Version: 2, ExpectedPlanVersion: locked.Version},
		&applied); err != nil {
		return err
	}
	if applied.ThresholdVersion != 2 || applied.MaximumLatencyMS != 200 ||
		applied.MinimumSuccessRatio != 0.9 || applied.Version != locked.Version+1 {
		return ensure(false, "apply did not lock template v2 onto the plan")
	}
	// 并发：两个请求都拿同一个期望版本竞争，必须恰好一个成功。
	raceBase := applied.Version
	results := make(chan error, 2)
	raceApply := func() {
		var plan domain.RolloutPlan
		results <- harness.call(http.MethodPatch, "/v1/plans/"+locked.ID+"/threshold",
			domain.ApplyThresholdInput{TemplateID: template.ID, Version: 1, ExpectedPlanVersion: raceBase},
			&plan)
	}
	go raceApply()
	go raceApply()
	failures := 0
	for attempt := 0; attempt < 2; attempt++ {
		err = <-results
		if err != nil {
			if err := expectFailure(err, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
				return err
			}
			failures++
		}
	}
	if failures != 1 {
		return ensure(false, fmt.Sprintf("concurrent apply must have one winner, got %d conflicts", failures))
	}
	var afterRace domain.RolloutPlan
	if err := harness.call(http.MethodGet, "/v1/plans/"+locked.ID, nil, &afterRace); err != nil {
		return err
	}
	if afterRace.ThresholdVersion != 1 || afterRace.MaximumLatencyMS != 100 || afterRace.Version != raceBase+1 {
		return ensure(false, "winning concurrent apply did not lock v1")
	}
	// 对当前已锁定版本重复应用是幂等的，计划版本不变。
	var replay domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+locked.ID+"/threshold",
		domain.ApplyThresholdInput{TemplateID: template.ID, Version: 1, ExpectedPlanVersion: afterRace.Version},
		&replay); err != nil {
		return err
	}
	if replay.Version != afterRace.Version || replay.ThresholdVersion != 1 {
		return ensure(false, "re-applying the same template version was not idempotent")
	}

	// 停用模板：重复停用幂等；停用后不能再应用或用于创建计划，但可以再出版本 v3。
	// 先把已锁定计划推进到可判定状态，用它验证模板后续变化不回改计划。
	lockedRevision, err := addRevisionAndActivate(harness, &afterRace, 5001)
	if err != nil {
		return err
	}
	group, err := allocate(harness, afterRace.ID, "tpl-lease-0001", "core", 2)
	if err != nil {
		return err
	}
	for index, leaseID := range group.LeaseIDs {
		if _, err := submitProbe(harness, leaseID, fmt.Sprintf("tpl-probe-%s", leaseID),
			true, lockedRevision.Serial, 150+index*10); err != nil {
			return err
		}
	}

	var v3 domain.ThresholdTemplateVersion
	if err := harness.call(http.MethodPatch, "/v1/threshold-templates/"+template.ID+"/versions",
		domain.ThresholdVersionCreateInput{
			MinimumSuccessRatio: 1,
			MaximumLatencyMS:    500,
			Scope:               "core-dns",
		}, &v3); err != nil {
		return err
	}
	if v3.Version != 3 {
		return ensure(false, "template v3 was not created")
	}

	var disabled domain.ThresholdTemplateView
	if err := harness.call(http.MethodPatch, "/v1/threshold-templates/"+template.ID+"/disable", nil, &disabled); err != nil {
		return err
	}
	if disabled.State != domain.ThresholdTemplateDisabled || disabled.DisabledAt == nil {
		return ensure(false, "template was not disabled")
	}
	var disabledAgain domain.ThresholdTemplateView
	if err := harness.call(http.MethodPatch, "/v1/threshold-templates/"+template.ID+"/disable", nil, &disabledAgain); err != nil {
		return err
	}
	if disabledAgain.State != domain.ThresholdTemplateDisabled {
		return ensure(false, "repeated disable changed the template state")
	}
	// 停用后不能再应用（即使计划仍是 draft），也不能再出版本。
	var draftPlan domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                "draft.example",
		MinimumSuccessRatio: 0.5,
		MaximumLatencyMS:    60,
		ExpectedNodes:       1,
	}, &draftPlan); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/plans/"+draftPlan.ID+"/threshold",
		domain.ApplyThresholdInput{TemplateID: template.ID, Version: 1, ExpectedPlanVersion: draftPlan.Version},
		nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateDisabled)); err != nil {
		return err
	}
	err = harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                "newlocked.example",
		ExpectedNodes:       1,
		ThresholdTemplateID: template.ID,
		ThresholdVersion:    3,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateDisabled)); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/threshold-templates/"+template.ID+"/versions",
		domain.ThresholdVersionCreateInput{
			MinimumSuccessRatio: 1,
			MaximumLatencyMS:    600,
		}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeTemplateDisabled)); err != nil {
		return err
	}

	// 已锁定 v1（上限 100）的计划不受 v2/v3 与停用影响：探针最大延迟 160 必须判为超限。
	var pinned domain.RolloutAssessment
	if err := harness.call(http.MethodPatch, "/v1/plans/"+afterRace.ID+"/assessments",
		domain.AssessmentInput{RequestKey: "tpl-assess-0001"}, &pinned); err != nil {
		return err
	}
	if pinned.Verdict != domain.VerdictReviewRequired || !containsReason(pinned.Reasons, "latency_limit_exceeded") {
		return ensure(false, "pinned v1 thresholds were silently changed by newer template versions")
	}
	// 判定会提升计划版本，先重新读取当前版本，再断言激活后应用模板被拒绝。
	if err := harness.call(http.MethodGet, "/v1/plans/"+afterRace.ID, nil, &afterRace); err != nil {
		return err
	}
	// 计划激活后再尝试应用模板必须得到确定的非法状态结果。
	err = harness.call(http.MethodPatch, "/v1/plans/"+afterRace.ID+"/threshold",
		domain.ApplyThresholdInput{TemplateID: template.ID, Version: 1, ExpectedPlanVersion: afterRace.Version},
		nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// 未使用模板的计划继续使用请求内联阈值。
	inlinePlan, inlineRevision, err := createActivePlan(harness, "inline.example", 5101, 1, 1, 50)
	if err != nil {
		return err
	}
	if inlinePlan.ThresholdTemplateID != "" {
		return ensure(false, "inline plan unexpectedly references a template")
	}
	inlineGroup, err := allocate(harness, inlinePlan.ID, "tpl-inline-lease-0001", "core", 1)
	if err != nil {
		return err
	}
	if _, err := submitProbe(harness, inlineGroup.LeaseIDs[0], "tpl-inline-probe-0001",
		true, inlineRevision.Serial, 10); err != nil {
		return err
	}
	var inlineAssessment domain.RolloutAssessment
	if err := harness.call(http.MethodPatch, "/v1/plans/"+inlinePlan.ID+"/assessments",
		domain.AssessmentInput{RequestKey: "tpl-inline-assess-0001"}, &inlineAssessment); err != nil {
		return err
	}
	if inlineAssessment.Verdict != domain.VerdictConsistent {
		return ensure(false, "inline-threshold plan did not pass with request thresholds")
	}

	// 模板查询与列表返回版本历史。
	var fetched domain.ThresholdTemplateView
	if err := harness.call(http.MethodGet, "/v1/threshold-templates/"+template.ID, nil, &fetched); err != nil {
		return err
	}
	if fetched.LatestVersion != 3 || len(fetched.Versions) != 3 {
		return ensure(false, "template view does not contain full version history")
	}
	var listing struct {
		Items []domain.ThresholdTemplateView `json:"items"`
	}
	if err := harness.call(http.MethodGet, "/v1/threshold-templates", nil, &listing); err != nil {
		return err
	}
	if len(listing.Items) != 1 {
		return ensure(false, "template listing did not return the created template")
	}
	return nil
}

func addRevisionAndActivate(harness *harness, plan *domain.RolloutPlan, serial uint32) (domain.ZoneRevision, error) {
	var revision domain.ZoneRevision
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/revisions", domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1." + plan.Zone,
		Entries: []domain.ZoneEntry{
			{Owner: plan.Zone, Type: domain.EntryA, Value: "192.0.2.20"},
		},
	}, &revision); err != nil {
		return domain.ZoneRevision{}, err
	}
	plan.Version++
	var activated domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/activate", map[string]any{
		"expected_version": plan.Version,
	}, &activated); err != nil {
		return domain.ZoneRevision{}, err
	}
	*plan = activated
	return revision, nil
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
