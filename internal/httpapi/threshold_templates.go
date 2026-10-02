package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/threshold"
)

func (s *Server) createThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	var input domain.ThresholdTemplateCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	template, err := s.thresholdTemplates.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, template)
}

func (s *Server) listThresholdTemplates(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	state := domain.ThresholdTemplateState(strings.TrimSpace(r.URL.Query().Get("state")))
	page, err := s.thresholdTemplates.List(
		r.Context(),
		state,
		limit,
		r.URL.Query().Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	template, err := s.thresholdTemplates.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, template)
}

func (s *Server) addThresholdTemplateVersion(w http.ResponseWriter, r *http.Request) {
	var input domain.ThresholdTemplateVersionInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	template, err := s.thresholdTemplates.AddVersion(
		r.Context(),
		r.PathValue("id"),
		input,
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, template)
}

func (s *Server) disableThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	template, err := s.thresholdTemplates.Disable(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, template)
}

func (s *Server) applyThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	var input threshold.ApplyInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	plan, err := s.thresholdTemplates.ApplyToPlan(
		r.Context(),
		r.PathValue("id"),
		input,
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, plan)
}
