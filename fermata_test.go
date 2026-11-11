package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	cfg := fermataConfig{Dwell: 2 * time.Hour, Lead: 60 * time.Second}
	role := Role{Form: "@r", Holder: "h", Policy: Policy{Present: true, Enabled: true, Threshold: 30}}
	seated := Seat{Holder: "h", Since: now.Add(-3 * time.Hour)}
	tripAt := func(left time.Duration) time.Time { return now.Add(left - 5*time.Minute) }
	warm := Reading{Context: 40, TTL: 5 * time.Minute, LastTrip: tripAt(45 * time.Second)}
	with := func(f func(*Reading)) Reading { r := warm; f(&r); return r }

	cases := []struct {
		name string
		seat Seat
		read Reading
		ping bool
		why  string
	}{
		{"expiring idle holder above threshold", seated, warm, true, "expiring"},
		{"already pinged this seat", Seat{Holder: "h", Since: seated.Since, Pinged: now.Add(-time.Hour)}, warm, false, "pinged"},
		{"seated under the dwell", Seat{Holder: "h", Since: now.Add(-time.Hour)}, warm, false, "dwell"},
		{"caching off", seated, with(func(r *Reading) { r.TTL = 0 }), false, "cache-off"},
		{"no context limit", seated, with(func(r *Reading) { r.Context = -1 }), false, "context-unknown"},
		{"below threshold", seated, with(func(r *Reading) { r.Context = 29.9 }), false, "below-threshold"},
		{"no round-trip seen", seated, with(func(r *Reading) { r.LastTrip = time.Time{} }), false, "no-trip"},
		{"mid-turn", seated, with(func(r *Reading) { r.Busy = true }), false, "busy"},
		{"cache still warm", seated, with(func(r *Reading) { r.LastTrip = tripAt(61 * time.Second) }), false, "warm"},
		{"one-hour cache", seated, with(func(r *Reading) { r.TTL = time.Hour; r.LastTrip = now.Add(-59*time.Minute - 30*time.Second) }), true, "expiring"},
		{"expired, nothing queued", seated, with(func(r *Reading) { r.LastTrip = tripAt(-time.Second) }), false, "expired"},
		{"expired with a queued message", seated, with(func(r *Reading) { r.LastTrip = tripAt(-time.Second); r.Queued = 1 }), true, "expired-queued"},
		{"expired, busy, queued", seated, with(func(r *Reading) { r.LastTrip = tripAt(-time.Minute); r.Busy = true; r.Queued = 2 }), true, "expired-queued"},
	}
	for _, c := range cases {
		v := decide(now, role, c.seat, c.read, cfg)
		if v.Ping != c.ping || v.Why != c.why {
			t.Errorf("%s: got ping=%v why=%s, want ping=%v why=%s", c.name, v.Ping, v.Why, c.ping, c.why)
		}
	}
}

func TestParsePolicy(t *testing.T) {
	cases := []struct {
		raw  string
		want Policy
	}{
		{``, Policy{Threshold: 30}},
		{`{"begin-at":"60%"}`, Policy{Present: true, Threshold: 30}},
		{`{"cache-succession":true,"cache-threshold":"45%"}`, Policy{Present: true, Enabled: true, Threshold: 45}},
		{`{"cache-succession":"true","cache-threshold":25}`, Policy{Present: true, Enabled: true, Threshold: 25}},
		{`{"cache-succession":false,"cache-threshold":"junk"}`, Policy{Present: true, Threshold: 30}},
	}
	for _, c := range cases {
		if got := parsePolicy(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func TestCacheTTLFollowsFigaro(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 5 * time.Minute, "short": 5 * time.Minute, "5m": 5 * time.Minute,
		"1h": time.Hour, "long": time.Hour, "60m": time.Hour,
		"none": 0, "off": 0,
	} {
		if got := cacheTTL(in); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func tripRow(ts, aria, method, url string, dur, status int) string {
	attr := func(k string, v any) map[string]any {
		return map[string]any{"Key": k, "Value": map[string]any{"Value": v}}
	}
	b, _ := json.Marshal(map[string]any{
		"Timestamp": ts,
		"Body":      map[string]any{"Type": "String", "Value": "provider round-trip"},
		"Attributes": []any{attr("aria", aria), attr("method", method), attr("url", url),
			attr("duration_ms", dur), attr("status", status)},
	})
	return string(b) + "\n"
}

func TestTripLogKeepsLastSuccessfulRequestStart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs.jsonl")
	write := func(s string, flag int) {
		f, err := os.OpenFile(p, flag|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s)
		f.Close()
	}
	write(tripRow("2026-11-10T20:00:10Z", "a", "POST", "https://api.anthropic.com/v1/messages", 2000, 200)+
		tripRow("2026-11-10T20:01:00Z", "a", "POST", "https://api.anthropic.com/v1/messages", 0, 429)+
		tripRow("2026-11-10T20:02:00Z", "a", "GET", "https://api.anthropic.com/v1/models", 0, 200)+
		`{"Body":{"Value":"something else"}}`+"\n", os.O_TRUNC)
	tl := newTripLog(p)
	if err := tl.Refresh(); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 10, 20, 0, 8, 0, time.UTC); !tl.last["a"].Equal(want) {
		t.Fatalf("got %v, want %v", tl.last["a"], want)
	}

	write(tripRow("2026-11-10T20:03:00Z", "a", "POST", "https://x/v1/messages", 500, 200)+`{"partial`, os.O_APPEND)
	tl.Refresh()
	if want := time.Date(2026, 11, 10, 20, 2, 59, 500e6, time.UTC); !tl.last["a"].Equal(want) {
		t.Fatalf("append: got %v, want %v", tl.last["a"], want)
	}

	write(tripRow("2026-11-10T20:04:00Z", "b", "POST", "https://x/v1/messages", 0, 200), os.O_TRUNC)
	tl.Refresh()
	if tl.last["b"].IsZero() {
		t.Fatal("a truncated log was not reread from the start")
	}
}

func TestSeatResetsOnNewHolder(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	born := now.Add(-5 * time.Hour)
	f := &fermata{seats: map[string]Seat{}}
	r := Role{Form: "@r", Holder: "h1"}
	if s := f.seat(r, Reading{Born: born}, now); !s.Since.Equal(born) {
		t.Fatalf("first sight: seated %v, want the holder's birth %v", s.Since, born)
	}
	f.seats["@r"] = Seat{Holder: "h1", Since: born, Pinged: now}
	if s := f.seat(r, Reading{Born: born}, now); s.Pinged.IsZero() {
		t.Fatal("same holder lost its ping")
	}
	r.Holder = "h2"
	if s := f.seat(r, Reading{Born: born}, now); !s.Since.Equal(now) || !s.Pinged.IsZero() {
		t.Fatalf("new holder: got %+v, want a fresh seat at now", s)
	}
}
