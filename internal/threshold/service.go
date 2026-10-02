package threshold

import (
	"context"
	"sort"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

// ApplyInput selects a template version for a plan. TemplateVersion zero
// means the template's latest version at application time.
type ApplyInput struct {
	TemplateID      string `json:"template_id"`
	TemplateVersion int    `json:"template_version"`
	ExpectedVersion uint64 `json:"expected_version"`
}

func ValidateApplyInput(input ApplyInput) error {
	details := make(map[string]any)
	if strings.TrimSpace(input.TemplateID) == "" {
		details["template_id"] = "is required"
	}
	if input.ExpectedVersion == 0 {
		details["expected_version"] = "must be greater than zero"
	}
	if input.TemplateVersion < 0 {
		details["template_version"] = "must be zero (latest) or a positive version number"
	}
	if len(details) > 0 {
		return domain.Validation("threshold template application is invalid", details)
	}
	return nil
}

func (s *Service) Create(
	ctx context.Context,
	input domain.ThresholdTemplateCreateInput,
) (domain.ThresholdTemplate, error) {
	input = domain.NormalizeThresholdTemplateCreateInput(input)
	if err := domain.ValidateThresholdTemplateCreateInput(input); err != nil {
		return domain.ThresholdTemplate{}, err
	}
	now := s.clock.Now().UTC()
	template := domain.ThresholdTemplate{
		ID:            s.ids.New("tpl"),
		Name:          input.Name,
		Description:   input.Description,
		State:         domain.ThresholdTemplateActive,
		LatestVersion: 1,
		Versions: []domain.ThresholdTemplateVersion{
			{
				Version:             1,
				MinimumSuccessRatio: input.MinimumSuccessRatio,
				MaximumLatencyMS:    input.MaximumLatencyMS,
				Scope:               domain.ScopeFromInput(input.Scope),
				CreatedAt:           now,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		for _, existing := range state.ThresholdTemplates {
			if existing.Name == template.Name {
				return domain.NewError(domain.CodeConflict, "threshold template name is already in use", map[string]any{
					"name": template.Name,
				})
			}
		}
		state.ThresholdTemplates[template.ID] = template
		store.AddAudit(state, s.ids, "threshold_template", template.ID, "created", now, map[string]any{
			"name": template.Name,
		})
		return nil
	})
	return template, err
}

func (s *Service) AddVersion(
	ctx context.Context,
	templateID string,
	input domain.ThresholdTemplateVersionInput,
) (domain.ThresholdTemplate, error) {
	input = domain.NormalizeThresholdTemplateVersionInput(input)
	if err := domain.ValidateThresholdTemplateVersionInput(input); err != nil {
		return domain.ThresholdTemplate{}, err
	}
	now := s.clock.Now().UTC()
	var updated domain.ThresholdTemplate
	err := s.store.Write(ctx, func(state *domain.State) error {
		template, ok := state.ThresholdTemplates[templateID]
		if !ok {
			return domain.NotFound("threshold_template", templateID)
		}
		if template.State != domain.ThresholdTemplateActive {
			return templateDisabled(templateID, template.State)
		}
		version := domain.ThresholdTemplateVersion{
			Version:             template.LatestVersion + 1,
			MinimumSuccessRatio: input.MinimumSuccessRatio,
			MaximumLatencyMS:    input.MaximumLatencyMS,
			Scope:               domain.ScopeFromInput(input.Scope),
			CreatedAt:           now,
		}
		template.Versions = append(template.Versions, version)
		template.LatestVersion = version.Version
		template.UpdatedAt = now
		state.ThresholdTemplates[templateID] = template
		store.AddAudit(state, s.ids, "threshold_template", templateID, "version_added", now, map[string]any{
			"version": version.Version,
		})
		updated = template
		return nil
	})
	return updated, err
}

func (s *Service) Disable(ctx context.Context, templateID string) (domain.ThresholdTemplate, error) {
	now := s.clock.Now().UTC()
	var updated domain.ThresholdTemplate
	err := s.store.Write(ctx, func(state *domain.State) error {
		template, ok := state.ThresholdTemplates[templateID]
		if !ok {
			return domain.NotFound("threshold_template", templateID)
		}
		if template.State != domain.ThresholdTemplateActive {
			return templateDisabled(templateID, template.State)
		}
		template.State = domain.ThresholdTemplateDisabled
		disabledAt := now
		template.DisabledAt = &disabledAt
		template.UpdatedAt = now
		state.ThresholdTemplates[templateID] = template
		store.AddAudit(state, s.ids, "threshold_template", templateID, "disabled", now, nil)
		updated = template
		return nil
	})
	return updated, err
}

func (s *Service) Get(ctx context.Context, templateID string) (domain.ThresholdTemplate, error) {
	var template domain.ThresholdTemplate
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.ThresholdTemplates[templateID]
		if !ok {
			return domain.NotFound("threshold_template", templateID)
		}
		template = found
		return nil
	})
	return template, err
}

func (s *Service) List(
	ctx context.Context,
	stateFilter domain.ThresholdTemplateState,
	limit int,
	cursor string,
) (domain.ThresholdTemplatePage, error) {
	if limit < 1 || limit > 100 {
		return domain.ThresholdTemplatePage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	if stateFilter != "" &&
		stateFilter != domain.ThresholdTemplateActive &&
		stateFilter != domain.ThresholdTemplateDisabled {
		return domain.ThresholdTemplatePage{}, domain.Validation("threshold template state is not supported", map[string]any{
			"state": stateFilter,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.ThresholdTemplatePage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.ThresholdTemplatePage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.ThresholdTemplate, 0, len(state.ThresholdTemplates))
		for _, template := range state.ThresholdTemplates {
			if stateFilter != "" && template.State != stateFilter {
				continue
			}
			if after != "" && template.ID <= after {
				continue
			}
			items = append(items, template)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

// ApplyToPlan snapshots a template version onto a draft plan. The plan
// stores the thresholds directly and keeps a reference to the exact
// template version, so later template changes never mutate the plan.
func (s *Service) ApplyToPlan(
	ctx context.Context,
	planID string,
	input ApplyInput,
) (domain.RolloutPlan, error) {
	if err := ValidateApplyInput(input); err != nil {
		return domain.RolloutPlan{}, err
	}
	now := s.clock.Now().UTC()
	var updated domain.RolloutPlan
	err := s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if plan.Version != input.ExpectedVersion {
			return domain.NewError(domain.CodeVersionConflict, "plan version does not match", map[string]any{
				"plan_id":  planID,
				"expected": input.ExpectedVersion,
				"actual":   plan.Version,
			})
		}
		template, ok := state.ThresholdTemplates[input.TemplateID]
		if !ok {
			return domain.NotFound("threshold_template", input.TemplateID)
		}
		// An explicit request for the exact version already locked on the
		// plan is an idempotent replay: return the plan unchanged without
		// moving its version or re-validating template state, so disabling
		// the template cannot turn a safe replay into a failure. A request
		// for "latest" (template_version omitted) is always re-evaluated,
		// which keeps active-plan updates a deterministic conflict.
		if input.TemplateVersion != 0 && plan.ThresholdTemplate != nil &&
			plan.ThresholdTemplate.TemplateID == input.TemplateID &&
			plan.ThresholdTemplate.Version == input.TemplateVersion {
			updated = plan
			return nil
		}
		if plan.State != domain.PlanDraft {
			return domain.InvalidState("threshold template can be applied only while plan is draft", map[string]any{
				"plan_id": planID,
				"state":   plan.State,
			})
		}
		if template.State != domain.ThresholdTemplateActive {
			return templateDisabled(template.ID, template.State)
		}
		versionNumber := template.LatestVersion
		if input.TemplateVersion != 0 {
			versionNumber = input.TemplateVersion
		}
		version, found := versionByNumber(template, versionNumber)
		if !found {
			return domain.NewError(domain.CodeNotFound, "threshold template version was not found", map[string]any{
				"template_id": template.ID,
				"version":     versionNumber,
				"latest":      template.LatestVersion,
			})
		}
		if !domain.ScopeAppliesToZone(version.Scope, plan.Zone) {
			return domain.NewError(domain.CodeTemplateScopeMismatch, "threshold template scope does not include plan zone", map[string]any{
				"template_id": template.ID,
				"version":     version.Version,
				"scope":       version.Scope,
				"zone":        plan.Zone,
			})
		}
		// Snapshot the thresholds onto the plan; the plan never reads the
		// template again, which locks this decision to the version.
		plan.MinimumSuccessRatio = version.MinimumSuccessRatio
		plan.MaximumLatencyMS = version.MaximumLatencyMS
		plan.ThresholdSource = domain.ThresholdFromTemplate
		plan.ThresholdTemplate = &domain.AppliedThresholdTemplate{
			TemplateID: template.ID,
			Version:    version.Version,
			AppliedAt:  now,
		}
		plan.Version++
		state.Plans[planID] = plan
		store.AddAudit(state, s.ids, "plan", planID, "threshold_template_applied", now, map[string]any{
			"template_id":           template.ID,
			"template_version":      version.Version,
			"minimum_success_ratio": version.MinimumSuccessRatio,
			"maximum_latency_ms":    version.MaximumLatencyMS,
		})
		updated = plan
		return nil
	})
	return updated, err
}

func versionByNumber(
	template domain.ThresholdTemplate,
	number int,
) (domain.ThresholdTemplateVersion, bool) {
	for _, version := range template.Versions {
		if version.Version == number {
			return version, true
		}
	}
	return domain.ThresholdTemplateVersion{}, false
}

func templateDisabled(templateID string, current domain.ThresholdTemplateState) *domain.DomainError {
	return domain.NewError(domain.CodeTemplateDisabled, "threshold template is disabled", map[string]any{
		"template_id": templateID,
		"state":       current,
	})
}
