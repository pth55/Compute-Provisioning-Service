package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cps/internal/db"
	"cps/internal/model"
	"cps/internal/service"
)

type Server struct {
	Service *service.Service
	Store   *db.Store
	Logger  *slog.Logger
}

func NewServer(svc *service.Service, store *db.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Service: svc, Store: store, Logger: logger}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metadata", s.handleMetadata)
	mux.HandleFunc("GET /images", s.handleImages)
	mux.HandleFunc("GET /inventory", s.handleInventory)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /instances", s.handleListInstances)
	mux.HandleFunc("POST /instances", s.handleCreateInstance)
	mux.HandleFunc("GET /instances/{id}", s.handleGetInstance)
	mux.HandleFunc("PUT /instances/{id}", s.handleUpdateInstance)
	mux.HandleFunc("POST /instances/{id}/actions", s.handleInstanceAction)
	// Keep a singular alias for calls from the early prototype.
	mux.HandleFunc("POST /instances/{id}/action", s.handleInstanceAction)
	mux.HandleFunc("DELETE /instances/{id}", s.handleDecommission)
	mux.HandleFunc("GET /instances/{id}/logs", s.handleInstanceLogs)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "route_not_found", "no API route matches this path")
	})
	return AccessLog(s.Logger, mux, s.Store.RecordEvent)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	meta, err := s.Service.Metadata(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "metadata_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, meta)
}
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.Service.ListImages(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image_inventory_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": images, "count": len(images)})
}
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	inventory, err := s.Service.Inventory(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inventory_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inventory)
}
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		limit = n
	}
	events, err := s.Service.ListEvents(r.Context(), r.URL.Query().Get("instance_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "events_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.Service.ListInstances(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inventory_failed", err.Error())
		return
	}
	filter := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("state")))
	if filter != "" {
		filtered := make([]model.Instance, 0)
		for _, inst := range instances {
			if string(inst.Status) == filter {
				filtered = append(filtered, inst)
			}
		}
		instances = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances, "count": len(instances)})
}
func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req model.LaunchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	inst, err := s.Service.Create(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "provision_request_rejected", err.Error())
		return
	}
	w.Header().Set("Location", "/instances/"+inst.ID)
	writeJSON(w, http.StatusAccepted, inst)
}
func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Service.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}
func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateInstanceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	inst, err := s.Service.UpdateConfig(r.Context(), r.PathValue("id"), req)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, http.StatusNotFound, "instance_not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "only be changed") || strings.Contains(err.Error(), "not supported") {
			writeError(w, http.StatusConflict, "invalid_configuration_update", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "configuration_update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleInstanceAction(w http.ResponseWriter, r *http.Request) {
	var req model.ActionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	inst, err := s.Service.Action(r.Context(), r.PathValue("id"), req.Action)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, http.StatusNotFound, "instance_not_found", "instance was not found in the inventory")
			return
		}
		if strings.Contains(err.Error(), "not valid") || strings.Contains(err.Error(), "unsupported action") || strings.Contains(err.Error(), "legacy VM") {
			writeError(w, http.StatusConflict, "invalid_lifecycle_action", err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "lifecycle_action_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inst)
}
func (s *Server) handleDecommission(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Service.Decommission(r.Context(), r.PathValue("id"))
	if err != nil {
		if isNotFoundError(err) || strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "instance_not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "decommission_failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, inst)
}
func (s *Server) handleInstanceLogs(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Service.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	kind := strings.ToLower(r.URL.Query().Get("type"))
	if kind == "" {
		kind = "qemu"
	}
	path := ""
	switch kind {
	case "qemu":
		path = inst.LogFile
	case "serial":
		path = inst.SerialLogFile
	case "debug":
		path = inst.DebugLogFile
	default:
		writeError(w, http.StatusBadRequest, "invalid_log_type", "type must be qemu, serial, or debug")
		return
	}
	if path == "" {
		writeError(w, http.StatusNotFound, "log_unavailable", "this instance has no path recorded for that log")
		return
	}
	if !pathWithin(inst.InstanceDir, path) {
		writeError(w, http.StatusForbidden, "unsafe_log_path", "refusing to read a log file outside this instance directory")
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_lines", "lines must be a positive integer")
			return
		}
		limit = n
	}
	if limit > 1000 {
		limit = 1000
	}
	content, err := tailFile(path, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "log_not_found", "log file does not exist yet")
		} else {
			writeError(w, http.StatusInternalServerError, "log_read_failed", err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Log-Type", kind)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, content)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain only one JSON value")
		}
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return nil
}
func isNotFoundError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no rows in result set") || strings.Contains(message, "instance not found")
}

func handleStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		writeError(w, http.StatusRequestTimeout, "request_cancelled", err.Error())
		return
	}
	if strings.Contains(strings.ToLower(err.Error()), "no rows in result set") {
		writeError(w, http.StatusNotFound, "instance_not_found", "instance was not found in the inventory")
		return
	}
	writeError(w, http.StatusInternalServerError, "database_error", err.Error())
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("encode API response", "error", err)
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func pathWithin(parent, candidate string) bool {
	if parent == "" || candidate == "" {
		return false
	}
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(parentAbs, candidateAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func tailFile(path string, limit int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	const maxRead = int64(4 * 1024 * 1024)
	start := int64(0)
	if info.Size() > maxRead {
		start = info.Size() - maxRead
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		return "", err
	}
	text := string(data)
	if start > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}
