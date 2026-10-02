package domain

import (
	"strconv"
	"time"
)

// 阈值模板的可应用状态。停用只阻止新的引用与应用，
// 已锁定具体版本的计划不受影响。
type ThresholdTemplateState string

const (
	ThresholdTemplateActive   ThresholdTemplateState = "active"
	ThresholdTemplateDisabled ThresholdTemplateState = "disabled"
)

// ThresholdTemplate 是按业务等级复用的一致性阈值定义。
// 具体阈值内容存放在不可变的版本中，模板本身只承载名称、
// 适用范围摘要、状态和最新版本号。
type ThresholdTemplate struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Scope         string                 `json:"scope"`
	State         ThresholdTemplateState `json:"state"`
	LatestVersion int                    `json:"latest_version"`
	CreatedAt     time.Time              `json:"created_at"`
	DisabledAt    *time.Time             `json:"disabled_at,omitempty"`
}

// ThresholdTemplateVersion 一经创建即不可变。计划应用模板时
// 锁定的就是这个版本记录，模板后续新增版本或停用都不会回改它。
type ThresholdTemplateVersion struct {
	ID                  string    `json:"id"`
	TemplateID          string    `json:"template_id"`
	Version             int       `json:"version"`
	MinimumSuccessRatio float64   `json:"minimum_success_ratio"`
	MaximumLatencyMS    int       `json:"maximum_latency_ms"`
	Scope               string    `json:"scope"`
	CreatedAt           time.Time `json:"created_at"`
}

// ThresholdTemplateView 聚合模板与其全部版本，版本按版本号升序返回。
type ThresholdTemplateView struct {
	ThresholdTemplate
	Versions []ThresholdTemplateVersion `json:"versions"`
}

// TemplateVersionIndexKey 是模板标识与版本号组成的稳定复合索引键。
func TemplateVersionIndexKey(templateID string, version int) string {
	return templateID + "/v" + strconv.Itoa(version)
}

// ResolveActiveTemplateVersion 在状态快照内解析模板版本。
// version 为 0 表示最新版本。模板不存在、已停用或版本缺失时
// 返回稳定的领域错误，供创建计划与应用模板两条路径复用。
func ResolveActiveTemplateVersion(state *State, templateID string, version int) (ThresholdTemplateVersion, error) {
	template, ok := state.ThresholdTemplates[templateID]
	if !ok {
		return ThresholdTemplateVersion{}, NotFound("threshold_template", templateID)
	}
	if template.State != ThresholdTemplateActive {
		return ThresholdTemplateVersion{}, NewError(CodeTemplateDisabled, "threshold template is disabled", map[string]any{
			"template_id": templateID,
		})
	}
	if version == 0 {
		version = template.LatestVersion
	}
	versionID, ok := state.ThresholdVersionIndex[TemplateVersionIndexKey(templateID, version)]
	if !ok {
		return ThresholdTemplateVersion{}, NotFound("threshold_template_version", TemplateVersionIndexKey(templateID, version))
	}
	resolved, ok := state.ThresholdVersions[versionID]
	if !ok {
		return ThresholdTemplateVersion{}, NewError(CodeStorageFailure, "template index references a missing version", map[string]any{
			"template_id": templateID,
			"version":     version,
		})
	}
	return resolved, nil
}
