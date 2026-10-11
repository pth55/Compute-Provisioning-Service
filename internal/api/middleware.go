package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cps/internal/model"
)

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func AccessLog(logger *slog.Logger, next http.Handler, audit ...func(context.Context, model.Event) error) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			buf := make([]byte, 8)
			if _, err := rand.Read(buf); err == nil {
				requestID = hex.EncodeToString(buf)
			} else {
				requestID = "generated-locally"
			}
		}
		w.Header().Set("X-Request-ID", requestID)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		duration := time.Since(start)
		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		} else if sw.status >= 400 {
			level = slog.LevelWarn
		}
		logger.Log(r.Context(), level, "HTTP request completed", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.bytes, "duration_ms", duration.Milliseconds(), "remote_addr", remoteHost(r.RemoteAddr), "user_agent", r.UserAgent())
		if len(audit) > 0 && audit[0] != nil {
			eventLevel := "INFO"
			if sw.status >= 500 {
				eventLevel = "ERROR"
			} else if sw.status >= 400 {
				eventLevel = "WARN"
			}
			instanceID := instanceIDFromPath(r.URL.Path)
			message := r.Method + " " + r.URL.Path + " status=" + strconv.Itoa(sw.status) + " duration_ms=" + strconv.FormatInt(duration.Milliseconds(), 10) + " remote=" + remoteHost(r.RemoteAddr) + " request_id=" + requestID
			auditCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := audit[0](auditCtx, model.Event{InstanceID: instanceID, Level: eventLevel, Component: "http", Event: "HTTP_REQUEST", Message: message, CreatedAt: time.Now().UTC()}); err != nil {
				logger.Error("could not persist HTTP request audit event", "request_id", requestID, "error", err)
			}
			cancel()
		}
	})
}
func remoteHost(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

func instanceIDFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "instances" && strings.HasPrefix(parts[1], "i-") {
		return parts[1]
	}
	return ""
}
