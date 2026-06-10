package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ── Public token cache ────────────────────────────────────────────────────────

type publicAgentInfo struct {
	ID             string
	Name           string
	Description    string
	StarterPrompts string
}

var (
	pubTokenCache   = map[string]*publicAgentInfo{}
	pubTokenExp     = map[string]time.Time{}
	pubTokenMu      sync.RWMutex
)

func lookupPublicToken(db *sql.DB, token string) *publicAgentInfo {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	pubTokenMu.RLock()
	if pa, ok := pubTokenCache[token]; ok && time.Now().Before(pubTokenExp[token]) {
		pubTokenMu.RUnlock()
		return pa
	}
	pubTokenMu.RUnlock()

	var id, name, desc, starters string
	err := db.QueryRow(
		`SELECT id, name, description, starter_prompts FROM agents WHERE public_token=? AND is_public=1 LIMIT 1`,
		token,
	).Scan(&id, &name, &desc, &starters)
	if err != nil {
		return nil
	}
	pa := &publicAgentInfo{ID: id, Name: name, Description: desc, StarterPrompts: starters}
	pubTokenMu.Lock()
	pubTokenCache[token] = pa
	pubTokenExp[token] = time.Now().Add(60 * time.Second)
	pubTokenMu.Unlock()
	return pa
}

func publicTokenFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-Public-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("public_token")
}

// ── Public API handlers ───────────────────────────────────────────────────────

func publicGetAgentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := chi.URLParam(r, "agentID")
		var id, name, desc, starters string
		var isPublic int
		err := db.QueryRowContext(r.Context(),
			`SELECT id, name, description, starter_prompts, is_public FROM agents WHERE id=?`, agentID,
		).Scan(&id, &name, &desc, &starters, &isPublic)
		if err == sql.ErrNoRows || isPublic == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":              id,
			"name":            name,
			"description":     desc,
			"starter_prompts": json.RawMessage(starters),
		})
	}
}

func publicCreateSessionHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pa := lookupPublicToken(db, publicTokenFromRequest(r))
		if pa == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid public token"})
			return
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO chat_sessions(id,title,agent_id,status,created_at,updated_at)
			VALUES(?,?,?,?,?,?)
		`, id, "VITA Tax Chat", pa.ID, "active", now, now); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "agent_id": pa.ID, "status": "active"})
	}
}

func publicGetSessionHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pa := lookupPublicToken(db, publicTokenFromRequest(r))
		if pa == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid public token"})
			return
		}
		sessionID := chi.URLParam(r, "sessionID")
		var id, title, agentID, status string
		err := db.QueryRowContext(r.Context(),
			`SELECT id, title, agent_id, status FROM chat_sessions WHERE id=? AND agent_id=?`,
			sessionID, pa.ID,
		).Scan(&id, &title, &agentID, &status)
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "title": title, "agent_id": agentID, "status": status})
	}
}

func publicListMessagesHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pa := lookupPublicToken(db, publicTokenFromRequest(r))
		if pa == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid public token"})
			return
		}
		sessionID := chi.URLParam(r, "sessionID")
		var agentID string
		if err := db.QueryRowContext(r.Context(), `SELECT agent_id FROM chat_sessions WHERE id=?`, sessionID).Scan(&agentID); err != nil || agentID != pa.ID {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, role, content, created_at FROM chat_messages WHERE session_id=? ORDER BY created_at ASC
		`, sessionID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, role, content, createdAt string
			if err := rows.Scan(&id, &role, &content, &createdAt); err != nil {
				continue
			}
			out = append(out, map[string]any{"id": id, "role": role, "content": content, "created_at": createdAt})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func publicSendMessageHandler(db *sql.DB, workspaceRoot string, _ interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pa := lookupPublicToken(db, publicTokenFromRequest(r))
		if pa == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid public token"})
			return
		}
		sessionID := chi.URLParam(r, "sessionID")
		var agentID string
		if err := db.QueryRowContext(r.Context(), `SELECT agent_id FROM chat_sessions WHERE id=?`, sessionID).Scan(&agentID); err != nil || agentID != pa.ID {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content required"})
			return
		}
		runID, _, err := createRunForSessionMessage(r.Context(), db, workspaceRoot, sessionID, req.Content, "queued")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"run_id": runID})
	}
}

func publicStreamRunHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pa := lookupPublicToken(db, publicTokenFromRequest(r))
		if pa == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid public token"})
			return
		}
		runID := chi.URLParam(r, "runID")
		// Verify this run belongs to the public agent's sessions.
		var agentID string
		err := db.QueryRowContext(r.Context(), `
			SELECT cs.agent_id FROM runs r
			JOIN session_runs sr ON sr.run_id = r.id
			JOIN chat_sessions cs ON cs.id = sr.session_id
			WHERE r.id = ? LIMIT 1
		`, runID).Scan(&agentID)
		if err != nil || agentID != pa.ID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		streamEventsForRun(w, r, db, runID)
	}
}

func streamEventsForRun(w http.ResponseWriter, r *http.Request, db *sql.DB, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	lastSeq := 0
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	send := func(ctx context.Context) error {
		rows, err := db.QueryContext(ctx,
			`SELECT id,step_id,event_type,seq,payload_json,created_at FROM events WHERE run_id=? AND seq>? ORDER BY seq ASC LIMIT 200`,
			runID, lastSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, stepID, typ, payload, createdAt string
			var seq int
			if err := rows.Scan(&id, &stepID, &typ, &seq, &payload, &createdAt); err != nil {
				return err
			}
			lastSeq = seq
			evt := map[string]any{"id": id, "step_id": stepID, "event_type": typ, "seq": seq, "payload": json.RawMessage(payload), "created_at": createdAt}
			b, _ := json.Marshal(evt)
			if _, err := fmt.Fprintf(w, "event: run_event\ndata: %s\n\n", string(b)); err != nil {
				return err
			}
			flusher.Flush()
		}
		return nil
	}

	if err := send(r.Context()); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := send(r.Context()); err != nil {
				return
			}
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ── Public chat page ──────────────────────────────────────────────────────────

func publicChatPageHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := chi.URLParam(r, "agentID")
		var name, desc, starters, token string
		var isPublic int
		err := db.QueryRowContext(r.Context(),
			`SELECT name, description, starter_prompts, public_token, is_public FROM agents WHERE id=?`, agentID,
		).Scan(&name, &desc, &starters, &token, &isPublic)
		if err == sql.ErrNoRows || isPublic == 0 {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if starters == "" || starters == "null" {
			starters = "[]"
		}
		page := buildPublicChatPage(agentID, name, desc, starters, token)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		_, _ = w.Write([]byte(page))
	}
}

func buildPublicChatPage(agentID, name, desc, startersJSON, token string) string {
	startersJSON = strings.ReplaceAll(startersJSON, "</", "<\\/")
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>` + htmlEscape(name) + `</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         background: #f0f4f0; display: flex; flex-direction: column; height: 100dvh; }
  header { background: #1a472a; color: #fff; padding: 14px 20px; }
  header h1 { font-size: 1.1rem; font-weight: 600; }
  header p  { font-size: 0.8rem; opacity: 0.8; margin-top: 2px; }
  #messages { flex: 1; overflow-y: auto; padding: 16px; display: flex; flex-direction: column; gap: 10px; }
  .bubble { max-width: 80%; padding: 10px 14px; border-radius: 16px; font-size: 0.9rem; line-height: 1.5; white-space: pre-wrap; word-break: break-word; }
  .bubble.user { background: #1a472a; color: #fff; align-self: flex-end; border-bottom-right-radius: 4px; }
  .bubble.assistant { background: #fff; color: #111; align-self: flex-start; border: 1px solid #dde; border-bottom-left-radius: 4px; }
  .bubble.thinking { background: #f8f8f8; color: #888; align-self: flex-start; font-style: italic; font-size: 0.82rem; border: 1px dashed #ccc; }
  #starters { padding: 0 16px 12px; display: flex; flex-wrap: wrap; gap: 8px; }
  .starter-btn { background: #fff; border: 1.5px solid #1a472a; color: #1a472a; border-radius: 20px;
                 padding: 6px 14px; font-size: 0.82rem; cursor: pointer; transition: background .15s; }
  .starter-btn:hover { background: #1a472a; color: #fff; }
  #input-bar { display: flex; gap: 8px; padding: 12px 16px; background: #fff; border-top: 1px solid #dde; }
  #msg-input { flex: 1; border: 1.5px solid #ccc; border-radius: 22px; padding: 10px 16px;
               font-size: 0.9rem; outline: none; resize: none; height: 44px; max-height: 120px; overflow-y: auto; }
  #msg-input:focus { border-color: #1a472a; }
  #send-btn { background: #1a472a; color: #fff; border: none; border-radius: 22px;
              padding: 0 20px; font-size: 0.9rem; cursor: pointer; white-space: nowrap; }
  #send-btn:disabled { opacity: 0.5; cursor: default; }
  .courtesy { font-size: 0.78rem; color: #555; padding: 8px 20px 2px; text-align: center; line-height: 1.4; }
  .courtesy a { color: #1a472a; text-decoration: none; }
  .courtesy a:hover { text-decoration: underline; }
  .disclaimer { font-size: 0.72rem; color: #888; padding: 2px 20px 8px; text-align: center; line-height: 1.4; }
</style>
</head>
<body>
<header>
  <h1 id="agent-name">` + htmlEscape(name) + `</h1>
  <p id="agent-desc">` + htmlEscape(desc) + `</p>
</header>
<div id="messages"></div>
<div id="starters"></div>
<div id="input-bar">
  <textarea id="msg-input" placeholder="Type your question…" rows="1"></textarea>
  <button id="send-btn">Send</button>
</div>
<p class="courtesy">A free service provided by <strong>Minnesota EquiVoice Partnership</strong> · <a href="https://www.mnequivoicepartnership.org" target="_blank" rel="noopener">mnequivoicepartnership.org</a></p>
<p class="disclaimer">This is a free tax estimation tool. Results are estimates only — not tax advice. Always verify with a certified VITA volunteer.</p>

<script>
const AGENT_ID = ` + "`" + agentID + "`" + `;
const TOKEN    = ` + "`" + token + "`" + `;
const STARTERS = ` + startersJSON + `;

const $ = id => document.getElementById(id);
let sessionId = sessionStorage.getItem('pub_session_' + AGENT_ID);
let busy = false;

function addBubble(role, text) {
  const d = document.createElement('div');
  d.className = 'bubble ' + role;
  d.textContent = text;
  $('messages').appendChild(d);
  $('messages').scrollTop = $('messages').scrollHeight;
  return d;
}

function headers() {
  return { 'Content-Type': 'application/json', 'X-Public-Token': TOKEN };
}

async function ensureSession() {
  if (sessionId) return sessionId;
  const r = await fetch('/api/public/chat/sessions', {
    method: 'POST', headers: headers(), body: JSON.stringify({})
  });
  const d = await r.json();
  sessionId = d.id;
  sessionStorage.setItem('pub_session_' + AGENT_ID, sessionId);
  return sessionId;
}

async function sendMessage(text) {
  if (busy || !text.trim()) return;
  busy = true;
  $('send-btn').disabled = true;
  $('starters').style.display = 'none';

  addBubble('user', text.trim());
  const thinking = addBubble('thinking', 'Thinking…');

  const sid = await ensureSession();
  const r = await fetch('/api/public/chat/sessions/' + sid + '/messages', {
    method: 'POST', headers: headers(), body: JSON.stringify({ content: text.trim() })
  });
  const d = await r.json();
  const runId = d.run_id;

  let reply = '';
  const src = new EventSource('/api/public/stream/runs/' + runId + '?public_token=' + encodeURIComponent(TOKEN));
  src.addEventListener('run_event', e => {
    try {
      const ev = JSON.parse(e.data);
      const p = ev.payload || {};
      if (ev.event_type === 'text_delta' && p.delta) {
        if (thinking.parentNode) thinking.remove();
        if (!reply) addBubble('assistant', '');
        const last = $('messages').querySelector('.bubble.assistant:last-child');
        reply += p.delta;
        if (last) { last.textContent = reply; $('messages').scrollTop = $('messages').scrollHeight; }
      }
      if (ev.event_type === 'run_finished' || ev.event_type === 'run_failed') {
        src.close();
        if (thinking.parentNode) thinking.remove();
        if (!reply) addBubble('assistant', p.error || 'Something went wrong. Please try again.');
        busy = false;
        $('send-btn').disabled = false;
      }
    } catch {}
  });
  src.onerror = () => {
    src.close();
    if (thinking.parentNode) thinking.remove();
    if (!reply) addBubble('assistant', 'Connection error. Please try again.');
    busy = false;
    $('send-btn').disabled = false;
  };
}

// Render starter prompts
if (Array.isArray(STARTERS) && STARTERS.length) {
  STARTERS.forEach(s => {
    const label = typeof s === 'string' ? s : (s.label || s.text || s.prompt || '');
    if (!label) return;
    const btn = document.createElement('button');
    btn.className = 'starter-btn';
    btn.textContent = label;
    btn.onclick = () => { $('msg-input').value = label; sendMessage(label); $('msg-input').value = ''; };
    $('starters').appendChild(btn);
  });
}

$('send-btn').onclick = () => { const t = $('msg-input').value; $('msg-input').value = ''; sendMessage(t); };
$('msg-input').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); const t = $('msg-input').value; $('msg-input').value = ''; sendMessage(t); }
});
</script>
</body>
</html>`
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	return s
}
