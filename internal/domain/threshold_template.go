package domain

import (
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxTemplateNameRunes        = 120
	MaxTemplateDescriptionRunes = 480
	MaxTemplateScopeZones       = 500
)

type ThresholdTemplateCreateInput struct {
	Name                string             `json:"name"`
	Description         string             `json:"description"`
	MinimumSuccessRatio float64            `json:"minimum_success_ratio"`
	MaximumLatencyMS    int                `json:"maximum_latency_ms"`
	Scope               TemplateScopeInput `json:"scope"`
}

type ThresholdTemplateVersionInput struct {
	MinimumSuccessRatio float64            `json:"minimum_success_ratio"`
	MaximumLatencyMS    int                `json:"maximum_latency_ms"`
	Scope               TemplateScopeInput `json:"scope"`
}

// TemplateScopeInput is the client-supplied applicability of a version.
// An empty object defaults to the global scope; an explicit global scope
// must not carry zones, and a zones scope must list at least one zone.
type TemplateScopeInput struct {
	Type  string   `json:"type"`
	Zones []string `json:"zones"`
}

func NormalizeThresholdTemplateCreateInput(
	input ThresholdTemplateCreateInput,
) ThresholdTemplateCreateInput {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Scope = normalizeScopeInput(input.Scope)
	return input
}

func NormalizeThresholdTemplateVersionInput(
	input ThresholdTemplateVersionInput,
) ThresholdTemplateVersionInput {
	input.Scope = normalizeScopeInput(input.Scope)
	return input
}

func normalizeScopeInput(input TemplateScopeInput) TemplateScopeInput {
	input.Type = strings.TrimSpace(strings.ToLower(input.Type))
	if input.Type == "" {
		input.Type = string(TemplateScopeGlobal)
	}
	seen := make(map[string]struct{}, len(input.Zones))
	zones := make([]string, 0, len(input.Zones))
	for _, zone := range input.Zones {
		normalized := canonicalName(zone)
		if normalized == "" {
			zones = append(zones, normalized)
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		zones = append(zones, normalized)
	}
	sort.Strings(zones)
	input.Zones = zones
	return input
}

func ValidateThresholdValues(minimumRatio float64, maximumLatency int) map[string]any {
	details := make(map[string]any)
	if minimumRatio < 0 || minimumRatio > 1 {
		details["minimum_success_ratio"] = "must be between 0 and 1"
	}
	if maximumLatency < 1 || maximumLatency > 600000 {
		details["maximum_latency_ms"] = "must be between 1 and 600000"
	}
	return details
}

func ValidateThresholdTemplateCreateInput(input ThresholdTemplateCreateInput) error {
	details := make(map[string]any)
	if count := utf8.RuneCountInString(input.Name); count < 2 || count > MaxTemplateNameRunes {
		details["name"] = "must contain between 2 and 120 characters"
	}
	if utf8.RuneCountInString(input.Description) > MaxTemplateDescriptionRunes {
		details["description"] = "must be at most 480 characters"
	}
	for field, message := range ValidateThresholdValues(input.MinimumSuccessRatio, input.MaximumLatencyMS) {
		details[field] = message
	}
	if scopeDetails, valid := validateScopeInput(input.Scope); !valid {
		details["scope"] = scopeDetails
	}
	if len(details) > 0 {
		return Validation("threshold template input is invalid", details)
	}
	return nil
}

func ValidateThresholdTemplateVersionInput(input ThresholdTemplateVersionInput) error {
	details := ValidateThresholdValues(input.MinimumSuccessRatio, input.MaximumLatencyMS)
	if scopeDetails, valid := validateScopeInput(input.Scope); !valid {
		details["scope"] = scopeDetails
	}
	if len(details) > 0 {
		return Validation("threshold template version input is invalid", details)
	}
	return nil
}

func validateScopeInput(input TemplateScopeInput) (map[string]any, bool) {
	switch TemplateScopeType(input.Type) {
	case TemplateScopeGlobal:
		if len(input.Zones) > 0 {
			return map[string]any{"scope": "global scope must not list zones"}, false
		}
		return nil, true
	case TemplateScopeZones:
		if len(input.Zones) == 0 {
			return map[string]any{"scope": "zones scope must list at least one zone"}, false
		}
		if len(input.Zones) > MaxTemplateScopeZones {
			return map[string]any{"scope": "scope must list at most 500 zones"}, false
		}
		zoneDetails := make(map[string]any)
		for _, zone := range input.Zones {
			if err := validateCanonicalName(zone); err != nil {
				zoneDetails[zone] = err.Error()
			}
		}
		if len(zoneDetails) > 0 {
			return map[string]any{"zones": zoneDetails}, false
		}
		return nil, true
	default:
		return map[string]any{"type": "must be \"global\" or \"zones\""}, false
	}
}

func ScopeFromInput(input TemplateScopeInput) TemplateScope {
	return TemplateScope{
		Type:  TemplateScopeType(input.Type),
		Zones: append([]string(nil), input.Zones...),
	}
}

// ScopeAppliesToZone reports whether a version's scope covers the plan zone.
func ScopeAppliesToZone(scope TemplateScope, zone string) bool {
	if scope.Type == TemplateScopeGlobal {
		return true
	}
	for _, scopedZone := range scope.Zones {
		if scopedZone == zone {
			return true
		}
	}
	return false
}
