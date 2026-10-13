package api

import (
	"io"
	"net/http"
	"os"
)

// usageDashboardPath is where the cron-pushed static HTML snapshot lives.
// Same /data volume as the sqlite DB, so it survives redeploys.
const usageDashboardPath = "/data/usage-dashboard.html"

const usageDashboardMaxBytes = 2 << 20 // 2MB, generous for a single static page

// usageDashboardPushHandler accepts a raw HTML body (from the AI usage
// telemetry dashboard refresh on the Kali VM) and writes it to disk,
// overwriting the previous snapshot. Protected by the normal Basic Auth
// middleware, same credentials as every other Colosseum route.
func usageDashboardPushHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, usageDashboardMaxBytes+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read body"})
			return
		}
		if len(body) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty body"})
			return
		}
		if len(body) > usageDashboardMaxBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return
		}
		if err := os.WriteFile(usageDashboardPath, body, 0o644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not write snapshot"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	}
}

// usageDashboardViewHandler serves the latest pushed snapshot. Protected
// by the normal Basic Auth middleware -- not in isPublicPath.
func usageDashboardViewHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(usageDashboardPath)
		if err != nil {
			http.Error(w, "No dashboard snapshot yet -- the usage telemetry refresh hasn't pushed one.", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(body)
	}
}
