package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		{"held for a window", seated, warm, false, "held"},
		{"expired, busy, queued", seated, with(func(r *Reading) { r.LastTrip = tripAt(-time.Minute); r.Busy = true; r.Queued = 2 }), true, "expired-queued"},
	}
	for _, c := range cases {
		role.Hold = ""
		if c.why == "held" {
			role.Hold = now.Add(time.Hour).Format(time.RFC3339)
		}
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
		{`{"cache-succession":true,"cache-idle-budget":"5m"}`, Policy{Present: true, Enabled: true, Threshold: 30, Budget: 5 * time.Minute}},
		{`{"cache-idle-budget":"soon"}`, Policy{Present: true, Threshold: 30}},
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

func TestFermataMessageNamesTheBrokenCache(t *testing.T) {
	warm := fermataMessage(Verdict{Ping: true, Why: "expiring", Left: 30 * time.Second}, 31.6)
	if !strings.Contains(warm, "expires in about 30 s") || !strings.Contains(warm, "32%") {
		t.Errorf("expiring: %q", warm)
	}
	cold := fermataMessage(Verdict{Ping: true, Why: "expired-queued", Left: -time.Minute}, 40)
	if strings.Contains(cold, "expires in") || !strings.Contains(cold, "has expired") {
		t.Errorf("expired: %q", cold)
	}
}

type stubGauge map[string]Reading

func (g stubGauge) Refresh() error { return nil }
func (g stubGauge) Read(aria string) (Reading, error) {
	r, ok := g[aria]
	if !ok {
		return Reading{}, os.ErrNotExist
	}
	return r, nil
}

func TestOnePingPerHolderAcrossRoles(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	on := Policy{Present: true, Enabled: true, Threshold: 30}
	var sent []string
	f := &fermata{
		gauge: stubGauge{"h": {Context: 40, TTL: 5 * time.Minute, Born: now.Add(-3 * time.Hour), LastTrip: now.Add(-270 * time.Second)}},
		send:  func(aria, msg string) error { sent = append(sent, aria); return nil },
		cfg:   fermataConfig{Dwell: 2 * time.Hour, Lead: time.Minute},
		roles: map[string]Role{"@a": {Form: "@a", Holder: "h", Policy: on}, "@b": {Form: "@b", Holder: "h", Policy: on}},
		seats: map[string]Seat{},
		said:  map[string]string{},
	}
	f.log, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	f.judge(now)
	f.judge(now.Add(20 * time.Second))
	if len(sent) != 1 {
		t.Fatalf("sent %v, want one ping to h", sent)
	}
	for form, s := range f.seats {
		if s.Pinged.IsZero() {
			t.Errorf("%s: seat not marked told", form)
		}
	}
}

func TestHeld(t *testing.T) {
	now := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	for hold, want := range map[string]bool{
		"": false, "true": true, "TRUE": true, "false": false, "junk": false,
		now.Add(time.Minute).Format(time.RFC3339):  true,
		now.Add(-time.Minute).Format(time.RFC3339): false,
	} {
		if got := held(hold, now); got != want {
			t.Errorf("%q: got %v, want %v", hold, got, want)
		}
	}
}

func TestDecideWarmCacheClock(t *testing.T) {
	now := time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)
	cfg := fermataConfig{Dwell: 2 * time.Hour, Lead: 2 * time.Minute}
	seated := Seat{Holder: "h", Since: now.Add(-3 * time.Hour)}
	on := Policy{Present: true, Enabled: true, Threshold: 30}
	budget := on
	budget.Budget = 5 * time.Minute
	base := Reading{Context: 40, TTL: 5 * time.Minute}
	at := func(clock string, ago time.Duration, fenced bool) Reading {
		r := base
		r.Clock, r.LastTrip, r.Fenced = clock, now.Add(-ago), fenced
		return r
	}
	cases := []struct {
		name string
		pol  Policy
		read Reading
		ping bool
		why  string
	}{
		{"messages inside the two-minute lead", on, at("messages", 3*time.Minute+time.Second, false), true, "expiring"},
		{"messages just outside the lead", on, at("messages", 3*time.Minute-time.Second, false), false, "warm"},
		{"responses without a budget", on, at("responses", 4*time.Minute, false), false, "retention-unknown"},
		{"responses with a budget, in the lead", budget, at("responses", 4*time.Minute, false), true, "expiring"},
		{"responses with a budget, warm", budget, at("responses", time.Minute, false), false, "warm"},
		{"budget overrides an hour TTL", budget, func() Reading { r := at("messages", 4*time.Minute, false); r.TTL = time.Hour; return r }(), true, "expiring"},
		{"route changed since the last request", budget, at("responses", 4*time.Minute, true), false, "clock-unknown"},
		{"caching off beats a budget", budget, func() Reading { r := at("messages", 4*time.Minute, false); r.TTL = 0; return r }(), false, "cache-off"},
		{"a long queue past the budget", budget, func() Reading { r := at("responses", 6*time.Minute, false); r.Queued = 1; return r }(), true, "expired-queued"},
	}
	for _, c := range cases {
		v := decide(now, Role{Holder: "h", Policy: c.pol}, seated, c.read, cfg)
		if v.Ping != c.ping || v.Why != c.why {
			t.Errorf("%s: got ping=%v why=%s, want ping=%v why=%s", c.name, v.Ping, v.Why, c.ping, c.why)
		}
	}
	msg := fermataMessage(Verdict{Ping: true, Left: 100 * time.Second, Clock: "responses"}, 35)
	if !strings.Contains(msg, "budget ends in about 100 s") || strings.Contains(msg, "expires") {
		t.Errorf("responses message claims a provider expiry: %q", msg)
	}
}

func TestTripLogReadsResponsesStreams(t *testing.T) {
	attr := func(k string, v any) map[string]any { return map[string]any{"Key": k, "Value": map[string]any{"Value": v}} }
	row := func(ts, method, url string, extra ...map[string]any) string {
		a := append([]any{attr("aria", "a"), attr("method", method), attr("url", url), attr("duration_ms", 1000)}, toAny(extra)...)
		b, _ := json.Marshal(map[string]any{"Timestamp": ts, "Body": map[string]any{"Value": "provider round-trip"}, "Attributes": a})
		return string(b) + "\n"
	}
	cases := []struct {
		name, line string
		want       string
	}{
		{"completed SSE", row("2026-11-16T21:00:10Z", "SSE", "https://x/v1/responses", attr("stream_status", "completed")), "responses"},
		{"ok WS", row("2026-11-16T21:00:10Z", "WS", "wss://x/v1/responses?model=m", attr("stream_status", "ok")), "responses"},
		{"failed stream", row("2026-11-16T21:00:10Z", "SSE", "https://x/v1/responses", attr("stream_status", "error"), attr("stream_err_class", "rate_limit_exceeded")), ""},
		{"ok status with an error class", row("2026-11-16T21:00:10Z", "SSE", "https://x/v1/responses", attr("stream_status", "ok"), attr("stream_err_class", "server")), ""},
		{"Responses HTTP header record", row("2026-11-16T21:00:10Z", "POST", "https://x/v1/responses", attr("status", 200)), ""},
		{"rate-limited Messages", row("2026-11-16T21:00:10Z", "POST", "https://x/v1/messages", attr("status", 429)), ""},
		{"model list", row("2026-11-16T21:00:10Z", "GET", "https://x/v1/models", attr("status", 200)), ""},
	}
	for _, c := range cases {
		tl := newTripLog("")
		tl.take([]byte(c.line))
		if tl.kind["a"] != c.want {
			t.Errorf("%s: got kind %q, want %q", c.name, tl.kind["a"], c.want)
		}
		if c.want != "" && !tl.last["a"].Equal(time.Date(2026, 11, 16, 21, 0, 9, 0, time.UTC)) {
			t.Errorf("%s: clock %v, want the request start", c.name, tl.last["a"])
		}
	}
}

func toAny(m []map[string]any) []any {
	out := make([]any, len(m))
	for i, v := range m {
		out[i] = v
	}
	return out
}

func TestGaugeFencesRouteChangesAndBoot(t *testing.T) {
	g := &figaroGauge{trips: newTripLog(""), routeAt: map[string]string{}}
	g.rows = map[string]ariaRow{"a": {ID: "a", Provider: "chatgpt", Model: "gpt"}}
	g.trips.last["a"] = time.Now()
	g.trips.kind["a"] = "responses"
	g.fig = "/bin/false"
	if r, _ := g.Read("a"); !r.Fenced {
		t.Fatal("a clock read at boot was trusted")
	}
	g.routeAt["a"] = "chatgpt/gpt"
	if r, _ := g.Read("a"); r.Fenced {
		t.Fatal("a clock on the same route was fenced")
	}
	g.rows["a"] = ariaRow{ID: "a", Provider: "anthropic", Model: "opus"}
	if r, _ := g.Read("a"); !r.Fenced {
		t.Fatal("a clock from the old route was carried over")
	}
}

func TestHolderReresolvedBeforeSending(t *testing.T) {
	now := time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)
	on := Policy{Present: true, Enabled: true, Threshold: 30}
	var sent []string
	f := &fermata{
		gauge:   stubGauge{"h": {Context: 40, TTL: 5 * time.Minute, Born: now.Add(-3 * time.Hour), LastTrip: now.Add(-270 * time.Second)}},
		send:    func(aria, msg string) error { sent = append(sent, aria); return nil },
		recheck: func(string) (Role, bool) { return Role{Holder: "successor"}, true },
		cfg:     fermataConfig{Dwell: 2 * time.Hour, Lead: time.Minute},
		roles:   map[string]Role{"@a": {Form: "@a", Holder: "h", Policy: on}},
		seats:   map[string]Seat{},
		said:    map[string]string{},
	}
	f.log, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	f.judge(now)
	if len(sent) != 0 {
		t.Fatalf("pinged a holder no longer seated: %v", sent)
	}
}

