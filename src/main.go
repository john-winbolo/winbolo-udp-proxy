// Copyright (c) 2026 John Morrison.
//
// This program is free software; you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; either version 2 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// maxRelayPacketSize bounds relayed game datagrams in both directions. The
	// game protocol builds datagrams up to 1400 bytes (1392 payload + 8-byte
	// header), so this MUST exceed that: a short read buffer truncates silently
	// (Read returns n == len(buf) with a nil error and the kernel discards the
	// rest), and the client's bounds-checked parser then drops the frame.
	maxRelayPacketSize = 2048

	// maxMetadataFrameSize bounds the metadata frame sent to the client on
	// connect. Unrelated to the UDP path -- it is a WebSocket-only frame.
	maxMetadataFrameSize = 1024

	udpTimeout       = 30 * time.Second
	metadataType     = 0x01
	maxPlayerNameLen = 32

	// WebSocket close codes
	closeInvalidJoinCode   = 4001
	closeServerUnreachable = 4002
	closeJoinCodeConsumed  = 4003

	// Defaults for the connection guards (overridable via env)
	defaultMaxConnections  = 500
	defaultRateLimitPerIP  = 30
	defaultRateLimitWindow = time.Minute
)

var (
	upgrader    websocket.Upgrader
	connCounter atomic.Uint64 // monotonic, used only to mint per-connection IDs
	httpClient  = &http.Client{Timeout: 10 * time.Second}

	// Connection guards, initialised from env in main().
	connLimit *connLimiter
	ipLimiter *rateLimiter
)

func init() {
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return originAllowed(r.Header.Get("Origin"), os.Getenv("ALLOWED_ORIGIN"))
		},
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

// originAllowed reports whether origin matches the ALLOWED_ORIGIN config.
// The config is a comma-separated list of exact origins (e.g.
// "https://wbn.winbolo.net,https://wbn.winbolo.com"); an empty config allows all.
func originAllowed(origin, allowed string) bool {
	allowed = strings.TrimSpace(allowed)
	if allowed == "" {
		return true
	}
	for _, entry := range strings.Split(allowed, ",") {
		if strings.TrimSpace(entry) == origin {
			return true
		}
	}
	return false
}

type joinCodeResponse struct {
	ServerIP   string `json:"server_ip"`
	ServerPort int    `json:"server_port"`
	PlayerName string `json:"player_name"`
	// CountryCode is ISO-3166 alpha-2, captured by WBN at mint time; "" when WBN
	// could not geolocate the joiner (the metadata frame then carries "??").
	CountryCode string `json:"country_code"`
	UserID      *int   `json:"user_id"`
	// IsLoggedIn is the source for the metadata frame's WBN-participant flag.
	IsLoggedIn bool   `json:"is_logged_in"`
	IPAddress  string `json:"ip_address"`
	// Prefs is captured verbatim and relayed opaquely to the client. It is a JSON
	// object such as {"KEYS":{…},"MENU":{…}} or {} when the player has none.
	Prefs json.RawMessage `json:"prefs"`
}

type joinCodeError struct {
	Error string `json:"error"`
}

func resolveJoinCode(joinCode string) (*joinCodeResponse, int, error) {
	baseURL := os.Getenv("WBN_API_URL")
	if baseURL == "" {
		return nil, 0, fmt.Errorf("WBN_API_URL not configured")
	}

	// Strip trailing slashes
	baseURL = strings.TrimRight(baseURL, "/")

	// If WBN_API_URL already has a scheme, use it as-is.
	// Otherwise, default to https (or http if WBN_API_INSECURE=true).
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		scheme := "https"
		if os.Getenv("WBN_API_INSECURE") == "true" {
			scheme = "http"
		}
		baseURL = scheme + "://" + baseURL
	}

	url := fmt.Sprintf("%s/api/join/resolve?code=%s", baseURL, joinCode)
	log.Printf("join code API request: GET %s", url)
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("reading response: %w", err)
	}

	log.Printf("join code API response: status=%d content-type=%s body=%q", resp.StatusCode, resp.Header.Get("Content-Type"), string(body))

	if resp.StatusCode == http.StatusNotFound {
		return nil, closeInvalidJoinCode, fmt.Errorf("invalid or expired join code")
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, closeJoinCodeConsumed, fmt.Errorf("join code already consumed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, closeInvalidJoinCode, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result joinCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, 0, fmt.Errorf("parsing response (body=%q): %w", string(body), err)
	}

	if result.ServerIP == "" || result.ServerPort == 0 {
		return nil, 0, fmt.Errorf("invalid server details in response: server_ip=%q server_port=%d", result.ServerIP, result.ServerPort)
	}

	return &result, 0, nil
}

// normalizePrefs returns the prefs JSON as raw bytes to embed, or nil when the
// player has no prefs. Absent, empty, "{}" and "null" all collapse to nil so the
// frame carries a zero-length prefs field in those cases. The content is treated
// as opaque: it is not parsed, validated, or reshaped.
func normalizePrefs(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	switch string(trimmed) {
	case "", "{}", "null":
		return nil
	}
	return trimmed
}

// buildMetadataFrame builds the binary metadata frame:
//
//	[0x01] [name_len] [name_bytes...] [wbn_flag] [cc_byte1] [cc_byte2] [prefs_len_hi] [prefs_len_lo] [prefs_bytes...]
//
// Sizes: name_len is a 1-byte length (name is clamped to maxPlayerNameLen).
// prefs_len is a 2-byte big-endian length because prefs can exceed 255 bytes.
// The whole frame must fit within maxMetadataFrameSize; an oversized frame is an error.
func buildMetadataFrame(playerName string, wbn bool, countryCode string, prefs []byte) ([]byte, error) {
	name := []byte(playerName)
	if len(name) > maxPlayerNameLen {
		name = name[:maxPlayerNameLen]
	}
	if len(name) == 0 {
		name = []byte("?")
	}

	total := 7 + len(name) + len(prefs)
	if total > maxMetadataFrameSize {
		return nil, fmt.Errorf("metadata frame too large: %d bytes exceeds max frame size %d (prefs=%d bytes)", total, maxMetadataFrameSize, len(prefs))
	}

	frame := make([]byte, total)
	frame[0] = metadataType
	frame[1] = byte(len(name))
	copy(frame[2:], name)

	p := 2 + len(name)

	// WBN participant flag
	if wbn {
		frame[p] = 0x01
	} else {
		frame[p] = 0x00
	}
	p++

	// Country code: 2 ASCII bytes, pad with space if short, '??' if absent
	cc := countryCode
	switch {
	case len(cc) >= 2:
		frame[p] = cc[0]
		frame[p+1] = cc[1]
	case len(cc) == 1:
		frame[p] = cc[0]
		frame[p+1] = ' '
	default:
		frame[p] = '?'
		frame[p+1] = '?'
	}
	p += 2

	// Prefs: 2-byte big-endian length prefix followed by the raw JSON bytes.
	binary.BigEndian.PutUint16(frame[p:], uint16(len(prefs)))
	p += 2
	copy(frame[p:], prefs)

	return frame, nil
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}

// clientIPOnly returns just the source IP (no port), used as the rate-limit key.
// It honours the first hop of X-Forwarded-For when set by a trusted reverse proxy.
func clientIPOnly(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func wsClose(ws *websocket.Conn, code int, reason string) {
	metrics.recordCloseCode(code)
	msg := websocket.FormatCloseMessage(code, reason)
	ws.WriteMessage(websocket.CloseMessage, msg)
	ws.Close()
}

func handleClient(w http.ResponseWriter, r *http.Request) {
	joinCode := r.URL.Query().Get("join_code")
	if joinCode == "" {
		joinCode = r.URL.Query().Get("joinCode")
	}
	if joinCode == "" {
		http.Error(w, "missing join_code param", http.StatusBadRequest)
		return
	}

	// Per-IP rate limit and total connection cap are checked up front, before any
	// backend resolve, UDP dial, or WebSocket upgrade, so abusive callers are cheap
	// to reject and never touch the game server or WBN API.
	ipKey := clientIPOnly(r)
	if !ipLimiter.allow(ipKey) {
		metrics.rejectedRateLimit.Add(1)
		log.Printf("rate limit exceeded client=%s: rejecting connection (limit=%d/%s)", ipKey, ipLimiter.limit, ipLimiter.window)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if !connLimit.acquire() {
		metrics.rejectedMaxConns.Add(1)
		log.Printf("max connections reached (%d active, cap=%d): rejecting client=%s", connLimit.count(), connLimit.max, clientIP(r))
		http.Error(w, "server at capacity", http.StatusServiceUnavailable)
		return
	}
	// Active-connection gauge is released on every exit path from here on.
	defer connLimit.release()
	metrics.totalConns.Add(1)

	// Resolve join code before upgrading to WebSocket
	resolved, closeCode, err := resolveJoinCode(joinCode)
	if err != nil {
		if closeCode != 0 {
			// Upgrade so we can send a proper WS close code
			ws, upgradeErr := upgrader.Upgrade(w, r, nil)
			if upgradeErr != nil {
				log.Printf("websocket upgrade failed client=%s: %v", clientIP(r), upgradeErr)
				return
			}
			log.Printf("join code rejected client=%s join_code=%s: %v", clientIP(r), joinCode, err)
			wsClose(ws, closeCode, err.Error())
			return
		}
		log.Printf("join code resolve failed client=%s join_code=%s: %v", clientIP(r), joinCode, err)
		http.Error(w, fmt.Sprintf("join code error: %v", err), http.StatusInternalServerError)
		return
	}

	serverAddr := fmt.Sprintf("%s:%d", resolved.ServerIP, resolved.ServerPort)

	if allowlist := os.Getenv("SERVER_ALLOWLIST"); allowlist != "" {
		if !isAllowed(serverAddr, allowlist) {
			log.Printf("rejected client=%s server=%s (not in allowlist)", clientIP(r), serverAddr)
			http.Error(w, "server not allowed", http.StatusForbidden)
			return
		}
	}

	// Build the metadata frame before upgrading so an oversized prefs blob fails
	// the connection cleanly rather than mid-relay.
	prefs := normalizePrefs(resolved.Prefs)
	metaFrame, err := buildMetadataFrame(resolved.PlayerName, resolved.IsLoggedIn, resolved.CountryCode, prefs)
	if err != nil {
		ws, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			log.Printf("metadata frame error client=%s: %v (upgrade also failed: %v)", clientIP(r), err, upgradeErr)
			return
		}
		log.Printf("metadata frame error client=%s: %v", clientIP(r), err)
		wsClose(ws, websocket.CloseInternalServerErr, "metadata too large")
		return
	}

	// Resolve and dial UDP before upgrading WebSocket
	udpAddr, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		ws, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		log.Printf("invalid server address client=%s server=%s: %v", clientIP(r), serverAddr, err)
		wsClose(ws, closeServerUnreachable, "Game server unreachable")
		return
	}

	udpConn, err := net.DialUDP("udp4", nil, udpAddr)
	if err != nil {
		ws, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		log.Printf("udp dial error server=%s: %v", serverAddr, err)
		wsClose(ws, closeServerUnreachable, "Game server unreachable")
		return
	}
	defer udpConn.Close()

	// Upgrade to WebSocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed client=%s: %v", clientIP(r), err)
		return
	}
	defer ws.Close()

	id := fmt.Sprintf("conn#%d", connCounter.Add(1))
	client := clientIP(r)
	udpLocal := udpConn.LocalAddr().String()

	log.Printf("[%s] CONNECT    client=%s server=%s udp-local=%s player=%s country=%s wbn=%v",
		id, client, serverAddr, udpLocal, resolved.PlayerName, resolved.CountryCode, resolved.IsLoggedIn)

	// Send metadata frame as the first message
	if err := ws.WriteMessage(websocket.BinaryMessage, metaFrame); err != nil {
		log.Printf("[%s] metadata frame write error: %v", id, err)
		return
	}
	log.Printf("[%s] METADATA   player=%s country=%s wbn=%v prefs=%dB (%d bytes total)",
		id, resolved.PlayerName, resolved.CountryCode, resolved.IsLoggedIn, len(prefs), len(metaFrame))

	// Bidirectional relay
	var (
		wg         sync.WaitGroup
		wsToUDP    atomic.Uint64
		udpToWS    atomic.Uint64
		wsPackets  atomic.Uint64
		udpPackets atomic.Uint64
	)

	// WebSocket -> UDP
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				log.Printf("[%s] ws-read closed: %v", id, err)
				break
			}
			n := len(msg)
			if n > maxRelayPacketSize {
				log.Printf("[%s] WS->UDP   DROP oversized packet (%d bytes)", id, n)
				continue
			}
			udpConn.SetWriteDeadline(time.Now().Add(udpTimeout))
			if _, err := udpConn.Write(msg); err != nil {
				log.Printf("[%s] WS->UDP   write error: %v", id, err)
				break
			}
			wsToUDP.Add(uint64(n))
			wsPackets.Add(1)
			metrics.bytesWSToUDP.Add(uint64(n))
			log.Printf("[%s] WS->UDP   %d bytes (total pkts=%d bytes=%d)",
				id, n, wsPackets.Load(), wsToUDP.Load())
		}
		udpConn.Close()
	}()

	// UDP -> WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, maxRelayPacketSize)
		for {
			udpConn.SetReadDeadline(time.Now().Add(udpTimeout))
			n, err := udpConn.Read(buf)
			if err != nil {
				log.Printf("[%s] udp-read closed: %v", id, err)
				break
			}
			// A read that exactly fills the buffer is always a truncation: the
			// kernel discarded the tail and reported no error. Forwarding it
			// would hand the client a malformed frame that it silently drops.
			if n == len(buf) {
				log.Printf("[%s] UDP->WS   WARNING datagram filled the %d-byte buffer; payload was truncated", id, n)
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				log.Printf("[%s] UDP->WS   write error: %v", id, err)
				break
			}
			udpToWS.Add(uint64(n))
			udpPackets.Add(1)
			metrics.bytesUDPToWS.Add(uint64(n))
			log.Printf("[%s] UDP->WS   %d bytes (total pkts=%d bytes=%d)",
				id, n, udpPackets.Load(), udpToWS.Load())
		}
		ws.Close()
	}()

	wg.Wait()
	log.Printf("[%s] DISCONNECT client=%s server=%s  ws->udp pkts=%d bytes=%d  udp->ws pkts=%d bytes=%d",
		id, client, serverAddr,
		wsPackets.Load(), wsToUDP.Load(),
		udpPackets.Load(), udpToWS.Load())
}

func isAllowed(addr, allowlist string) bool {
	for len(allowlist) > 0 {
		var entry string
		if i := indexOf(allowlist, ','); i >= 0 {
			entry, allowlist = allowlist[:i], allowlist[i+1:]
		} else {
			entry, allowlist = allowlist, ""
		}
		if entry == addr {
			return true
		}
	}
	return false
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// connLimiter is a live active-connection gauge with an upper bound. A max <= 0
// means unbounded (the gauge is still tracked for metrics).
type connLimiter struct {
	max    int64
	active atomic.Int64
}

// acquire increments the active-connection count, returning false (and undoing
// the increment) if doing so would exceed the cap.
func (c *connLimiter) acquire() bool {
	n := c.active.Add(1)
	if c.max > 0 && n > c.max {
		c.active.Add(-1)
		return false
	}
	return true
}

func (c *connLimiter) release() { c.active.Add(-1) }

func (c *connLimiter) count() int64 { return c.active.Load() }

// rateLimiter caps new connections per key (source IP) within a sliding window.
// A limit <= 0 disables limiting.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		hits:   make(map[string][]time.Time),
		now:    time.Now,
	}
}

// allow records a new attempt for key and reports whether it is within the limit.
func (r *rateLimiter) allow(key string) bool {
	if r.limit <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	cutoff := now.Add(-r.window)

	// Prune timestamps that have aged out of the window.
	kept := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= r.limit {
		r.hits[key] = kept
		return false
	}
	r.hits[key] = append(kept, now)
	return true
}

// proxyMetrics holds process-wide counters exposed at /metrics.
type proxyMetrics struct {
	bytesWSToUDP      atomic.Uint64
	bytesUDPToWS      atomic.Uint64
	totalConns        atomic.Uint64
	rejectedRateLimit atomic.Uint64
	rejectedMaxConns  atomic.Uint64

	mu         sync.Mutex
	closeCodes map[int]uint64

	// startTime is stamped at package init, i.e. process start.
	startTime time.Time
}

func (m *proxyMetrics) recordCloseCode(code int) {
	m.mu.Lock()
	m.closeCodes[code]++
	m.mu.Unlock()
}

var metrics = &proxyMetrics{closeCodes: make(map[int]uint64), startTime: time.Now()}

// handleMetrics serves a minimal JSON status snapshot. JSON (not Prometheus) is
// used to avoid pulling in the prometheus client library and its dependencies;
// the proxy otherwise depends only on gorilla/websocket.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics.mu.Lock()
	codes := make(map[string]uint64, len(metrics.closeCodes))
	for c, n := range metrics.closeCodes {
		codes[strconv.Itoa(c)] = n
	}
	metrics.mu.Unlock()

	out := map[string]any{
		"uptime_seconds":           int64(time.Since(metrics.startTime).Seconds()),
		"active_connections":       connLimit.count(),
		"total_connections":        metrics.totalConns.Load(),
		"bytes_ws_to_udp":          metrics.bytesWSToUDP.Load(),
		"bytes_udp_to_ws":          metrics.bytesUDPToWS.Load(),
		"rejected_rate_limit":      metrics.rejectedRateLimit.Load(),
		"rejected_max_connections": metrics.rejectedMaxConns.Load(),
		"ws_close_codes":           codes,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("metrics encode error: %v", err)
	}
}

// handlePing is the lobby's load-aware relay probe. Because it is served by the
// proxy (not Caddy), a successful response confirms the proxy itself is up and
// serving — not merely that the TLS front end is reachable. It reports
// "offline" once the active-connection cap is reached so the lobby steers new
// players toward a relay with spare capacity instead of one that would reject
// them. With no cap (MAX_CONNECTIONS=0) the relay is always "online".
func handlePing(w http.ResponseWriter, r *http.Request) {
	status := "online"
	if max := connLimit.max; max > 0 && connLimit.count() >= max {
		status = "offline"
	}

	// Mirror the app's WebSocket CORS policy so the browser lobby can read this
	// cross-origin. Empty ALLOWED_ORIGIN (allow all) reflects any origin.
	if origin := r.Header.Get("Origin"); origin != "" && originAllowed(origin, os.Getenv("ALLOWED_ORIGIN")) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"status\":\"%s\"}\n", status)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("invalid %s=%q, using default %d", key, v, def)
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("invalid %s=%q, using default %s", key, v, def)
	}
	return def
}

func main() {
	addr := envOr("LISTEN_ADDR", ":8085")
	maxConns := envInt("MAX_CONNECTIONS", defaultMaxConnections)
	rlLimit := envInt("RATE_LIMIT_PER_IP", defaultRateLimitPerIP)
	rlWindow := envDuration("RATE_LIMIT_WINDOW", defaultRateLimitWindow)

	connLimit = &connLimiter{max: int64(maxConns)}
	ipLimiter = newRateLimiter(rlLimit, rlWindow)

	http.HandleFunc("/proxy", handleClient)
	http.HandleFunc("/ping", handlePing)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/metrics", handleMetrics)

	log.Printf("WinBolo WebSocket-UDP proxy listening on %s (max_connections=%d, rate_limit=%d/%s)",
		addr, maxConns, rlLimit, rlWindow)
	log.Fatal(http.ListenAndServe(addr, nil))
}
