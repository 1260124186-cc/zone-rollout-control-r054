package httpapi

import (
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) createThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	var input domain.ThresholdTemplateCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.thresholds.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, view)
}

func (s *Server) addThresholdVersion(w http.ResponseWriter, r *http.Request) {
	var input domain.ThresholdVersionCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	version, err := s.thresholds.AddVersion(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, version)
}

func (s *Server) disableThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	view, err := s.thresholds.Disable(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) getThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	view, err := s.thresholds.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) listThresholdTemplates(w http.ResponseWriter, r *http.Request) {
	views, err := s.thresholds.List(r.Context())
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, map[string]any{"items": views})
}

func (s *Server) applyThresholdTemplate(w http.ResponseWriter, r *http.Request) {
	var input domain.ApplyThresholdInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	plan, err := s.plans.ApplyThreshold(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, plan)
}
