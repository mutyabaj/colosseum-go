# MN EquiVoice Partnership — Deployment Reference

**Organization:** Minnesota EquiVoice Partnership (MN-EVP), 501(c)(3)  
**Primary domain:** mnequivoicepartnership.org  
**Colosseum instance:** colosseum.mnequivoicepartnership.org  
**Last updated:** 2026-06-18

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Infrastructure](#2-infrastructure)
3. [Docker Containers — Colosseum VM](#3-docker-containers--colosseum-vm)
4. [Colosseum Application](#4-colosseum-application)
5. [Colosseum Agents](#5-colosseum-agents)
6. [Environment Variables](#6-environment-variables)
7. [Phone System](#7-phone-system)
8. [FreePBX — Volunteer Phone System](#8-freepbx--volunteer-phone-system)
9. [Adding a New Extension](#9-adding-a-new-extension)
10. [DNS](#10-dns)
11. [CI/CD — Coolify](#11-cicd--coolify)
12. [Persistent Storage](#12-persistent-storage)
13. [Pending Items](#13-pending-items)

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
| POST | /voice/menu | IVR digit handler (1=replay, 2=voicemail) |
| POST | /voice/voicemail | Voicemail prompt |
| POST | /voice/voicemail-done | Voicemail recording complete, notifies admin |
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
1. Twilio POSTs to `/voice/inbound`
2. Colosseum sends an SMS with the booking link to the caller's number
3. Colosseum returns TwiML: plays IVR message (Alice voice), offers menu
4. Press 1 → replay message; Press 2 → voicemail
5. Voicemail completion POSTs to `/voice/voicemail-done`, sends SMS notification to `VITA_VOICEMAIL_NOTIFY_NUMBER`

**Booking URL default:** `https://outlook.office.com/book/MNEVPVITATCE@mnequivoicepartnership.org/`  
Override with `VITA_BOOKING_URL` env var.

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

### AI Providers

| Variable | Description |
|----------|-------------|
| `ANTHROPIC_API_KEY` | Claude models |
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

### A2P 10DLC Registration

**Status:** **In Review** (submitted 2026-06-13) — SMS messages are currently **Undelivered** by carriers until approved.

Campaign SID: `CM70cd3063a4406b93308762ce3177ca5b`  
Brand: Minnesota EquiVoice Partnership (`BN54a91bdb1ddd80348c84fc92ef35b758`)  
Messaging service: `MG91900402c4f9f2500f8832adf07d8452`

CTA URL: `https://mnequivoicepartnership.org/vita-tce-services`  
Privacy Policy URL: `https://mnequivoicepartnership.org/privacy-policy`

After approval: remove `TWILIO_SKIP_SIG_VALIDATION` from Coolify.

---

## 8. FreePBX — Volunteer Phone System

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

## 9. Adding a New Extension

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

## 10. DNS

All records point to Colosseum VM (128.203.195.79) unless noted.

| Hostname | Type | Value | Purpose |
|----------|------|-------|---------|
| mnequivoicepartnership.org | A | — | Main website (WordPress) |
| colosseum.mnequivoicepartnership.org | A | 128.203.195.79 | Colosseum AI platform |
| coolify.mnequivoicepartnership.org | A | 128.203.195.79 | Coolify CI/CD dashboard |
| voice.mnequivoicepartnership.org | A | 20.9.49.33 | FreePBX admin + SIP |
| vita.mnequivoicepartnership.org | A | 128.203.195.79 | **Not yet configured** |

---

## 11. CI/CD — Coolify

**Dashboard:** https://coolify.mnequivoicepartnership.org  
**Direct (IP):** http://128.203.195.79:8000 (restricted to admin IP)  
**GitHub repo:** github.com/mutyabaj/colosseum-go

Push to `master` triggers automatic build and deploy via Coolify. The container is rebuilt from source and restarted with zero-downtime rollover managed by Traefik.

**After each redeploy**, if PDF extraction is needed:
```bash
sudo /opt/equivoice-scripts/restore-docs.sh
```

---

## 12. Persistent Storage

| Path | Volume | Contents |
|------|--------|----------|
| `/data/colosseum.db` | Docker named volume (`/data`) | SQLite database (agents, runs, artifacts, sessions) |
| `/data/workspaces/equivoice-docs/` | Docker named volume (`/data`) | Persistent EquiVoice org documents for grant writer |
| `{workspacePath}/uploads/` | Per-run workspace | Session-scoped file uploads (cleared after run) |

---

## 13. Pending Items

| Item | Priority | Notes |
|------|----------|-------|
| Remove `TWILIO_SKIP_SIG_VALIDATION` from Coolify | High | After A2P campaign approved (currently **In Review** since 2026-06-13) |
| Enable SRTP for ext 102 | Low | Deferred — Linphone SRTP is global and would break Ubiquiti desk phone account. Revisit when Ubiquiti is ported to FreePBX or a per-account SRTP softphone (e.g. Zoiper) replaces Linphone |
| Configure Linphone keep-alive / disable battery optimization | Medium | Prevents registration drops when app is backgrounded |
| Configure `vita.mnequivoicepartnership.org` DNS A record → 128.203.195.79 | Low | Not yet active |
| Add street addresses to vita-page.html location cards | Low | Currently shows placeholder branch names |
| Add extension 101 assignment | Low | Spare extension, assign when next volunteer onboards |
| Renew Asterisk TLS cert after Let's Encrypt renewal | Recurring | Cert expires 2026-08-21; copy to `/etc/asterisk/keys/` and run `fwconsole certificates --import` |
