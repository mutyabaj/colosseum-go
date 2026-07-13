# MN EquiVoice Partnership — Deployment Reference

**Organization:** Minnesota EquiVoice Partnership (MN-EVP), 501(c)(3)  
**Primary domain:** mnequivoicepartnership.org  
**Colosseum instance:** colosseum.mnequivoicepartnership.org  
**Last updated:** 2026-07-13

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Infrastructure](#2-infrastructure)
3. [Docker Containers — Colosseum VM](#3-docker-containers--colosseum-vm)
4. [Colosseum Application](#4-colosseum-application)
5. [Colosseum Agents](#5-colosseum-agents)
6. [Environment Variables](#6-environment-variables)
7. [Phone System](#7-phone-system)
8. [VITA Voice AI Pipeline](#8-vita-voice-ai-pipeline)
9. [A2P 10DLC SMS Compliance](#9-a2p-10dlc-sms-compliance)
10. [WordPress Compliance Pages](#10-wordpress-compliance-pages)
11. [Database Tables — Voice & SMS](#11-database-tables--voice--sms)
12. [FreePBX — Volunteer Phone System](#12-freepbx--volunteer-phone-system)
13. [Adding a New Extension](#13-adding-a-new-extension)
14. [DNS](#14-dns)
15. [CI/CD — Coolify](#15-cicd--coolify)
16. [Persistent Storage](#16-persistent-storage)
17. [Pending Items](#17-pending-items)

---

## 1. Architecture Overview

```
                        ┌─────────────────────────────────────────┐
                        │          Azure VM — Colosseum           │
                        │            128.203.195.79               │
                        │                                         │
  HTTPS ───────────────▶│  Traefik (reverse proxy, TLS)           │
                        │    ├── colosseum.mnequivoicepartnership  │
                        │    └── coolify.mnequivoicepartnership    │
                        │                                         │
  Twilio webhook ───────▶│  Colosseum (Go, port 8080)             │
  (voice + SMS)         │    ├── /voice/inbound  (VITA IVR)       │
                        │    ├── /sms/inbound    (SMS chatbot)    │
                        │    └── /whatsapp/inbound                │
                        │                                         │
                        │  Zernio Bridge (port 3001)              │
                        │  Tax API       (port 8001)              │
                        └─────────────────────────────────────────┘

                        ┌─────────────────────────────────────────┐
                        │          Azure VM — FreePBX             │
                        │             20.9.49.33                  │
                        │                                         │
  Twilio SIP trunk ────▶│  FreePBX / Asterisk                     │
                        │    port 5060 UDP/TCP (trunk + legacy)   │
                        │    port 5061 TCP/TLS (softphones)       │
                        │    ├── Extension 101  (spare)           │
                        │    └── Extension 102  (Fred Kigundu)    │
                        │                                         │
  Linphone (TLS+SRTP)  ◀│  voice.mnequivoicepartnership.org       │
                        └─────────────────────────────────────────┘

  Twilio Numbers
    +16513027258  ──── Webhook ──────▶ Colosseum VITA IVR
    +16515151968  ──── SIP Trunk ────▶ FreePBX → Extension 102
```

---

## 2. Infrastructure

### Colosseum VM

| Property | Value |
|----------|-------|
| Provider | Azure (Standard_B2ms) |
| Public IP | 128.203.195.79 |
| OS | Ubuntu 22.04.5 LTS |
| vCPU / RAM | 2 vCPU / 8 GB |
| SSH | `ssh -p 2222 azureuser@128.203.195.79` |
| SSH key | `~/.ssh/id_rsa` |
| NSG | mnequivoice-ubuntu-01NSG (mnequivoice-rg) |

**NSG inbound rules:**

| Name | Port | Protocol | Source |
|------|------|----------|--------|
| open-port-80 | 80 | Any | Any |
| open-port-443 | 443 | Any | Any |
| open-port-2222 | 2222 | Any | Any |
| default-allow-ssh | 22 | TCP | Any |
| Allow-Coolify-8000 | 8000 | TCP | 73.242.124.13 |
| Allow-Colosseum-8080 | 8080 | TCP | 73.242.124.13 |

### FreePBX VM

| Property | Value |
|----------|-------|
| Provider | Azure |
| Public IP | 20.9.49.33 |
| OS | Ubuntu (FreePBX distro) |
| SSH | `ssh -i ~/.ssh/id_rsa pbxadmin@20.9.49.33` |
| Web admin | https://voice.mnequivoicepartnership.org/admin |
| NSG | mnequivoice-pbx-01NSG (MNEQUIVOICE-RG) |

**NSG inbound rules:**

| Name | Port | Protocol |
|------|------|----------|
| default-allow-ssh | 22 | TCP |
| FreePBX-HTTP | 80 | TCP |
| FreePBX-HTTPS | 443 | TCP |
| FreePBX-SIP-UDP | 5060 | UDP |
| FreePBX-SIP-TCP | 5060 | TCP |
| FreePBX-SIP-TLS | 5061 | TCP |
| FreePBX-RTP | 10000–20000 | UDP |
| FreePBX-IAX2 | 4569 | UDP |
| FreePBX-UCP-Node | 8003 | TCP |

---

## 3. Docker Containers — Colosseum VM

All containers are managed by Coolify. The Colosseum container name changes on each redeploy (Coolify naming pattern: `u1v1m04mj22sqmnhkdrm1kqq-<build_id>`).

| Container | Image | Internal Port | Purpose |
|-----------|-------|---------------|---------|
| colosseum | colosseum-go build | 8080 | Main application |
| zernio-bridge | zernio-bridge | 3001 | Social media bridge |
| tax-api | tax-api | 8001 | Tax calculation API |
| coolify-proxy | traefik:v3.6 | 80, 443 | Reverse proxy / TLS |
| coolify | coollabsio/coolify:4.0.0 | 8000 | CI/CD dashboard |
| coolify-db | postgres:15-alpine | 5432 | Coolify database |
| coolify-redis | redis:7-alpine | 6379 | Coolify cache |
| coolify-realtime | coolify-realtime:1.0.13 | 6001–6002 | Realtime events |
| coolify-sentinel | sentinel:0.0.21 | — | Monitoring |

**Useful commands:**

```bash
# Find current Colosseum container name
sudo docker ps --format '{{.Names}}' | grep u1v1m

# Exec into Colosseum container
sudo docker exec <container_name> sqlite3 /data/colosseum.db

# View Colosseum logs
sudo docker logs <container_name> --tail 100 -f
```

---

## 4. Colosseum Application

**Repository:** github.com/mutyabaj/colosseum-go  
**Branch:** master  
**Build:** Go binary with embedded UI assets (`make ui-build && make build`)  
**Database:** SQLite at `/data/colosseum.db` (on Docker named volume)

### Key API routes

| Method | Path | Purpose |
|--------|------|---------|
| POST | /voice/inbound | VITA IVR entry point (Twilio webhook) |
| POST | /voice/play | Replay IVR message |
| POST | /voice/menu | IVR digit handler (1=replay, 2=voicemail, 3=transfer, 4=SMS consent) |
| POST | /voice/after-hours-menu | After-hours digit handler (1=voicemail, 2=SMS consent) |
| POST | /voice/transfer | Direct SIP dial to volunteer queue (ext 650) |
| POST | /voice/sms-consent | Plays A2P consent disclosure, gathers DTMF/speech response |
| POST | /voice/sms-consent-response | Records consent decision, sends SMS only if consented |
| POST | /voice/voicemail | Voicemail prompt |
| POST | /voice/voicemail-done | Voicemail recording complete, notifies admin |
| POST | /voice/queue-fallback | Plays voicemail fallback when volunteer queue is unavailable |
| POST | /voice/stream | Returns TwiML `<Connect><Stream>` for AI voice session |
| GET | /voice/stream/ws | WebSocket endpoint — Twilio Media Streams audio bridge |
| GET | /voice/vita/toggle | Staff toggle: `?state=open\|close\|ai-on\|ai-off\|status` |
| POST | /sms/inbound | SMS chatbot (Twilio webhook) |
| POST | /whatsapp/inbound | WhatsApp chatbot (Twilio webhook) |
| GET | /api/runs/:id/artifacts/:aid/content | Download agent-generated file |

### Auth bypass paths

The following paths bypass basic auth and API token checks (required for Twilio webhooks):

```go
/c/*            // Public agent chat pages
/api/public/*   // Public API
/voice/*        // VITA IVR webhooks
/sms/inbound    // SMS chatbot webhook
/whatsapp/inbound
```

### VITA IVR (`internal/api/voice_routes.go`)

When a call arrives at +16513027258:

**During VITA hours (Sat–Sun 9 AM–5 PM CT) with AI enabled:**
1. Twilio POSTs to `/voice/inbound`
2. Colosseum returns TwiML `<Connect><Stream>` — call audio streams to the AI voice pipeline
3. AI answers, handles questions, and can transfer to volunteer queue or voicemail
4. If AI fails, call falls back to `/voice/play` (standard IVR)

**During VITA hours with AI disabled (or outside VITA hours):**
1. Twilio POSTs to `/voice/inbound`
2. Colosseum checks schedule and force-close flag
3. During hours: IVR plays message (Amazon Polly Joanna voice), offers menu
   - Press 1 → replay; Press 2 → voicemail; Press 3 → volunteer transfer; Press 4 → SMS consent flow
4. After hours / holiday: abbreviated menu — Press 1 → voicemail; Press 2 → SMS consent flow
5. Voicemail completion POSTs to `/voice/voicemail-done`, sends SMS notification to `VITA_VOICEMAIL_NOTIFY_NUMBER`

**SMS is never sent automatically.** Callers must explicitly request a text and consent via the IVR disclosure (see Section 9).

**Booking URL default:** `https://outlook.office.com/book/MNEVPVITATCE@mnequivoicepartnership.org/`  
Override with `VITA_BOOKING_URL` env var.

**Queue force-close:** Use the toggle endpoint to close the queue without a redeploy. The close auto-expires at midnight CT.

```
GET /voice/vita/toggle?token=TOKEN&state=close    # force close until midnight CT
GET /voice/vita/toggle?token=TOKEN&state=open     # reopen immediately
GET /voice/vita/toggle?token=TOKEN&state=ai-on    # enable AI voice (persisted to DB)
GET /voice/vita/toggle?token=TOKEN&state=ai-off   # disable AI voice (persisted to DB)
GET /voice/vita/toggle?token=TOKEN&state=status   # show current state
```

---

## 5. Colosseum Agents

| Agent | ID | Model | Purpose |
|-------|----|-------|---------|
| Equivoice-social-media-agent | dadbb108-e083-42de-8897-1b629e4be095 | — | Social media post generation |
| Equivoice-scheduler | 4c5bc5d8-d257-4c14-b485-d02a3d353389 | — | Scheduled post publishing |
| EquiVoice-grant-writer | 393a912f-... | — | Grant drafts — reads persistent docs from `/data/workspaces/equivoice-docs/` + session uploads |
| Grant-writer-client-orgs | d6e2228a-... | — | Grant drafts for other nonprofits — reads org identity entirely from session uploads |
| EquiVoice-grant-researcher | f50c5c59-acd2-43d4-a1f1-80a2405999fc | claude-opus-4-7 | Foundation research — MN-specific guidelines when EquiVoice context detected |
| grant-web-fetcher | 27a8d1b9-6511-43d3-9b4b-5ae2e7b9f76b | claude-haiku-4-5-20251001 | Web content retrieval for grant research |

### Persistent EquiVoice docs

EquiVoice org documents are stored at `/data/workspaces/equivoice-docs/` on the `/data` Docker volume. These persist across container redeploys. The grant writer agent reads from this path automatically.

To restore after a rebuild:
```bash
sudo /opt/equivoice-scripts/restore-docs.sh
```

### Agent artifacts

Files published by agents (e.g. grant drafts) are stored in the `artifacts` table and served at:
```
GET /api/runs/{runID}/artifacts/{artifactID}/content
```
Accessible from the Artifacts tab in the Colosseum UI and as inline links in the chat transcript.

---

## 6. Environment Variables

Set in Coolify → Colosseum application → Environment Variables.

### Twilio / SMS

| Variable | Description |
|----------|-------------|
| `TWILIO_ACCOUNT_SID` | Twilio account SID |
| `TWILIO_AUTH_TOKEN` | Twilio auth token |
| `TWILIO_FROM_NUMBER` | `+16513027258` — VITA line, used as SMS sender |
| `TWILIO_SMS_AGENT_ID` | Agent ID that handles inbound SMS conversations |
| `TWILIO_SMS_WEBHOOK_URL` | Webhook URL for SMS |
| `TWILIO_WHATSAPP_NUMBER` | WhatsApp-enabled number |
| `TWILIO_WHATSAPP_AGENT_ID` | Agent ID that handles WhatsApp conversations |
| `TWILIO_WHATSAPP_WEBHOOK_URL` | Webhook URL for WhatsApp |
| `TWILIO_SKIP_SIG_VALIDATION` | Set to `true` during development only — **remove after A2P campaign approved** |

### VITA IVR

| Variable | Description |
|----------|-------------|
| `VITA_BOOKING_URL` | Override default Microsoft Bookings link (optional) |
| `VITA_VOICEMAIL_NOTIFY_NUMBER` | Mobile number to receive SMS when a voicemail is left via VITA IVR (e.g. `+16512956509`) |
| `VITA_TOGGLE_TOKEN` | Secret token required to call `/voice/vita/toggle` — keep private |

### VITA Voice AI Pipeline

| Variable | Description |
|----------|-------------|
| `DEEPGRAM_API_KEY` | Deepgram API key — enables STT (nova-2) and TTS (Aura/Joanna) for the AI voice pipeline |

The AI voice pipeline is only active when `DEEPGRAM_API_KEY` is set **and** AI mode has been enabled via the toggle endpoint (`state=ai-on`). AI mode is persisted to the `system_settings` table so it survives container restarts.

### AI Providers

| Variable | Description |
|----------|-------------|
| `ANTHROPIC_API_KEY` | Claude models — required for both the AI voice pipeline (Haiku) and agent runs |
| `OPENAI_API_KEY` | OpenAI models |
| `DEEPSEEK_API_KEY` | DeepSeek models |

---

## 7. Phone System

### Twilio Account

**Account name:** My first Twilio account  
**Console:** console.twilio.com

### Phone Numbers

| Number | Friendly Name | Voice Handler | Purpose |
|--------|--------------|---------------|---------|
| +16513027258 | (651) 302-7258 | Webhook → `https://colosseum.mnequivoicepartnership.org/voice/inbound` | VITA IVR + SMS chatbot |
| +16515151968 | (651) 515-1968 | SIP Trunk → MNEquivoiceSIP | Volunteer softphone (Fred Kigundu) |

### SIP Trunk — MNEquivoiceSIP

| Property | Value |
|----------|-------|
| Trunk SID | TK7b086375e8f4c01bb4e1f7e6530da9ea |
| Termination URI | `mnequivoice.pstn.twilio.com` |
| Origination URI | `sip:voice.mnequivoicepartnership.org:5060` |
| Credential list | Equivoice (`pbxadmin` username) |

**Termination** = outbound calls from FreePBX → Twilio → PSTN  
**Origination** = inbound calls from PSTN → Twilio → FreePBX

### Twilio Phone Numbers

| Number | Friendly Name | Voice Handler | Purpose |
|--------|--------------|---------------|---------|
| +16513027258 | (651) 302-7258 | Webhook → `https://colosseum.mnequivoicepartnership.org/voice/inbound` | VITA IVR + SMS chatbot |
| +16515151968 | (651) 515-1968 | SIP Trunk → MNEquivoiceSIP | Volunteer softphone (Fred Kigundu) |

---

## 8. VITA Voice AI Pipeline

The VITA Voice AI Pipeline routes inbound calls through a real-time speech AI stack when enabled. Implementation is in `internal/voice/`.

### Architecture

```
Twilio (caller audio)
  │
  ▼
/voice/inbound  ──── TwiML <Connect><Stream> ────▶  /voice/stream/ws (WebSocket)
                                                           │
                                              ┌────────────┼────────────┐
                                              ▼            ▼            ▼
                                         Deepgram      Claude        Deepgram
                                         STT           Haiku         Aura TTS
                                         nova-2        (LLM)         (mulaw 8kHz)
                                         streaming     max 150 tok
                                              │
                                              └──── audio chunks back to caller via WebSocket
```

### Components

| File | Role |
|------|------|
| `internal/voice/session.go` | Per-call orchestrator — WebSocket event loop, greeting, barge-in |
| `internal/voice/conversation.go` | Claude Haiku integration — streaming LLM, phrase extraction, intent detection |
| `internal/voice/deepgram.go` | Deepgram STT — streaming WebSocket, endpointing 300 ms |
| `internal/voice/tts.go` | Deepgram Aura TTS — REST API, mulaw 8 kHz output |
| `internal/voice/mulaw.go` | PCM ↔ mulaw codec |
| `internal/api/voice_stream.go` | HTTP handlers — TwiML generator + WebSocket upgrader |

### AI Capabilities

The AI assistant (Claude Haiku) can:
1. Answer questions about VITA eligibility, locations, hours, and documents
2. Transfer the caller to the volunteer queue (`/voice/transfer` → SIP ext 650)
3. Redirect to voicemail (`/voice/voicemail`)
4. Hand off to the SMS consent IVR (`/voice/sms-consent`) when caller asks for an appointment link by text

### Toggle (Kill Switch)

AI mode is toggled via `GET /voice/vita/toggle?token=TOKEN&state=ai-on|ai-off`. The state is persisted in the `system_settings` SQLite table so it survives container restarts. Use `state=status` to check the current state without changing it.

---

## 9. A2P 10DLC SMS Compliance

### Campaign Details

**Status:** Resubmission in progress (July 2026) — previous submissions rejected (error 30909/30917).

| Field | Value |
|-------|-------|
| Campaign SID | `CM70cd3063a4406b93308762ce3177ca5b` |
| Brand | Minnesota EquiVoice Partnership (`BN54a91bdb1ddd80348c84fc92ef35b758`) |
| Messaging service | `MG91900402c4f9f2500f8832adf07d8452` |

### Approved Opt-In Methods

Only **Via Text** and **Verbal** are selected. Website/Form is not selected.

| Method | Description |
|--------|-------------|
| Via Text | Caller sees SMS disclosure on VITA page and voluntarily texts (651) 302-7258 — first text constitutes consent to automated replies about their inquiry |
| Verbal / IVR | During a call, caller selects the text-link option → IVR reads full A2P disclosure → caller presses 1 or says "yes" → one automated SMS is sent |

### Campaign URLs

| Field | URL |
|-------|-----|
| Privacy Policy | `https://mnequivoicepartnership.org/privacy-policy/` |
| Terms of Service | `https://mnequivoicepartnership.org/terms/` |
| IVR consent evidence | `https://mnequivoicepartnership.org/vita-sms-consent-disclosure/` |

### IVR Consent Script (verbatim)

> "Would you like Minnesota EquiVoice Partnership VITA to send one automated text message containing the appointment scheduling link to the number you are calling from? Message and data rates may apply. Reply STOP to opt out or HELP for help. Press 1 or say yes to agree. Press 2 or say no to continue without a text."

This exact wording is in both `voice_routes.go` (`vitaVoiceSMSConsentHandler`) and the public disclosure page. "One automated text message" is included inline so frequency is stated at the point of consent.

### SMS Keyword Responses

| Keyword | Response sent |
|---------|---------------|
| STOP / CANCEL / END / QUIT / UNSUBSCRIBE | Minnesota EquiVoice Partnership VITA: You have been unsubscribed and will receive no further messages. Reply START to resubscribe or call (651) 302-7258 for VITA services. |
| START / YES / UNSTOP | Minnesota EquiVoice Partnership VITA: You have been re-subscribed to automated VITA tax-assistance responses. Message frequency varies. Msg & data rates may apply. Reply STOP to opt out or HELP for help. |
| HELP | Minnesota EquiVoice Partnership VITA: Help is available at mnequivoicepartnership.org or call (651) 302-7258. Msg & data rates may apply. Reply STOP to opt out. |

### Consent Audit Log

Every IVR consent interaction is recorded in the `sms_consent_log` table with: call SID, caller phone number, consent decision, method (dtmf/speech), raw response, and timestamp. This provides per-call evidence for A2P compliance audits.

After A2P approval: remove `TWILIO_SKIP_SIG_VALIDATION` from Coolify environment variables.

---

## 10. WordPress Compliance Pages

Three HTML files in the repo root are pasted into WordPress as Custom HTML blocks:

| File | WordPress slug | Purpose |
|------|---------------|---------|
| `privacy-policy.html` | `/privacy-policy/` | Full privacy policy including SMS section |
| `terms.html` | `/terms/` | SMS Terms of Service (15 sections) |
| `ivr-consent-script.html` | `/vita-sms-consent-disclosure/` | Verbatim IVR consent script — A2P evidence URL |
| `vita-page.html` | `/volunteer-tax-assisstance-and-tax-assistance-for-the-elderly-vita-tce/` | VITA services page with SMS CTA disclosure |

All pages use brand colors `#0B1545` (navy) and `#B31942` (red). After any edit to these files, paste the updated HTML into the corresponding WordPress page and publish.

---

## 11. Database Tables — Voice & SMS

Two migrations were added to support the voice AI and A2P compliance features:

### `system_settings` (migration 014)

General key-value store for persistent runtime configuration.

| Column | Type | Description |
|--------|------|-------------|
| `key` | TEXT PRIMARY KEY | Setting name (e.g. `vita_ai_enabled`) |
| `value` | TEXT | Setting value |
| `updated_at` | TEXT | ISO 8601 timestamp of last change |

Current keys:

| Key | Values | Purpose |
|-----|--------|---------|
| `vita_ai_enabled` | `0` / `1` | Whether the VITA Voice AI pipeline is active |

### `sms_consent_log` (migration 015)

Per-call audit log for A2P SMS consent.

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER PK | Auto-increment |
| `call_sid` | TEXT | Twilio Call SID |
| `phone_number` | TEXT | Caller's phone number |
| `consented` | INTEGER | `1` = agreed, `0` = declined or no response |
| `consent_method` | TEXT | `dtmf` or `speech` |
| `consent_response` | TEXT | Raw digit pressed or speech result |
| `consented_at` | TEXT | ISO 8601 UTC timestamp |

Indexed on `call_sid` and `phone_number`.

---

## 12. FreePBX — Volunteer Phone System

**Admin UI:** https://voice.mnequivoicepartnership.org/admin  
**SSH:** `ssh -i ~/.ssh/id_rsa pbxadmin@20.9.49.33`  
**Asterisk logs:** `/var/log/asterisk/full`

### Extensions

| Extension | Name | Softphone | Status |
|-----------|------|-----------|--------|
| 101 | (spare) | — | Unavailable |
| 102 | Fred Kigundu | Linphone (mobile) | Active |

### Linphone Configuration (Extension 102)

| Field | Value |
|-------|-------|
| SIP Server | `voice.mnequivoicepartnership.org` |
| Username | `102` |
| Password | Secret from FreePBX extension 102 |
| Transport | **TLS** |
| Port | **5061** |
| Media encryption | None (deferred — Linphone SRTP is global; would break secondary Ubiquiti account) |
| Outbound CID | `+16515151968` |

> **Note:** Linphone on mobile drops SIP registration when backgrounded. Enable "Keep alive" and disable battery optimization for reliable inbound calls. Desktop Zoiper is more reliable for always-on use.

### FreePBX TLS / HTTPS

| Component | Configuration |
|-----------|--------------|
| Web HTTPS | Apache with Let's Encrypt cert (`voice.mnequivoicepartnership.org`) |
| Cert location | `/etc/letsencrypt/live/voice.mnequivoicepartnership.org/` |
| Cert copy (Asterisk) | `/etc/asterisk/keys/voice.mnequivoicepartnership.org.{crt,key}` |
| TLS protocol | TLSv1.2 + TLSv1.3 only (TLSv1/1.1 disabled in `sangoma-ssl.conf`) |
| SIP TLS transport | `[0.0.0.0-tls]` in `/etc/asterisk/pjsip.transports_custom.conf` |
| SIP TLS method | `tlsv1_2` (minimum TLS 1.2) |
| HTTP → HTTPS redirect | Configured in Apache `sangoma.conf` via RewriteRule |
| Cert renewal | Let's Encrypt auto-renews; after renewal copy to `/etc/asterisk/keys/` and run `fwconsole certificates --import` |

### Intrusion Detection (Fail2ban)

Active with 10 jails: `asterisk-iptables`, `pbx-gui`, `sshd`, `recidive`, and others. No additional configuration needed.

To check status:
```bash
sudo fail2ban-client status
sudo fail2ban-client status asterisk-iptables
sudo fail2ban-client status recidive
```

### SMTP (Voicemail-to-Email)

Email is relayed via **Resend** (smtp.resend.com:587). Voicemail emails arrive from `pbx@mnequivoicepartnership.org`.

| File | Purpose |
|------|---------|
| `/etc/postfix/sasl_passwd` | Resend API key credentials |
| `/etc/postfix/sender_canonical` | Maps `root` and `asterisk` → `pbx@mnequivoicepartnership.org` |

### Voicemail — Extensions

| Ext | Name | Email | Attachment |
|-----|------|-------|------------|
| 101 | John Mutyaba | john.mutyaba@mnequivoicepartnership.org | Yes |
| 102 | Fred Kigundu | fred.kigundu@mnequivoicepartnership.org | Yes |

### Notifications

Admin → System Admin → Notifications: all alerts go to `info@mnequivoicepartnership.org` (aliased to John).

### Inbound Routes

| DID | Destination |
|-----|-------------|
| `+16515151968` | Extension 102 (Fred Kigundu) |

### Outbound Routes

| Route | Trunk | Dial Patterns |
|-------|-------|---------------|
| Twilio Outbound | Twilio | `NXXNXXXXXX` (prepend +1), `1NXXNXXXXXX` (prepend +) |

### PJSIP Trunk (Twilio)

| Field | Value |
|-------|-------|
| Username | `mnequivoice` |
| Auth username | `pbxadmin` |
| Secret | Must match Twilio credential list password |
| SIP Server | `mnequivoice.pstn.twilio.com` |
| Registration | None (Twilio doesn't require registration) |
| Transport | UDP |

### Firewall — Trusted Twilio IP Ranges

Added via `fwconsole firewall add trusted`:

```
54.172.60.0/23
54.244.51.0/24
54.171.127.192/26
35.156.191.128/25
35.154.71.128/25
52.215.127.0/24
73.242.124.13/32  (admin IP)
```

---

## 13. Adding a New Extension

Follow these steps each time a new volunteer softphone is added.

### Step 1 — Buy a Twilio number (if the extension needs its own DID)

1. Twilio Console → Phone Numbers → Buy a Number → select a Minnesota (651/612/763/952) number
2. Set Voice handling to **SIP Trunk** → **MNEquivoiceSIP**
3. Do **not** add the number to the trunk's NUMBERS tab — that overrides the inbound route

### Step 2 — Create the extension in FreePBX

**Applications → Extensions → Add Extension → PJSIP**

| Tab | Field | Value |
|-----|-------|-------|
| General | Extension | Next available (e.g. 103) |
| General | Display Name | Volunteer's full name |
| General | Secret | Generate a strong password — this is the SIP password |
| General | Outbound CID | `+1XXXXXXXXXX` (the Twilio DID for this extension) |
| Voicemail | Enabled | Yes |
| Voicemail | Email Address | volunteer@mnequivoicepartnership.org |
| Voicemail | Email Attachment | **Yes** |
| Voicemail | Pager Email | Leave blank (avoids duplicate emails) |
| Advanced | Media Encryption | **SRTP via in-SDP (recommended)** |
| Advanced | Transport | Auto |

Click **Submit** → **Apply Config** (red button, top right).

### Step 3 — Create an inbound route

**Connectivity → Inbound Routes → Add Incoming Route**

| Field | Value |
|-------|-------|
| Description | Volunteer name / number |
| DID Number | `+1XXXXXXXXXX` (full E.164 format — Twilio sends with `+1`) |
| Destination | Extensions → [new extension] |

Click **Submit** → **Apply Config**.

### Step 4 — Configure Linphone on the volunteer's phone

Install Linphone (iOS or Android), then:

**Settings → SIP Accounts → Add account**

| Field | Value |
|-------|-------|
| Username | Extension number (e.g. `103`) |
| SIP Domain | `voice.mnequivoicepartnership.org` |
| Password | Secret from Step 2 |
| Transport | **TLS** |
| Port | **5061** |

**Settings → Audio → Media encryption** → **SRTP**

After saving, Linphone should register (green dot). Test with an inbound call from a cell phone and an outbound call to a cell phone.

**Mobile keep-alive:** Enable keep-alive in Linphone settings and disable battery optimization for the Linphone app, otherwise the OS will kill the registration when the app is backgrounded.

### Step 5 — Test

| Test | Expected result |
|------|-----------------|
| Call the Twilio DID from a cell | Linphone rings |
| Answer and speak | Audio clear in both directions |
| Call from Linphone to an external number | Caller ID shows the Twilio DID |
| Call from Linphone to extension 102 | Extension 102 rings (free, no trunk used) |
| Let call go to voicemail | Volunteer receives email with `.wav` attachment |

### Step 6 — Document

Add the extension to the Extensions table in Section 8 and update the Architecture Overview.

---

## 14. DNS

All records point to Colosseum VM (128.203.195.79) unless noted.

| Hostname | Type | Value | Purpose |
|----------|------|-------|---------|
| mnequivoicepartnership.org | A | — | Main website (WordPress) |
| colosseum.mnequivoicepartnership.org | A | 128.203.195.79 | Colosseum AI platform |
| coolify.mnequivoicepartnership.org | A | 128.203.195.79 | Coolify CI/CD dashboard |
| voice.mnequivoicepartnership.org | A | 20.9.49.33 | FreePBX admin + SIP |
| vita.mnequivoicepartnership.org | A | 128.203.195.79 | **Not yet configured** |

---

## 15. CI/CD — Coolify

**Dashboard:** https://coolify.mnequivoicepartnership.org  
**Direct (IP):** http://128.203.195.79:8000 (restricted to admin IP)  
**GitHub repo:** github.com/mutyabaj/colosseum-go

Push to `master` triggers automatic build and deploy via Coolify. The container is rebuilt from source and restarted with zero-downtime rollover managed by Traefik.

**After each redeploy**, if PDF extraction is needed:
```bash
sudo /opt/equivoice-scripts/restore-docs.sh
```

---

## 16. Persistent Storage

| Path | Volume | Contents |
|------|--------|----------|
| `/data/colosseum.db` | Docker named volume (`/data`) | SQLite database (agents, runs, artifacts, sessions) |
| `/data/workspaces/equivoice-docs/` | Docker named volume (`/data`) | Persistent EquiVoice org documents for grant writer |
| `{workspacePath}/uploads/` | Per-run workspace | Session-scoped file uploads (cleared after run) |

---

## 17. Pending Items

| Item | Priority | Notes |
|------|----------|-------|
| A2P 10DLC resubmission | **Critical** | Resubmitting July 2026 with corrected Message Flow field, "one automated text message" wording in IVR, opt-in methods = Via Text + Verbal only |
| Paste WordPress compliance pages | **High** | `privacy-policy.html`, `terms.html`, `ivr-consent-script.html` → WordPress pages at their slugs; `vita-page.html` already updated |
| Remove `TWILIO_SKIP_SIG_VALIDATION` from Coolify | High | After A2P campaign approved — SMS currently undelivered by carriers |
| Live test of VITA Voice AI | High | Test during VITA hours (Sat–Sun 9 AM–5 PM CT) with `state=ai-on` enabled |
| Renew Asterisk TLS cert after Let's Encrypt renewal | Recurring | Cert expires 2026-08-21; copy to `/etc/asterisk/keys/` and run `fwconsole certificates --import` |
| Ring Groups 601–603 — staff cell forwards | Medium | Configure in FreePBX via Bulk Handler |
| VITA after-hours re-recording | Medium | New voice + library location details |
| Enable SRTP for ext 102 | Low | Deferred — Linphone SRTP is global and would break Ubiquiti desk phone account |
| Configure Linphone keep-alive / disable battery optimization | Low | Prevents registration drops when app is backgrounded |
| Add street addresses to vita-page.html location cards | Low | Currently shows branch names only |
| Add extension 101 assignment | Low | Spare extension, assign when next volunteer onboards |
