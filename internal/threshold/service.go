package threshold

import (
	"context"
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

// Create 建立模板并写入其首个不可变版本（版本号 1）。
func (s *Service) Create(
	ctx context.Context,
	input domain.ThresholdTemplateCreateInput,
) (domain.ThresholdTemplateView, error) {
	if err := domain.ValidateThresholdTemplateInput(input); err != nil {
		return domain.ThresholdTemplateView{}, err
	}
	input = domain.NormalizeThresholdTemplateInput(input)
	now := s.clock.Now().UTC()
	var view domain.ThresholdTemplateView
	err := s.store.Write(ctx, func(state *domain.State) error {
		if existingID, taken := state.ThresholdTemplateNames[input.Name]; taken {
			return domain.NewError(domain.CodeDuplicateTemplate, "threshold template name is already in use", map[string]any{
				"name":        input.Name,
				"template_id": existingID,
			})
		}
		template := domain.ThresholdTemplate{
			ID:            s.ids.New("tpl"),
			Name:          input.Name,
			Scope:         input.Scope,
			State:         domain.ThresholdTemplateActive,
			LatestVersion: 1,
			CreatedAt:     now,
		}
		first := domain.ThresholdTemplateVersion{
			ID:                  s.ids.New("tpv"),
			TemplateID:          template.ID,
			Version:             1,
			MinimumSuccessRatio: input.MinimumSuccessRatio,
			MaximumLatencyMS:    input.MaximumLatencyMS,
			Scope:               input.Scope,
			CreatedAt:           now,
		}
		state.ThresholdTemplates[template.ID] = template
		state.ThresholdVersions[first.ID] = first
		state.ThresholdVersionIndex[domain.TemplateVersionIndexKey(template.ID, 1)] = first.ID
		state.ThresholdTemplateNames[template.Name] = template.ID
		store.AddAudit(state, s.ids, "threshold_template", template.ID, "created", now, map[string]any{
			"name":  template.Name,
			"scope": template.Scope,
		})
		view = assembleView(template, []domain.ThresholdTemplateVersion{first})
		return nil
	})
	return view, err
}

// AddVersion 为模板追加不可变版本；适用范围缺省继承最新版本。
// 内容与任一既有版本完全相同时拒绝，停用模板不能继续出版本。
func (s *Service) AddVersion(
	ctx context.Context,
	templateID string,
	input domain.ThresholdVersionCreateInput,
) (domain.ThresholdTemplateVersion, error) {
	if err := domain.ValidateThresholdVersionInput(input); err != nil {
		return domain.ThresholdTemplateVersion{}, err
	}
	input = domain.NormalizeThresholdVersionInput(input)
	now := s.clock.Now().UTC()
	var created domain.ThresholdTemplateVersion
	err := s.store.Write(ctx, func(state *domain.State) error {
		template, ok := state.ThresholdTemplates[templateID]
		if !ok {
			return domain.NotFound("threshold_template", templateID)
		}
		if template.State != domain.ThresholdTemplateActive {
			return domain.NewError(domain.CodeTemplateDisabled, "threshold template is disabled", map[string]any{
				"template_id": templateID,
			})
		}
		latestID := state.ThresholdVersionIndex[domain.TemplateVersionIndexKey(templateID, template.LatestVersion)]
		latest, ok := state.ThresholdVersions[latestID]
		if !ok {
			return domain.NewError(domain.CodeStorageFailure, "template references a missing version", map[string]any{
				"template_id": templateID,
				"version":     template.LatestVersion,
			})
		}
		if input.Scope == "" {
			input.Scope = latest.Scope
		}
		for version := 1; version <= template.LatestVersion; version++ {
			existingID := state.ThresholdVersionIndex[domain.TemplateVersionIndexKey(templateID, version)]
			existing, ok := state.ThresholdVersions[existingID]
			if !ok {
				continue
			}
			if existing.MinimumSuccessRatio == input.MinimumSuccessRatio &&
				existing.MaximumLatencyMS == input.MaximumLatencyMS &&
				existing.Scope == input.Scope {
				return domain.NewError(domain.CodeDuplicateTemplate, "template already contains a version with the same rules", map[string]any{
					"template_id": templateID,
					"version":     version,
				})
			}
		}
		template.LatestVersion++
		template.Scope = input.Scope
		created = domain.ThresholdTemplateVersion{
			ID:                  s.ids.New("tpv"),
			TemplateID:          templateID,
			Version:             template.LatestVersion,
			MinimumSuccessRatio: input.MinimumSuccessRatio,
			MaximumLatencyMS:    input.MaximumLatencyMS,
			Scope:               input.Scope,
			CreatedAt:           now,
		}
		state.ThresholdVersions[created.ID] = created
		state.ThresholdVersionIndex[domain.TemplateVersionIndexKey(templateID, created.Version)] = created.ID
		state.ThresholdTemplates[templateID] = template
		store.AddAudit(state, s.ids, "threshold_template", templateID, "version_added", now, map[string]any{
			"version": created.Version,
		})
		return nil
	})
	return created, err
}

// Disable 停用模板。停用是幂等的：重复停用直接返回当前视图，
// 不产生额外状态变更，也不会影响已锁定版本的计划。
func (s *Service) Disable(ctx context.Context, templateID string) (domain.ThresholdTemplateView, error) {
	now := s.clock.Now().UTC()
	var view domain.ThresholdTemplateView
	err := s.store.Write(ctx, func(state *domain.State) error {
		template, ok := state.ThresholdTemplates[templateID]
		if !ok {
			return domain.NotFound("threshold_template", templateID)
		}
		versions := collectVersions(state, templateID, template.LatestVersion)
		if template.State == domain.ThresholdTemplateDisabled {
			view = assembleView(template, versions)
			return nil
		}
		template.State = domain.ThresholdTemplateDisabled
		disabledAt := now
		template.DisabledAt = &disabledAt
		state.ThresholdTemplates[templateID] = template
		store.AddAudit(state, s.ids, "threshold_template", templateID, "disabled", now, nil)
		view = assembleView(template, versions)
		return nil
	})
	return view, err
}

func (s *Service) Get(ctx context.Context, templateID string) (domain.ThresholdTemplateView, error) {
	var view domain.ThresholdTemplateView
	err := s.store.Read(ctx, func(state *domain.State) error {
		template, ok := state.ThresholdTemplates[templateID]
		if !ok {
			return domain.NotFound("threshold_template", templateID)
		}
		view = assembleView(template, collectVersions(state, templateID, template.LatestVersion))
		return nil
	})
	return view, err
}

func (s *Service) List(ctx context.Context) ([]domain.ThresholdTemplateView, error) {
	views := []domain.ThresholdTemplateView{}
	err := s.store.Read(ctx, func(state *domain.State) error {
		for _, template := range state.ThresholdTemplates {
			views = append(views, assembleView(template, collectVersions(state, template.ID, template.LatestVersion)))
		}
		sort.Slice(views, func(i, j int) bool {
			return views[i].ID < views[j].ID
		})
		return nil
	})
	return views, err
}

func collectVersions(state *domain.State, templateID string, latest int) []domain.ThresholdTemplateVersion {
	versions := make([]domain.ThresholdTemplateVersion, 0, latest)
	for version := 1; version <= latest; version++ {
		versionID := state.ThresholdVersionIndex[domain.TemplateVersionIndexKey(templateID, version)]
		found, ok := state.ThresholdVersions[versionID]
		if ok {
			versions = append(versions, found)
		}
	}
	return versions
}

func assembleView(
	template domain.ThresholdTemplate,
	versions []domain.ThresholdTemplateVersion,
) domain.ThresholdTemplateView {
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version < versions[j].Version
	})
	return domain.ThresholdTemplateView{
		ThresholdTemplate: template,
		Versions:          versions,
	}
}
