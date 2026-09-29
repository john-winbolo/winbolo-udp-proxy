# WinBolo WebSocket-UDP Proxy

Bridges browser WebSocket connections to UDP game servers, allowing [WinBolo](https://github.com/john-winbolo/winbolo) WASM clients to connect to native game servers. Each WebSocket connection gets its own UDP socket so the game server sees each browser as a distinct player.

```
                          ┌─────────────────┐
Browser ──WebSocket──────>│                 │──UDP socket──> Game Server A
                          │  WS-UDP Proxy   │
Browser ──WebSocket──────>│  (this service) │──UDP socket──> Game Server B
                          │                 │
Browser ──WebSocket──────>│                 │──UDP socket──> Game Server A
                          └─────────────────┘
```

## How it works

1. A user clicks "Play" on the WinBolo.net (WBN) website
2. WBN creates a short-lived **join code** (32 hex characters, single-use, expires after 5 minutes) and redirects the browser to the WASM client
3. The WASM client picks the lowest-latency relay (via each relay's `/ping`) and opens a WebSocket to it: `wss://relay-<region>.winbolo.net/proxy?join_code=<32-hex-chars>`
4. The proxy calls the WBN backend API to resolve the join code into the game server address and player details
5. On success, the proxy opens a UDP socket to the game server and sends a **metadata frame** to the client
6. All subsequent frames are raw bidirectional UDP packet relay

## Connection lifecycle

```
Client                          Proxy                         WBN API          Game Server
  │                               │                              │                  │
  │──WS connect ?join_code=xxx──>│                              │                  │
  │                               │──GET /api/join/resolve──────>│                  │
  │                               │<──{server_ip, player...}────│                  │
  │                               │                              │                  │
  │                               │──────────UDP dial───────────────────────────────>│
  │                               │                              │                  │
  │<──metadata frame (0x01)──────│                              │                  │
  │                               │                              │                  │
  │──binary WS frame────────────>│──UDP datagram──────────────────────────────────>│
  │<──binary WS frame───────────│<──UDP datagram──────────────────────────────────│
  │              ...              │              ...              │        ...       │
```

## Metadata frame

After a successful connection, the proxy sends a single binary metadata frame before any game data:

```
Offset   Size  Description
------   ----  -----------
0        1     Message type = 0x01
1        1     Player name length (N, 1–32)
2        N     Player name bytes (UTF-8, NOT null-terminated)
2+N      1     WBN participant flag (0x00 or 0x01)
3+N      1     Country code byte 1 (ASCII, e.g. 'U')
4+N      1     Country code byte 2 (ASCII, e.g. 'S')
5+N      2     Prefs length L (big-endian uint16, 0 if none)
7+N      L     Prefs bytes (raw JSON, relayed verbatim)

Total: 7 + N + L bytes
```

The player name uses a 1-byte (pascal) length prefix; an empty name is sent as
`?` and names longer than 32 bytes are truncated. The prefs field uses a **2-byte
big-endian** length prefix because the prefs JSON can exceed 255 bytes. When the
player has no prefs the length is `0` and there are no prefs bytes. The whole
frame must fit within the 1024-byte max metadata frame size (this is separate from
the 2048-byte game-packet limit); a connection whose metadata would exceed that is
rejected (WS close 1011).

Field sources from the resolve response:

| Frame field | Resolve field | Notes |
|---|---|---|
| Player name | `player_name` | |
| WBN participant flag | `is_logged_in` | The resolve response carries `is_logged_in`, not a separate `wbn_participant` flag. |
| Country code | `country_code` | ISO-3166 alpha-2, captured by WBN when the join code was minted. A 1-char value is space-padded; empty (WBN couldn't geolocate the joiner) ⇒ `??`. |
| Prefs | `prefs` | Opaque JSON object. For logged-in players WBN sends a trimmed, gameplay-only slice of their saved prefs (`KEYS`, whitelisted `MENU` / `GAME OPTIONS` keys, …); anonymous players or accounts with nothing stored get `{}` ⇒ zero-length. Relayed verbatim, never parsed or reshaped. |

### How the client uses it

The WASM client ([`src/bolo/transport_udp_client.c`](https://github.com/john-winbolo/winbolo/blob/main/src/bolo/transport_udp_client.c) in the [winbolo repo](https://github.com/john-winbolo/winbolo)) consumes
the frame once, while joining, if the first message starts with `0x01`. Real game
packets start with the `'W''B'` magic, so the two can't collide. Only the first
binary frame after connection is metadata; everything after it is game data.

- **Prefs** are the only field acted on: when non-empty they're applied to the live
  game (keys, menu toggles, game options) before the first frame. The client later
  also syncs from WBN's `/api/v1/prefs`, which covers anything the join slice omits.
- **Name, WBN flag and country** are logged for diagnostics only. The game server
  establishes a web player's identity itself by verifying the join code with WBN,
  so it never trusts these values.

The frame is optional from the client's point of view: if a relay didn't send it,
the client would go straight to game traffic.

## WebSocket close codes

| Code | Meaning |
|------|---------|
| 4001 | Invalid or expired join code |
| 4002 | Game server unreachable |
| 4003 | Join code already consumed (single-use) |
| 1011 | Metadata frame too large (prefs exceed the 1024-byte frame limit) |

Some rejections happen before the WebSocket upgrade and are plain HTTP errors:
`400` (missing `join_code`), `403` (server not in `SERVER_ALLOWLIST`), `429` (per-IP
rate limit), `503` (`MAX_CONNECTIONS` reached) and `500` (WBN API unreachable or
misconfigured).

## Deployment layouts

Two ready-to-use stacks live under `deploy/`. Each has its own `.env.example`;
copy it to `.env`, fill it in, and `docker compose up -d` from that directory.

| | `deploy/main/` | `deploy/relay/` |
|---|---|---|
| **Use on** | the main box, where a shared Caddy already fronts many services | a regional box (AU, EU, …) that also runs game servers |
| **Caddy** | your existing shared Caddy (proxy joins its external network) | bundled in the stack — brings its own Caddy + TLS |
| **`WBN_API_URL`** | WBN's local address (e.g. `http://host.docker.internal:8081`) | the **central** public API (`https://wbn.winbolo.net`) — join codes are global |
| **Extra Caddy config** | add the block in `deploy/main/Caddyfile-snippet.txt` to your shared Caddyfile | none — `deploy/relay/Caddyfile` is complete |

Why regional relays exist: a player in Australia connecting through the US relay
would route US↔AU twice. Running a relay on the in-region game-server box keeps the
browser's WebSocket local. All relays resolve join codes against the same central
WBN, so a code works no matter which relay a player connects to.

## Quick start

### Prerequisites

- Docker and Docker Compose
- A running WBN backend (for join code resolution)

### 1. Configure environment

```bash
cd deploy/main        # or deploy/relay for a regional relay
cp .env.example .env
$EDITOR .env          # fill in WBN_API_URL, ALLOWED_ORIGIN, etc.
```

### 2. Start the proxy

```bash
docker compose up -d
```

The proxy listens on port **8085** inside the stack; public traffic arrives via
Caddy over `wss://` (443).

### 3. Verify it's running

```bash
# From an IP in METRICS_ALLOW_IP:
curl https://relay-au.winbolo.net/health
# Latency probe (any origin):
curl https://relay-au.winbolo.net/ping   # -> {"status":"online"}
```

## Configuration

All configuration is via environment variables, supplied through the `.env` file
in each `deploy/` stack (see `.env.example`).

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8085` | Address the proxy listens on |
| `WBN_API_URL` | `""` | WBN backend for join code resolution. Required. Either a full URL (`https://wbn.winbolo.net`, `http://host.docker.internal:8081`) or a bare host (`wbn.winbolo.net`), which defaults to HTTPS |
| `WBN_API_INSECURE` | `""` | Set to `true` to use HTTP for a **bare-host** `WBN_API_URL`. Ignored when the URL already has a scheme. **Development only** |
| `ALLOWED_ORIGIN` | `""` (allow all) | Restrict WebSocket connections to these HTTP origins. Single origin, or a comma-separated list, e.g. `https://wbn.winbolo.net,https://wbn.winbolo.com`. Empty allows all. |
| `SERVER_ALLOWLIST` | `""` (allow all) | Comma-separated `host:port` list of permitted game servers. Others are rejected with `403` |
| `MAX_CONNECTIONS` | `500` | Maximum concurrent active connections. New connections past this are rejected with `503`. `0` disables the cap. |
| `RATE_LIMIT_PER_IP` | `30` | Maximum new connections allowed per source IP within `RATE_LIMIT_WINDOW`. Over-limit connections are rejected with `429`. `0` disables the limit. |
| `RATE_LIMIT_WINDOW` | `1m` | Sliding window for `RATE_LIMIT_PER_IP`. Go duration string, e.g. `30s`, `1m`, `5m`. |

The per-IP rate limit and the concurrent-connection cap are both checked **before**
the join code is resolved and before any UDP dial or WebSocket upgrade, so abusive
callers never reach the WBN API or game server. When running behind a reverse proxy,
the source IP is taken from the first hop of `X-Forwarded-For`.

## Local development

No Docker, Caddy or TLS needed. Point the proxy at a local WBN over plain HTTP:

```bash
cd src
WBN_API_URL=localhost:8080 WBN_API_INSECURE=true go run .
go test ./...
```

With `ALLOWED_ORIGIN` unset, any origin may connect. The client then uses
`ws://localhost:8085/proxy?join_code=...`. Plain `ws://` only works when the page
itself is served over `http://`.

## WBN API

The proxy calls the WBN backend to resolve join codes:

```
GET https://<WBN_API_URL>/api/join/resolve?code=<join_code>
```

**Success (200):**
```json
{
  "server_ip": "1.2.3.4",
  "server_port": 27500,
  "player_name": "John",
  "country_code": "AU",
  "user_id": 42,
  "is_logged_in": true,
  "ip_address": "5.6.7.8",
  "prefs": { "KEYS": {}, "MENU": {}, "GAME OPTIONS": {} }
}
```

`server_ip` is the canonical game-server address field. `is_logged_in` drives the
metadata frame's WBN-participant flag, and `country_code` is `""` when WBN couldn't
geolocate the joiner. `prefs` is a trimmed, gameplay-only slice of a logged-in
player's saved prefs (`{}` for anonymous players or when nothing is stored). It is
relayed to the client verbatim; the proxy never parses or reshapes it.

**Invalid/expired (404):**
```json
{
  "error": "Invalid or expired join code"
}
```

**Already consumed (409):**
```json
{
  "error": "Join code already used"
}
```

## Browser / WASM usage

```javascript
const ws = new WebSocket("wss://relay-au.winbolo.net/proxy?join_code=abc123def456...");
ws.binaryType = "arraybuffer";

ws.onmessage = (event) => {
  const data = new Uint8Array(event.data);

  if (data[0] === 0x01) {
    // Metadata frame — parse player info
    const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
    const nameLen = data[1];
    const playerName = new TextDecoder().decode(data.slice(2, 2 + nameLen));
    const wbnParticipant = data[2 + nameLen] === 0x01;
    const countryCode = String.fromCharCode(data[3 + nameLen], data[4 + nameLen]);
    const prefsLen = view.getUint16(5 + nameLen, false); // big-endian
    const prefs = prefsLen
      ? JSON.parse(new TextDecoder().decode(data.slice(7 + nameLen, 7 + nameLen + prefsLen)))
      : {};
    console.log("Connected:", playerName, countryCode, wbnParticipant, prefs);
    return;
  }

  // Game data (starts with the 'W''B' magic)
  handleBoloPacket(data);
};

ws.onclose = (event) => {
  if (event.code === 4001) console.error("Invalid or expired join code");
  if (event.code === 4002) console.error("Game server unreachable");
  if (event.code === 4003) console.error("Join code already used");
};

// Send game packets
ws.send(packetBytes);
```

## WASM client integration

The browser client lives in the [winbolo repo](https://github.com/john-winbolo/winbolo). The relevant code:

| Path | What it does |
|---|---|
| [`src/bolo/transport_udp_client.c`](https://github.com/john-winbolo/winbolo/blob/main/src/bolo/transport_udp_client.c) | Client network transport. On the web build it runs over this proxy's WebSocket and consumes the `0x01` metadata frame (`udpClientProcessPacket`, `transportUdpParseProxyMeta`) |
| [`src/wasm/main_wasm.c`](https://github.com/john-winbolo/winbolo/blob/main/src/wasm/main_wasm.c) | Browser entry point; `wasmApplyJoinPrefs` applies the prefs carried in the metadata frame |
| [`src/wasm/prefs_bridge_wasm.c`](https://github.com/john-winbolo/winbolo/blob/main/src/wasm/prefs_bridge_wasm.c) | Cloud prefs sync against WBN's `/api/v1/prefs` |
| [`src/wasm/`](https://github.com/john-winbolo/winbolo/tree/main/src/wasm) | The rest of the Emscripten build (shell page, sound, voice, stubs) |

The packet format, sequencing and CRC are shared with the native client; only the
socket layer differs.

## Running behind Caddy (production)

Caddy's `reverse_proxy` directive handles WebSocket upgrades automatically and
terminates TLS, so the browser connects over `wss://`. Note that an HTTPS page
(`https://play.winbolo.net`) **cannot** open a plaintext `ws://` socket — the
relay must be served over `wss://`, which is what the Caddy TLS front end provides.

Complete, ready-to-use Caddy config ships in the `deploy/` stacks:

- **Regional relay:** `deploy/relay/Caddyfile` (used as-is by `deploy/relay/docker-compose.yml`).
- **Main box:** `deploy/main/Caddyfile-snippet.txt` — a site block to paste into your existing shared Caddyfile.

Both expose the same public surface:

| Path | Behaviour |
|---|---|
| `/proxy` | reverse-proxied to `proxy:8085` (WebSocket) |
| `/ping` | Proxy-served relay probe (via Caddy). Returns `{"status":"online"}`, or `{"status":"offline"}` once `MAX_CONNECTIONS` is reached, so the lobby can pick a fast relay that still has capacity. CORS mirrors `ALLOWED_ORIGIN`. |
| `/health`, `/metrics` | reverse-proxied, but restricted to an IP allowlist (`METRICS_ALLOW_IP`) |
| everything else | temporary (302) redirect to `https://bolo.io` |

Here `proxy:8085` is the proxy container reachable on the shared Docker network
(service name `proxy`, listen port `8085`). The browser then connects with:

```
wss://relay.winbolo.net/proxy?join_code=<32-hex-chars>
```

### Required runtime configuration

| Variable | Production value | Why |
|---|---|---|
| `ALLOWED_ORIGIN` | `https://play.winbolo.com,https://play.winbolo.net` | Only the game frontend may open relay connections |
| `WBN_API_URL` | Main box: WBN's local address (`http://host.docker.internal:8081`). Regional relay: the central public API (`https://wbn.winbolo.net`) | Used to resolve join codes. Join codes are global, so every relay resolves against the same WBN |
| `SERVER_ALLOWLIST` | Game-server `IP:port` **as seen from inside the container** | Prevents the proxy being used as an open UDP relay |

### Docker UDP networking

The container must be able to reach the game server's **UDP** port. The address in
`SERVER_ALLOWLIST` (and the one resolve returns) is interpreted from inside the
container's network namespace, so either:

- run the proxy with **host networking** (`network_mode: host`) so it shares the
  host's view of the game server's LAN/UDP address, or
- put the proxy on a **shared Docker network** that can route UDP to the game
  server, and use the server address reachable on that network.

UDP datagrams (unlike the inbound WebSocket/TCP traffic) are not proxied by Caddy —
they leave the container directly to the game server, so the container's own
network must have a path to the server's UDP port.

The `deploy/relay` stack already uses host networking. The game server runs on the
same host, and WBN hands back its **public** IP; a bridge-networked container can't
reliably reach its own host's public IP (Docker's NAT hairpin drops the UDP). With
host networking the proxy listens on `127.0.0.1:8085`, so only Caddy can reach it.

### Other production steps

1. **Never publish port 8085.** Both `deploy/` stacks keep the proxy reachable only by Caddy (shared external network on the main box, loopback on a relay); publishing it would bypass Caddy's `/health` / `/metrics` IP allowlist.

2. **Set `ALLOWED_ORIGIN`** to the game frontend origins (`https://play.winbolo.com,https://play.winbolo.net`) so only your frontend can open connections.

3. **Set `SERVER_ALLOWLIST`** to the known game server addresses to prevent the proxy from being used as an open relay.

4. **Tune `MAX_CONNECTIONS`, `RATE_LIMIT_PER_IP`, and `RATE_LIMIT_WINDOW`** for your expected load.

Real client IPs are read from the `X-Forwarded-For` header that Caddy sets automatically, so logs (and the per-IP rate limiter) use correct addresses rather than Caddy's internal IP.

## Endpoints

| Path | Description |
|---|---|
| `/proxy` | WebSocket endpoint. Requires `?join_code=` query parameter (alias: `?joinCode=`) |
| `/ping` | Load-aware relay probe for the lobby. Returns `{"status":"online"}`, or `{"status":"offline"}` once `MAX_CONNECTIONS` active connections are reached (always `online` when the cap is `0`). Mirrors `ALLOWED_ORIGIN` for CORS. |
| `/health` | HTTP health check. Returns `200 OK` |
| `/metrics` | JSON status snapshot (see below) |

### Metrics

`/metrics` returns a minimal JSON status snapshot. JSON (rather than a Prometheus
exposition format) is used to avoid adding the Prometheus client library and its
dependency tree — the proxy depends only on `gorilla/websocket`.

```json
{
  "uptime_seconds": 86400,
  "active_connections": 3,
  "total_connections": 142,
  "bytes_ws_to_udp": 81920,
  "bytes_udp_to_ws": 256000,
  "rejected_rate_limit": 4,
  "rejected_max_connections": 0,
  "ws_close_codes": { "4001": 2, "4002": 1 }
}
```

| Field | Meaning |
|---|---|
| `uptime_seconds` | Whole seconds since the proxy process started |
| `active_connections` | Live count of in-flight relayed connections |
| `total_connections` | Connections accepted since start |
| `bytes_ws_to_udp` / `bytes_udp_to_ws` | Total bytes relayed per direction |
| `rejected_rate_limit` | Connections rejected by the per-IP rate limit |
| `rejected_max_connections` | Connections rejected by the concurrent-connection cap |
| `ws_close_codes` | Count of WebSocket close codes sent, keyed by code |

## Logging

The proxy logs connection lifecycle events with microsecond timestamps and per-connection IDs for tracing:

```
join code API request: GET https://wbn.winbolo.net/api/join/resolve?code=abc123...
join code API response: status=200 content-type=application/json body="{\"server_ip\":\"1.2.3.4\",...}"
[conn#1] CONNECT    client=5.6.7.8 server=1.2.3.4:27500 udp-local=0.0.0.0:44001 player=John country=US wbn=true
[conn#1] METADATA   player=John country=US wbn=true prefs=112B (123 bytes total)
[conn#1] WS->UDP   42 bytes (total pkts=1 bytes=42)
[conn#1] UDP->WS   128 bytes (total pkts=1 bytes=128)
[conn#1] DISCONNECT client=5.6.7.8 server=1.2.3.4:27500  ws->udp pkts=150 bytes=6300  udp->ws pkts=200 bytes=25600
```

Error cases:

```
join code rejected client=5.6.7.8 join_code=expired123: invalid or expired join code
rate limit exceeded client=5.6.7.8: rejecting connection (limit=30/1m0s)
max connections reached (500 active, cap=500): rejecting client=5.6.7.8
rejected client=5.6.7.8 server=9.9.9.9:27500 (not in allowlist)
```

Note that the `join code API response` line logs the full resolve body, including
the player's IP address and prefs.

## Production checklist

- [ ] Copy the right `.env.example` to `.env` (`deploy/main` or `deploy/relay`) and fill it in
- [ ] Set `WBN_API_URL` — WBN's local address on the main box, the **central** public API on a regional relay
- [ ] Set `ALLOWED_ORIGIN` to your game frontend origin(s)
- [ ] Set `SERVER_ALLOWLIST` if you want to restrict which game servers are reachable
- [ ] Set `METRICS_ALLOW_IP` so `/health` and `/metrics` aren't public (relay stack)
- [ ] Point public DNS for each `RELAY_DOMAIN` at the box, with ports 80/443 open for ACME
- [ ] Keep the `caddy_data` volume persistent so TLS certs survive restarts
- [ ] Never publish the proxy's `8085` to the host — let Caddy reach it over the internal network

## License

Copyright (c) 2026 John Morrison.

This program is free software; you can redistribute it and/or modify it under the
terms of the GNU General Public License as published by the Free Software
Foundation; either version 2 of the License, or (at your option) any later version.
See [LICENSE](LICENSE) for the full text.
