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
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizePrefs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means nil
	}{
		{"absent", "", ""},
		{"empty object", "{}", ""},
		{"empty object padded", "  {}\n", ""},
		{"null", "null", ""},
		{"real prefs", `{"KEYS":{"a":1}}`, `{"KEYS":{"a":1}}`},
		{"real prefs trimmed", `  {"MENU":{}}  `, `{"MENU":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePrefs(json.RawMessage(tc.in))
			if string(got) != tc.want {
				t.Fatalf("normalizePrefs(%q) = %q, want %q", tc.in, string(got), tc.want)
			}
		})
	}
}

func TestBuildMetadataFrameWithPrefs(t *testing.T) {
	prefs := []byte(`{"KEYS":{"up":1}}`)
	frame, err := buildMetadataFrame("John", true, "US", prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	name := []byte("John")
	wantLen := 7 + len(name) + len(prefs)
	if len(frame) != wantLen {
		t.Fatalf("frame length = %d, want %d", len(frame), wantLen)
	}
	if frame[0] != metadataType {
		t.Errorf("type byte = %#x, want %#x", frame[0], metadataType)
	}
	if int(frame[1]) != len(name) {
		t.Errorf("name length byte = %d, want %d", frame[1], len(name))
	}
	if string(frame[2:2+len(name)]) != "John" {
		t.Errorf("name = %q, want John", frame[2:2+len(name)])
	}
	p := 2 + len(name)
	if frame[p] != 0x01 {
		t.Errorf("wbn flag = %#x, want 0x01", frame[p])
	}
	if frame[p+1] != 'U' || frame[p+2] != 'S' {
		t.Errorf("country = %q%q, want US", []byte{frame[p+1]}, []byte{frame[p+2]})
	}
	gotLen := binary.BigEndian.Uint16(frame[p+3 : p+5])
	if int(gotLen) != len(prefs) {
		t.Errorf("prefs length prefix = %d, want %d", gotLen, len(prefs))
	}
	if string(frame[p+5:]) != string(prefs) {
		t.Errorf("prefs payload = %q, want %q", frame[p+5:], prefs)
	}
}

func TestBuildMetadataFrameEmptyPrefs(t *testing.T) {
	frame, err := buildMetadataFrame("Al", false, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	name := []byte("Al")
	if len(frame) != 7+len(name) {
		t.Fatalf("frame length = %d, want %d", len(frame), 7+len(name))
	}
	p := 2 + len(name)
	if frame[p] != 0x00 {
		t.Errorf("wbn flag = %#x, want 0x00", frame[p])
	}
	// Country absent -> '??'
	if frame[p+1] != '?' || frame[p+2] != '?' {
		t.Errorf("country = %q%q, want ??", []byte{frame[p+1]}, []byte{frame[p+2]})
	}
	if gotLen := binary.BigEndian.Uint16(frame[p+3 : p+5]); gotLen != 0 {
		t.Errorf("prefs length prefix = %d, want 0", gotLen)
	}
}

func TestBuildMetadataFrameEmptyNameFallback(t *testing.T) {
	frame, err := buildMetadataFrame("", false, "GB", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame[1] != 1 || frame[2] != '?' {
		t.Errorf("empty name should fall back to '?', got len=%d byte=%q", frame[1], []byte{frame[2]})
	}
}

func TestBuildMetadataFrameOversize(t *testing.T) {
	huge := []byte("{" + strings.Repeat("a", maxMetadataFrameSize) + "}")
	if _, err := buildMetadataFrame("John", true, "US", huge); err == nil {
		t.Fatal("expected error for oversized metadata frame, got nil")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(2, time.Minute)
	base := time.Unix(1_000_000, 0)
	rl.now = func() time.Time { return base }

	if !rl.allow("1.2.3.4") {
		t.Fatal("first attempt should be allowed")
	}
	if !rl.allow("1.2.3.4") {
		t.Fatal("second attempt should be allowed")
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("third attempt within window should be rejected")
	}

	// A different IP is independent.
	if !rl.allow("5.6.7.8") {
		t.Fatal("different IP should be allowed")
	}

	// After the window passes, the original IP is allowed again.
	rl.now = func() time.Time { return base.Add(2 * time.Minute) }
	if !rl.allow("1.2.3.4") {
		t.Fatal("attempt after window should be allowed again")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	rl := newRateLimiter(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("limit<=0 should never reject (failed at %d)", i)
		}
	}
}

func TestConnLimiter(t *testing.T) {
	c := &connLimiter{max: 2}
	if !c.acquire() {
		t.Fatal("first acquire should succeed")
	}
	if !c.acquire() {
		t.Fatal("second acquire should succeed")
	}
	if c.acquire() {
		t.Fatal("third acquire should fail at cap")
	}
	if c.count() != 2 {
		t.Fatalf("active count = %d, want 2", c.count())
	}
	c.release()
	if c.count() != 1 {
		t.Fatalf("active count after release = %d, want 1", c.count())
	}
	if !c.acquire() {
		t.Fatal("acquire after release should succeed")
	}
}

func TestHandlePing(t *testing.T) {
	saved := connLimit
	defer func() { connLimit = saved }()

	newReq := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/ping", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	// Below the cap: online, and the allowed origin is reflected back.
	t.Setenv("ALLOWED_ORIGIN", "https://play.winbolo.net")
	connLimit = &connLimiter{max: 2}
	connLimit.acquire()
	rec := httptest.NewRecorder()
	handlePing(rec, newReq("https://play.winbolo.net"))
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"online"}` {
		t.Fatalf("under cap body = %q, want online", body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://play.winbolo.net" {
		t.Fatalf("ACAO = %q, want reflected origin", got)
	}

	// A disallowed origin gets no CORS header (browser blocks the read).
	rec = httptest.NewRecorder()
	handlePing(rec, newReq("https://evil.example"))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO for disallowed origin = %q, want empty", got)
	}

	// At the cap: offline.
	connLimit.acquire()
	if connLimit.count() != 2 {
		t.Fatalf("precondition: count = %d, want 2 (at cap)", connLimit.count())
	}
	rec = httptest.NewRecorder()
	handlePing(rec, newReq(""))
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"offline"}` {
		t.Fatalf("at cap body = %q, want offline", body)
	}

	// Unbounded (max=0) is always online, even with connections active.
	connLimit = &connLimiter{max: 0}
	connLimit.acquire()
	rec = httptest.NewRecorder()
	handlePing(rec, newReq(""))
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"online"}` {
		t.Fatalf("unbounded body = %q, want online", body)
	}
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		allowed string
		want    bool
	}{
		{"empty config allows all", "https://anything.example", "", true},
		{"empty config allows empty origin", "", "", true},
		{"single match", "https://wbn.winbolo.net", "https://wbn.winbolo.net", true},
		{"single mismatch", "https://evil.example", "https://wbn.winbolo.net", false},
		{"list first", "https://wbn.winbolo.net", "https://wbn.winbolo.net,https://wbn.winbolo.com", true},
		{"list second", "https://wbn.winbolo.com", "https://wbn.winbolo.net,https://wbn.winbolo.com", true},
		{"list with spaces", "https://wbn.winbolo.com", "https://wbn.winbolo.net, https://wbn.winbolo.com", true},
		{"not in list", "https://evil.example", "https://wbn.winbolo.net,https://wbn.winbolo.com", false},
		{"empty origin against list rejected", "", "https://wbn.winbolo.net", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := originAllowed(tc.origin, tc.allowed); got != tc.want {
				t.Fatalf("originAllowed(%q, %q) = %v, want %v", tc.origin, tc.allowed, got, tc.want)
			}
		})
	}
}

func TestConnLimiterUnbounded(t *testing.T) {
	c := &connLimiter{max: 0}
	for i := 0; i < 1000; i++ {
		if !c.acquire() {
			t.Fatalf("max<=0 should never reject (failed at %d)", i)
		}
	}
	if c.count() != 1000 {
		t.Fatalf("active count = %d, want 1000", c.count())
	}
}
