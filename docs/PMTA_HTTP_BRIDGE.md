# PMTA HTTP-to-SMTP Bridge — Operations Guide

## Overview

The PMTA HTTP Bridge is a lightweight Python service running on the OVH PMTA server
(`15.204.101.125`) that accepts email injection requests over HTTP and relays them to
PMTA's local SMTP listener. This bypasses AWS ECS Fargate's outbound SMTP port blocking
(ports 25 and 587 are throttled/blocked from AWS compute).

**Why not use PMTA's native HTTP injection API?**
PMTA v5.0r7 does not support the `http-listener` config directive. That feature was
introduced in later versions. This bridge provides the same `/api/inject/v1` endpoint
contract so the Go codebase works without modification.

---

## Server Specifications

| Property | Value |
|---|---|
| **Host** | `15.204.101.125` (OVH Dedicated, Rocky Linux) |
| **CPU** | 12 cores |
| **RAM** | 30 GB |
| **Disk** | 892 GB (2% used) |
| **PMTA Version** | PowerMTA v5.0r7 |
| **Python** | 3.9.25 |
| **SSH User** | `rocky` (sudo, key-based auth) |
| **SSH Key** | `~/.ssh/ovh_pmta` (ed25519, passphrase: `football`) |

---

## Architecture

```
┌──────────────────┐         HTTP POST :19099        ┌──────────────────────────┐
│  AWS ECS Fargate │  ──────────────────────────────► │  PMTA Server (OVH)       │
│  (Go application)│    /api/inject/v1               │                          │
│                  │    JSON payload                  │  pmta-http-bridge.py     │
│  sendViaPMTAAPI()│                                  │  ↓                       │
│                  │                                  │  SMTP 127.0.0.1:25       │
│                  │                                  │  ↓                       │
│                  │                                  │  PMTA daemon             │
│                  │                                  │  ↓ (DKIM, pool routing)  │
│                  │                                  │  Outbound to ISPs        │
└──────────────────┘                                  └──────────────────────────┘
```

---

## Endpoints

### `POST /api/inject/v1` — Inject Email

Accepts a JSON payload and relays the raw RFC822 content to PMTA via local SMTP.

**Request:**

```json
{
  "envelope_sender": "hello@em.discountblog.com",
  "recipients": [
    {"email": "user@gmail.com"}
  ],
  "content": "From: Discount Blog <hello@em.discountblog.com>\r\nTo: user@gmail.com\r\nSubject: Test\r\nMessage-ID: <uuid@pmta-api>\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<html><body>Hello</body></html>",
  "vmta": "mta1"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `envelope_sender` | string | Yes | MAIL FROM address |
| `recipients` | array of `{email: string}` | Yes | RCPT TO addresses |
| `content` | string | Yes | Full RFC822 message (headers + body) |
| `vmta` | string | No | Virtual MTA name — injects `X-Virtual-MTA` header for pool routing |

**Response (200 OK):**

```json
{
  "status": "ok",
  "message_id": "f549ba9a-4018-407a-9a5d-12feb0b93257",
  "recipients_accepted": 1,
  "timestamp": "2026-03-02T21:16:03.326874"
}
```

**Response (400 Bad Request):**

Returned when required fields are missing or JSON is malformed.

**Response (502 Bad Gateway):**

Returned when the bridge cannot connect to PMTA's local SMTP. This means PMTA is down
or restarting. The Go client should retry with backoff.

---

### `GET /health` — Health Check

```json
{"status": "ok", "service": "pmta-http-bridge"}
```

Returns 200 if the bridge process is alive. Does NOT verify PMTA SMTP is reachable
(that only happens on inject).

---

## Size Constraints & Limits

### Current Limits

| Constraint | Value | Source |
|---|---|---|
| **Max message size (local SMTP)** | **Unlimited** | PMTA `<source 127.0.0.1>` config: `max-message-size unlimited` |
| **Max message size (auth SMTP)** | **50 MB** | PMTA `<source {auth}>` config: `max-message-size 50M` |
| **HTTP body size** | **No explicit limit** | Python `http.server` reads `Content-Length` bytes; limited by available RAM |
| **SMTP timeout** | **30 seconds** | Bridge hardcoded `timeout=30` per connection |
| **HTTP client timeout (Go side)** | **30 seconds** | `sendViaPMTAAPI` uses `http.Client{Timeout: 30 * time.Second}` |
| **Per-IP message rate (warmup)** | **200/hour** per VTA, 50/hour for Gmail/Yahoo/Outlook/Hotmail | PMTA config per `<virtual-mta>` |
| **Max concurrent SMTP connections** | **10** per VTA to default domains, **2** to Gmail/Yahoo/Outlook/Hotmail | PMTA `max-smtp-out` |
| **File descriptor limit** | **1024** | Default `ulimit -n` for the rocky user |
| **Spool disk** | **882 GB free** | `/var/spool/pmta` on `/dev/md3` |

### Recommended Hardening

For production at 400k+ emails, these limits should be tightened on the bridge:

| Setting | Recommended Value | Why |
|---|---|---|
| Max request body | 10 MB | Prevents memory exhaustion from malformed payloads |
| Max recipients per request | 1 | Go codebase sends one recipient per call; prevent abuse |
| Connection timeout | 10 seconds | Fail fast if PMTA is unresponsive |
| Max concurrent requests | 100 | Python `http.server` is single-threaded; switch to threaded |
| File descriptors | 65535 | Raise `ulimit -n` in systemd service for high concurrency |
| Rate limit | None (PMTA handles this) | PMTA's per-VTA rate limits are the throttle |

---

## File Locations

| File | Path | Purpose |
|---|---|---|
| Bridge script | `/usr/local/bin/pmta-http-bridge` | The Python HTTP server |
| Systemd service | `/etc/systemd/system/pmta-http-bridge.service` | Auto-start, auto-restart |
| PMTA config | `/etc/pmta/config` | Main PMTA configuration |
| PMTA config backup | `/etc/pmta/config.bak.YYYYMMDDHHMMSS` | Timestamped backup |
| PMTA log | `/var/log/pmta/pmta.log` | PMTA daemon log |
| PMTA accounting | `/var/log/pmta/acct.csv` | Delivery/bounce/complaint records |
| PMTA bounce log | `/var/log/pmta/bounce.csv` | Bounce-specific records |
| PMTA spool | `/var/spool/pmta/` | Message queue |
| Bridge logs | `journalctl -u pmta-http-bridge` | Bridge access + error logs |

---

## Systemd Service

```ini
[Unit]
Description=PMTA HTTP-to-SMTP Bridge
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/python3 /usr/local/bin/pmta-http-bridge
Restart=always
RestartSec=3
Environment=BRIDGE_PORT=19099
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

### Service Commands

```bash
# Status
sudo systemctl status pmta-http-bridge

# Start / Stop / Restart
sudo systemctl start pmta-http-bridge
sudo systemctl stop pmta-http-bridge
sudo systemctl restart pmta-http-bridge

# View logs (last 50 lines, follow)
sudo journalctl -u pmta-http-bridge -n 50 -f

# Enable on boot
sudo systemctl enable pmta-http-bridge
```

---

## Firewall

Port 19099/tcp is open in `firewalld`:

```bash
# Verify
sudo firewall-cmd --list-ports
# Should include: 25/tcp 587/tcp 19000/tcp 19001/tcp 19099/tcp

# If missing, add:
sudo firewall-cmd --permanent --add-port=19099/tcp
sudo firewall-cmd --reload
```

---

## Go Codebase Integration

### Two Go callers exist:

**1. `internal/api/mailing_sending.go:662` — `sendViaPMTAAPI()`**

Used by `HandleSendTestEmail` and the test send flow. Already checks `profile.APIEndpoint`
first and falls back to SMTP.

**2. `internal/worker/esp_pmta_api.go:42` — `PMTAAPISender.Send()`**

Used by the async send worker pool. Accepts an `EmailMessage` struct and includes
campaign/subscriber tracking headers (`X-Campaign-ID`, `X-Subscriber-ID`).

### NOT yet wired:

**`internal/api/campaign_builder_send_sync.go:229-238`**

The campaign send loop hardcodes SMTP-only for `case "pmta"`. This must be updated to
check `profile.APIEndpoint` first, matching the pattern in `mailing_sending.go:182-184`.

### Database: Sending Profile `api_endpoint`

The DiscountBlog sending profile needs `api_endpoint` set:

```sql
UPDATE mailing_sending_profiles
SET api_endpoint = 'http://15.204.101.125:19099'
WHERE id = '10dc7031-8f39-40e3-80c0-4c3f8286df73';
```

Or via API:

```bash
curl -X PATCH "https://projectjarvis.io/api/mailing/sending-profiles/10dc7031-8f39-40e3-80c0-4c3f8286df73" \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "X-Organization-ID: 00000000-0000-0000-0000-000000000001" \
  -d '{"api_endpoint": "http://15.204.101.125:19099"}'
```

---

## PMTA Pool Routing via `vmta` Field

The bridge supports an optional `vmta` field in the JSON payload. When present, it
injects an `X-Virtual-MTA: {vmta}` header into the RFC822 content before relaying
to PMTA. PMTA's `<source 127.0.0.1>` has `process-x-virtual-mta yes`, so it will
route the message through the specified VTA.

### Available VMTAs and Pools

| VTA Name | IP Address | Hostname |
|---|---|---|
| `mta1` | 15.204.22.176 | mta1.mail.projectjarvis.io |
| `mta2` | 15.204.22.177 | mta2.mail.projectjarvis.io |
| `mta3` | 15.204.22.178 | mta3.mail.projectjarvis.io |
| `mta4` | 15.204.22.179 | mta4.mail.projectjarvis.io |
| `mta5` | 15.204.22.180 | mta5.mail.projectjarvis.io |
| `mta6` | 15.204.22.181 | mta6.mail.projectjarvis.io |
| `mta7` | 15.204.22.182 | mta7.mail.projectjarvis.io |
| `mta8` | 15.204.22.183 | mta8.mail.projectjarvis.io |
| `mta9` | 15.204.22.184 | mta9.mail.projectjarvis.io |
| `mta10` | 15.204.22.185 | mta10.mail.projectjarvis.io |
| `mta11` | 15.204.22.186 | mta11.mail.projectjarvis.io |
| `mta12` | 15.204.22.187 | mta12.mail.projectjarvis.io |
| `mta13` | 15.204.22.188 | mta13.mail.projectjarvis.io |
| `mta14` | 15.204.22.189 | mta14.mail.projectjarvis.io |
| `mta15` | 15.204.22.190 | mta15.mail.projectjarvis.io |
| `mta16` | 15.204.22.191 | mta16.mail.projectjarvis.io |

| Pool Name | VMTAs | Purpose |
|---|---|---|
| `default-pool` | mta1–mta16 | All 16 IPs, round-robin |
| `warmup-pool` | mta1–mta4 | First 4 IPs for warmup sends |

### Rate Limits Per VTA

Each VTA enforces:
- **Default domains**: 200 msg/hour, 10 concurrent SMTP connections
- **Gmail**: 50 msg/hour, 2 concurrent connections
- **Yahoo**: 50 msg/hour, 2 concurrent connections
- **Outlook**: 50 msg/hour, 2 concurrent connections
- **Hotmail**: 50 msg/hour, 2 concurrent connections

**Total throughput** (all 16 VMTAs combined):
- Default domains: 3,200 msg/hour
- Gmail: 800 msg/hour
- Yahoo: 800 msg/hour
- Outlook/Hotmail: 800 msg/hour each

---

## DKIM Signing

PMTA signs outbound mail automatically. The bridge relays through `127.0.0.1:25`
which has `dkim-sign yes` enabled on all VMTAs. Configured domain keys:

| Selector | Domain | Key File |
|---|---|---|
| `pj1` | projectjarvis.io | `/etc/pmta/dkim/projectjarvis.io.key` |
| `db1` | em.discountblog.com | `/etc/pmta/dkim/em.discountblog.com.key` |
| `qf1` | em.quizfiesta.com | `/etc/pmta/dkim/em.quizfiesta.com.key` |

No changes needed — DKIM works transparently through the bridge.

---

## Testing

### Health check from anywhere:

```bash
curl http://15.204.101.125:19099/health
```

### Inject a test email:

```bash
curl -X POST http://15.204.101.125:19099/api/inject/v1 \
  -H "Content-Type: application/json" \
  -d '{
    "envelope_sender": "hello@em.discountblog.com",
    "recipients": [{"email": "your-email@gmail.com"}],
    "content": "From: Test <hello@em.discountblog.com>\r\nTo: your-email@gmail.com\r\nSubject: Bridge Test\r\nMIME-Version: 1.0\r\nContent-Type: text/html\r\n\r\n<h1>Bridge works</h1>"
  }'
```

### Inject with pool routing:

```bash
curl -X POST http://15.204.101.125:19099/api/inject/v1 \
  -H "Content-Type: application/json" \
  -d '{
    "envelope_sender": "hello@em.discountblog.com",
    "recipients": [{"email": "your-email@gmail.com"}],
    "vmta": "mta1",
    "content": "From: Test <hello@em.discountblog.com>\r\nTo: your-email@gmail.com\r\nSubject: Pool Routing Test\r\nMIME-Version: 1.0\r\nContent-Type: text/html\r\n\r\n<h1>Routed through mta1</h1>"
  }'
```

### Check bridge logs:

```bash
ssh rocky@15.204.101.125 "sudo journalctl -u pmta-http-bridge -n 20 --no-pager"
```

---

## Monitoring & Troubleshooting

### Bridge not responding?

```bash
sudo systemctl status pmta-http-bridge
sudo journalctl -u pmta-http-bridge -n 50
```

### Bridge returns 502?

PMTA's local SMTP is unreachable. Check PMTA:

```bash
sudo systemctl status pmta
sudo tail -50 /var/log/pmta/pmta.log
echo "QUIT" | nc 127.0.0.1 25
```

### PMTA stuck in "activating"?

PMTA loads queued messages from `/var/spool/pmta/` on startup. With a large queue
this can take 30-120 seconds. Wait for it or check:

```bash
sudo journalctl -u pmta -f
```

### Port 19099 not reachable from outside?

```bash
sudo firewall-cmd --list-ports  # should include 19099/tcp
curl http://127.0.0.1:19099/health  # test locally first
```

---

## Durability Considerations

### What survives a reboot?
- **Bridge**: Yes — `systemctl enable pmta-http-bridge` is set
- **PMTA**: Yes — `systemctl enable pmta` is set
- **Firewall rules**: Yes — `--permanent` flag was used
- **Queued mail**: Yes — PMTA spools to `/var/spool/pmta/` (892 GB disk)

### What does NOT survive?
- **In-flight HTTP requests**: If the bridge crashes mid-relay, the HTTP client
  gets a connection reset. The Go caller should retry.
- **Python process state**: The bridge is stateless — no queue, no buffer. Each
  request is an independent SMTP relay. If it crashes, systemd restarts it in 3s.

### Single point of failure?
- The bridge is single-threaded Python `http.server`. Under heavy concurrent load
  (100+ simultaneous connections), requests will queue at the OS TCP level.
- For production at scale, consider replacing with a threaded/async version
  (e.g., using `ThreadingHTTPServer` or a `gunicorn`/`uvicorn` deployment).

### Recommended production upgrade:

Replace the `HTTPServer` line in the bridge with:

```python
from http.server import ThreadingHTTPServer
httpd = ThreadingHTTPServer(("0.0.0.0", LISTEN_PORT), InjectHandler)
```

And add to the systemd service:

```ini
LimitNOFILE=65535
```

This allows the bridge to handle concurrent injection requests (one thread per
request, each opening its own SMTP connection to 127.0.0.1:25).
