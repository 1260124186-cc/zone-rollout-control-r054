package plan

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

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

func (s *Service) Create(ctx context.Context, input domain.PlanCreateInput) (domain.RolloutPlan, error) {
	if err := domain.ValidatePlanInput(input); err != nil {
		return domain.RolloutPlan{}, err
	}
	input = domain.NormalizePlanInput(input)
	now := s.clock.Now().UTC()
	plan := domain.RolloutPlan{
		ID:            s.ids.New("pln"),
		Zone:          input.Zone,
		State:         domain.PlanDraft,
		ExpectedNodes: input.ExpectedNodes,
		Version:       1,
		CreatedAt:     now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		if input.ThresholdTemplateID != "" {
			// 引用模板时在同一事务内解析并快照阈值，之后模板变化不影响计划。
			resolved, err := domain.ResolveActiveTemplateVersion(state, input.ThresholdTemplateID, input.ThresholdVersion)
			if err != nil {
				return err
			}
			plan.MinimumSuccessRatio = resolved.MinimumSuccessRatio
			plan.MaximumLatencyMS = resolved.MaximumLatencyMS
			plan.ThresholdTemplateID = resolved.TemplateID
			plan.ThresholdVersion = resolved.Version
		} else {
			plan.MinimumSuccessRatio = input.MinimumSuccessRatio
			plan.MaximumLatencyMS = input.MaximumLatencyMS
		}
		state.Plans[plan.ID] = plan
		attributes := map[string]any{"zone": plan.Zone}
		if plan.ThresholdTemplateID != "" {
			attributes["threshold_template_id"] = plan.ThresholdTemplateID
			attributes["threshold_version"] = plan.ThresholdVersion
		}
		store.AddAudit(state, s.ids, "plan", plan.ID, "created", now, attributes)
		return nil
	})
	return plan, err
}

// ApplyThreshold 把模板版本的阈值快照应用到草稿计划并锁定该版本。
// 对同一版本重复应用是幂等的：直接返回计划，不提升版本。
func (s *Service) ApplyThreshold(
	ctx context.Context,
	planID string,
	input domain.ApplyThresholdInput,
) (domain.RolloutPlan, error) {
	if err := domain.ValidateApplyThresholdInput(input); err != nil {
		return domain.RolloutPlan{}, err
	}
	var plan domain.RolloutPlan
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if current.Version != input.ExpectedPlanVersion {
			return versionConflict(planID, input.ExpectedPlanVersion, current.Version)
		}
		if current.State != domain.PlanDraft {
			return domain.InvalidState("threshold template can be applied only while plan is draft", map[string]any{
				"plan_id": planID,
				"state":   current.State,
			})
		}
		resolved, err := domain.ResolveActiveTemplateVersion(state, input.TemplateID, input.Version)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		// 已锁定同一模板版本：幂等返回，保持计划版本不变。
		if current.ThresholdTemplateID == resolved.TemplateID && current.ThresholdVersion == resolved.Version {
			plan = current
			return nil
		}
		current.MinimumSuccessRatio = resolved.MinimumSuccessRatio
		current.MaximumLatencyMS = resolved.MaximumLatencyMS
		current.ThresholdTemplateID = resolved.TemplateID
		current.ThresholdVersion = resolved.Version
		current.Version++
		state.Plans[planID] = current
		store.AddAudit(state, s.ids, "plan", planID, "threshold_template_applied", now, map[string]any{
			"threshold_template_id": resolved.TemplateID,
			"threshold_version":     resolved.Version,
		})
		plan = current
		return nil
	})
	return plan, err
}

func (s *Service) AddRevision(
	ctx context.Context,
	planID string,
	input domain.RevisionCreateInput,
) (domain.ZoneRevision, error) {
	normalized, err := domain.NormalizeRevisionInput(input)
	if err != nil {
		return domain.ZoneRevision{}, err
	}
	if err := domain.ValidateRevisionInput(normalized); err != nil {
		return domain.ZoneRevision{}, err
	}
	now := s.clock.Now().UTC()
	var created domain.ZoneRevision
	err = s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if plan.State != domain.PlanDraft {
			return domain.InvalidState("revisions can be added only while plan is draft", map[string]any{
				"plan_id": planID,
				"state":   plan.State,
			})
		}
		for _, revision := range state.Revisions {
			if revision.PlanID != planID {
				continue
			}
			if revision.Serial == normalized.Serial {
				return domain.NewError(domain.CodeDuplicateRevision, "plan already contains this serial", map[string]any{
					"plan_id": planID,
					"serial":  normalized.Serial,
				})
			}
			if revision.Serial > normalized.Serial {
				return domain.Validation("revision serial must increase", map[string]any{
					"serial": normalized.Serial,
					"latest": revision.Serial,
				})
			}
		}
		created = domain.ZoneRevision{
			ID:         s.ids.New("rev"),
			PlanID:     planID,
			Serial:     normalized.Serial,
			Authority:  normalized.Authority,
			Entries:    append([]domain.ZoneEntry(nil), normalized.Entries...),
			EntryCount: len(normalized.Entries),
			CreatedAt:  now,
		}
		state.Revisions[created.ID] = created
		plan.LatestRevisionID = created.ID
		plan.Version++
		state.Plans[planID] = plan
		store.AddAudit(state, s.ids, "revision", created.ID, "added", now, map[string]any{
			"plan_id": planID,
			"serial":  created.Serial,
		})
		return nil
	})
	return created, err
}

func (s *Service) Activate(
	ctx context.Context,
	planID string,
	expectedVersion uint64,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if current.Version != expectedVersion {
			return versionConflict(planID, expectedVersion, current.Version)
		}
		now := s.clock.Now().UTC()
		if err := domain.ActivatePlan(&current, now); err != nil {
			return err
		}
		state.Plans[planID] = current
		store.AddAudit(state, s.ids, "plan", planID, "activated", now, map[string]any{
			"version": current.Version,
		})
		plan = current
		return nil
	})
	return plan, err
}

func (s *Service) Complete(
	ctx context.Context,
	planID string,
	expectedVersion uint64,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if current.Version != expectedVersion {
			return versionConflict(planID, expectedVersion, current.Version)
		}
		now := s.clock.Now().UTC()
		if err := domain.CompletePlan(&current, now); err != nil {
			return err
		}
		state.Plans[planID] = current
		store.AddAudit(state, s.ids, "plan", planID, "completed", now, map[string]any{
			"version": current.Version,
		})
		plan = current
		return nil
	})
	return plan, err
}

func (s *Service) Get(ctx context.Context, planID string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		plan = found
		return nil
	})
	return plan, err
}

func (s *Service) LatestRevision(
	ctx context.Context,
	planID string,
) (domain.ZoneRevision, error) {
	var revision domain.ZoneRevision
	err := s.store.Read(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if plan.LatestRevisionID == "" {
			return domain.NotFound("revision", planID)
		}
		found, ok := state.Revisions[plan.LatestRevisionID]
		if !ok {
			return domain.NewError(domain.CodeStorageFailure, "plan references a missing revision", map[string]any{
				"revision_id": plan.LatestRevisionID,
			})
		}
		revision = found
		return nil
	})
	return revision, err
}

func (s *Service) List(
	ctx context.Context,
	stateFilter domain.PlanState,
	limit int,
	cursor string,
) (domain.PlanPage, error) {
	if limit < 1 || limit > 100 {
		return domain.PlanPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.PlanPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.PlanPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.RolloutPlan, 0, len(state.Plans))
		for _, plan := range state.Plans {
			if stateFilter != "" && plan.State != stateFilter {
				continue
			}
			if after != "" && plan.ID <= after {
				continue
			}
			items = append(items, plan)
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

func (s *Service) RevisionExists(
	ctx context.Context,
	planID string,
	serial uint32,
	authority string,
	entries []domain.ZoneEntry,
) (bool, error) {
	exists := false
	err := s.store.Read(ctx, func(state *domain.State) error {
		for _, revision := range state.Revisions {
			if revision.PlanID != planID || revision.Serial != serial || revision.Authority != authority {
				continue
			}
			if reflect.DeepEqual(revision.Entries, entries) {
				exists = true
				return nil
			}
		}
		return nil
	})
	return exists, err
}

func RevisionDigest(revision domain.ZoneRevision) string {
	payload, _ := json.Marshal(struct {
		Serial    uint32
		Authority string
		Entries   []domain.ZoneEntry
	}{
		Serial:    revision.Serial,
		Authority: revision.Authority,
		Entries:   revision.Entries,
	})
	return string(payload)
}

func versionConflict(id string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "plan version does not match", map[string]any{
		"plan_id":  id,
		"expected": expected,
		"actual":   actual,
	})
}
