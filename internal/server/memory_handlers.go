package server

import (
	"encoding/json"
	"net/http"

	"LongCat-frontend/internal/memory"
)

// memoryEntry 记忆列项的 API 表示。
type memoryEntry struct {
	Scope   string `json:"scope"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Updated int64  `json:"updated"`
}

type memoryReadResp struct {
	Content string `json:"content"`
}

type apiError struct {
	Error string `json:"error"`
}

type memoryUpsertBody struct {
	Scope     string `json:"scope"`
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Content   string `json:"content"`
}

type syncRepoBody struct {
	RepoURL string `json:"repo_url"`
}

type syncResult struct {
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// writeJSON 复用 server.go 中的同名函数。

func (a *api) memoryList(w http.ResponseWriter, r *http.Request) {
	if a.memory == nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "memory not initialized"})
		return
	}
	ws := r.URL.Query().Get("workspace")
	entries, err := a.memory.List(ws)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	out := make([]memoryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, memoryEntry{Scope: e.Scope, Name: e.Name, Size: e.Size, Updated: e.Updated})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (a *api) memoryRead(w http.ResponseWriter, r *http.Request) {
	if a.memory == nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "memory not initialized"})
		return
	}
	q := r.URL.Query()
	content, err := a.memory.Read(q.Get("scope"), q.Get("workspace"), q.Get("name"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, memoryReadResp{Content: content})
}

func (a *api) memoryCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decodeMemoryBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	entry, err := a.memory.Create(body.Scope, body.Workspace, body.Name, body.Content)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toMemoryEntry(entry))
}

func (a *api) memoryUpdate(w http.ResponseWriter, r *http.Request) {
	body, err := decodeMemoryBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	entry, err := a.memory.Update(body.Scope, body.Workspace, body.Name, body.Content)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toMemoryEntry(entry))
}

func (a *api) memoryDelete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := a.memory.Delete(q.Get("scope"), q.Get("workspace"), q.Get("name")); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func decodeMemoryBody(r *http.Request) (memoryUpsertBody, error) {
	var b memoryUpsertBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		return b, err
	}
	return b, nil
}

func toMemoryEntry(e memory.Entry) memoryEntry {
	return memoryEntry{Scope: e.Scope, Name: e.Name, Size: e.Size, Updated: e.Updated}
}

// ---- 云同步（git CLI，手动触发） ----

func (a *api) memorySyncStatus(w http.ResponseWriter, r *http.Request) {
	if a.memory == nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "memory not initialized"})
		return
	}
	writeJSON(w, http.StatusOK, a.memory.SyncStatus())
}

func (a *api) memorySyncRepo(w http.ResponseWriter, r *http.Request) {
	var b syncRepoBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	if err := a.memory.SetRepo(b.RepoURL); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *api) memorySyncPush(w http.ResponseWriter, r *http.Request) {
	if a.memory == nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "memory not initialized"})
		return
	}
	msg, err := a.memory.SyncPush()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, syncResult{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, syncResult{Message: msg})
}

func (a *api) memorySyncPull(w http.ResponseWriter, r *http.Request) {
	if a.memory == nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "memory not initialized"})
		return
	}
	msg, err := a.memory.SyncPull()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, syncResult{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, syncResult{Message: msg})
}
