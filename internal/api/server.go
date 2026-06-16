package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adevireddy/colosseum/internal/providers"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	r             chi.Router
	DB            *sql.DB
	WorkspaceRoot string
	Providers     map[string]bool
	ProviderMap   map[string]providers.Client
}

func NewServer(
	db *sql.DB,
	workspaceRoot string,
	providers map[string]bool,
	openAIKey string,
	anthropicKey string,
	secretKey string,
	apiAuthToken string,
	providerMap map[string]providers.Client,
	basicAuthUser string,
	basicAuthPass string,
) *Server {
	s := &Server{DB: db, WorkspaceRoot: workspaceRoot, Providers: providers, ProviderMap: providerMap}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger())
	r.Use(timeoutExceptStream(120 * time.Second))
	r.Use(customDomainRedirect(db))
	if strings.TrimSpace(basicAuthUser) != "" && strings.TrimSpace(basicAuthPass) != "" {
		r.Use(basicAuthMiddleware(basicAuthUser, basicAuthPass))
	}
	if strings.TrimSpace(apiAuthToken) != "" {
		r.Use(apiAuthMiddleware(apiAuthToken))
	}

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	registerAPIRoutes(r, db, workspaceRoot, providers, openAIKey, anthropicKey, secretKey, providerMap)
	mountUI(r)

	s.r = r
	return s
}

func (s *Server) Handler() http.Handler { return s.r }

func mountUI(r chi.Router) {
	sub, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		r.Get("/*", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("UI assets not embedded yet. Run: make ui-build && make build"))
		})
		return
	}

	fsHandler := http.FileServer(http.FS(sub))

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		content, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(content)
	})

	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		if _, err := sub.Open(r.URL.Path[1:]); err == nil {
			fsHandler.ServeHTTP(w, r)
			return
		}
		content, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(content)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func timeoutExceptStream(d time.Duration) func(http.Handler) http.Handler {
	timeoutMW := middleware.Timeout(d)
	return func(next http.Handler) http.Handler {
		wrapped := timeoutMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/stream/") {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

type basicAuthPassedKey struct{}

func isPublicPath(path string) bool {
	return strings.HasPrefix(path, "/c/") ||
		strings.HasPrefix(path, "/api/public/") ||
		strings.HasPrefix(path, "/voice/") ||
		path == "/sms/inbound" ||
		path == "/whatsapp/inbound"
}

func basicAuthMiddleware(user, pass string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Colosseum"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), basicAuthPassedKey{}, true)))
		})
	}
}

// customDomainRedirect looks up agents by custom_domain and redirects to /c/:agentID.
func customDomainRedirect(db *sql.DB) func(http.Handler) http.Handler {
	type entry struct {
		agentID string
		exp     time.Time
	}
	var mu sync.RWMutex
	cache := map[string]entry{}

	lookup := func(host string) string {
		mu.RLock()
		if e, ok := cache[host]; ok && time.Now().Before(e.exp) {
			mu.RUnlock()
			return e.agentID
		}
		mu.RUnlock()

		var id string
		_ = db.QueryRow(
			"SELECT id FROM agents WHERE custom_domain=? AND is_public=1 LIMIT 1", host,
		).Scan(&id)

		mu.Lock()
		cache[host] = entry{agentID: id, exp: time.Now().Add(60 * time.Second)}
		mu.Unlock()
		return id
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if i := strings.IndexByte(host, ':'); i != -1 {
				host = host[:i]
			}
			if host != "" && r.URL.Path == "/" {
				if agentID := lookup(host); agentID != "" {
					http.Redirect(w, r, "/c/"+agentID, http.StatusFound)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func apiAuthMiddleware(apiAuthToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			if isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if r.Context().Value(basicAuthPassedKey{}) == true {
				next.ServeHTTP(w, r)
				return
			}
			var token string
			authz := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(authz, "Bearer ") {
				token = strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
			}
			if token == "" {
				token = strings.TrimSpace(r.Header.Get("X-API-Token"))
			}
			if token == "" && strings.HasPrefix(r.URL.Path, "/api/stream/") {
				// Native EventSource cannot set Authorization headers, so allow
				// an explicit stream token when API auth is enabled.
				token = strings.TrimSpace(r.URL.Query().Get("access_token"))
			}
			if token != strings.TrimSpace(apiAuthToken) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func requestLogger() func(http.Handler) http.Handler {
	const slowRequestThreshold = 1000 * time.Millisecond

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			duration := time.Since(start)
			if !shouldLogRequest(r.Method, r.URL.Path, status, duration) {
				return
			}
			level := "INFO"
			if status >= http.StatusInternalServerError {
				level = "ERROR"
			} else if status >= http.StatusBadRequest || duration >= slowRequestThreshold {
				level = "WARN"
			}
			remoteIP := r.RemoteAddr
			if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				remoteIP = host
			}
			log.Printf(
				"level=%s msg=\"http_request\" method=%s path=%s status=%d bytes=%d duration_ms=%d remote_ip=%s request_id=%s",
				level,
				r.Method,
				r.URL.Path,
				status,
				ww.BytesWritten(),
				duration.Milliseconds(),
				remoteIP,
				middleware.GetReqID(r.Context()),
			)
		})
	}
}

func shouldLogRequest(method, path string, status int, duration time.Duration) bool {
	if strings.HasPrefix(path, "/healthz") || strings.HasPrefix(path, "/readyz") {
		return false
	}
	if strings.HasPrefix(path, "/api/stream/") {
		return status >= http.StatusBadRequest
	}
	// Keep hot polling endpoints quiet unless they are slow or failing.
	if strings.HasSuffix(path, "/telemetry") || strings.HasSuffix(path, "/artifacts") {
		return status >= http.StatusBadRequest || duration >= 1500*time.Millisecond
	}
	// For GET/HEAD traffic, only log slow/failing requests.
	if method == http.MethodGet || method == http.MethodHead {
		return status >= http.StatusBadRequest || duration >= 1000*time.Millisecond
	}
	// Always log mutating/operational requests.
	return true
}
