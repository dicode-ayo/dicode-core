package webui

import (
	"encoding/json"
	"net/http"
)

// --- request / response types -----------------------------------------------

type taskCreateReq struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type taskCreateResp struct {
	TaskID string   `json:"task_id"`
	Source string   `json:"source"`
	Files  []string `json:"files"`
}

type taskEditReq struct {
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	Prompt    string `json:"prompt"`
}

type taskEditResp struct {
	SessionID   string `json:"session_id"`
	SandboxPath string `json:"sandbox_path"`
	Source      string `json:"source"`
	SourceKind  string `json:"source_kind"`
}

type taskSaveReq struct {
	SessionID string `json:"session_id"`
}

type taskCancelReq struct {
	SessionID string `json:"session_id"`
}

// --- POST /api/task/create --------------------------------------------------

func (s *Server) apiTaskCreate(w http.ResponseWriter, r *http.Request) {
	var req taskCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	res, err := s.CreateTask(r.Context(), req.Name, req.Source)
	if err != nil {
		jsonErr(w, err.Error(), statusFromAuthoringError(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(taskCreateResp{
		TaskID: res.TaskID,
		Source: res.Source,
		Files:  res.Files,
	})
}

// --- POST /api/task/edit ----------------------------------------------------

func (s *Server) apiTaskEdit(w http.ResponseWriter, r *http.Request) {
	var req taskEditReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	res, err := s.EditTask(r.Context(), req.SessionID, req.TaskID)
	if err != nil {
		jsonErr(w, err.Error(), statusFromAuthoringError(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(taskEditResp{
		SessionID:   res.SessionID,
		SandboxPath: res.SandboxPath,
		Source:      res.Source,
		SourceKind:  res.SourceKind,
	})
}

// --- POST /api/task/save ----------------------------------------------------

func (s *Server) apiTaskSave(w http.ResponseWriter, r *http.Request) {
	var req taskSaveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := s.SaveTask(r.Context(), req.SessionID); err != nil {
		jsonErr(w, err.Error(), statusFromAuthoringError(err))
		return
	}

	jsonOK(w, map[string]any{"applied": true})
}

// --- POST /api/task/cancel --------------------------------------------------

func (s *Server) apiTaskCancel(w http.ResponseWriter, r *http.Request) {
	var req taskCancelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := s.CancelTask(r.Context(), req.SessionID); err != nil {
		jsonErr(w, err.Error(), statusFromAuthoringError(err))
		return
	}

	jsonOK(w, map[string]bool{"cancelled": true})
}
