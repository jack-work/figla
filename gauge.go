package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Reading struct {
	Context  float64
	Busy     bool
	Born     time.Time
	LastTrip time.Time
	TTL      time.Duration
	Queued   int
	Provider string
	Route    string
	Clock    string
	Fenced   bool
}

type Gauge interface {
	Refresh() error
	Read(aria string) (Reading, error)
}

func cacheTTL(setting string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "none", "off", "false", "disabled":
		return 0
	case "long", "1h", "60m":
		return time.Hour
	}
	return 5 * time.Minute
}

type tripLog struct {
	path  string
	off   int64
	last  map[string]time.Time
	kind  map[string]string
	fresh map[string]bool
	read  bool
}

func newTripLog(path string) *tripLog {
	return &tripLog{path: path, last: map[string]time.Time{}, kind: map[string]string{}, fresh: map[string]bool{}}
}

func figaroLogPath() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "figaro", "logs.jsonl")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "figaro", "logs.jsonl")
}

func (t *tripLog) Refresh() error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < t.off {
		t.off = 0
	}
	booting := !t.read
	t.read = true
	clear(t.fresh)
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil
		}
		t.off += int64(len(line))
		if aria := t.take(line); aria != "" && !booting {
			t.fresh[aria] = true
		}
	}
}

var roundTripMark = []byte(`"provider round-trip"`)

func tripKind(method, url string, status int64, stream, errClass string) string {
	switch {
	case method == "POST" && strings.HasSuffix(url, "/messages") && status >= 200 && status < 300:
		return "messages"
	case (method == "SSE" || method == "WS") && strings.HasSuffix(url, "/responses") &&
		(stream == "ok" || stream == "completed") && errClass == "":
		return "responses"
	}
	return ""
}

func (t *tripLog) take(line []byte) string {
	if !bytes.Contains(line, roundTripMark) {
		return ""
	}
	var row struct {
		Timestamp  time.Time
		Attributes []struct {
			Key   string
			Value struct{ Value json.RawMessage }
		}
	}
	if json.Unmarshal(line, &row) != nil {
		return ""
	}
	var aria, method, url, stream, errClass string
	var dur, status int64
	for _, a := range row.Attributes {
		switch a.Key {
		case "aria":
			_ = json.Unmarshal(a.Value.Value, &aria)
		case "method":
			_ = json.Unmarshal(a.Value.Value, &method)
		case "url":
			_ = json.Unmarshal(a.Value.Value, &url)
		case "duration_ms":
			_ = json.Unmarshal(a.Value.Value, &dur)
		case "status":
			_ = json.Unmarshal(a.Value.Value, &status)
		case "stream_status":
			_ = json.Unmarshal(a.Value.Value, &stream)
		case "stream_err_class":
			_ = json.Unmarshal(a.Value.Value, &errClass)
		}
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url = url[:i]
	}
	kind := tripKind(method, url, status, stream, errClass)
	if aria == "" || kind == "" {
		return ""
	}
	at := row.Timestamp.Add(-time.Duration(dur) * time.Millisecond)
	if at.After(t.last[aria]) {
		t.last[aria] = at
		t.kind[aria] = kind
	}
	return aria
}

type ariaRow struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Provider     string `json:"provider"`
	CreatedAt    int64  `json:"created_at"`
	ContextTok   int64  `json:"context_tokens"`
	ContextLimit *int64 `json:"context_limit"`
	Model        string `json:"model"`
}

type figaroGauge struct {
	fig     string
	trips   *tripLog
	routeAt map[string]string
	rows    map[string]ariaRow
	forms   []string
}

func newFigaroGauge(fig, logPath string) *figaroGauge {
	return &figaroGauge{fig: fig, trips: newTripLog(logPath), routeAt: map[string]string{}}
}

func (g *figaroGauge) Refresh() error {
	out, err := figaroJSON(g.fig, "list", "-j")
	if err != nil {
		return err
	}
	var rows []ariaRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return err
	}
	g.rows = make(map[string]ariaRow, len(rows))
	g.forms = g.forms[:0]
	for _, r := range rows {
		g.rows[r.ID] = r
		if r.State == "form" {
			g.forms = append(g.forms, r.ID)
		}
	}
	if err := g.trips.Refresh(); err != nil {
		return err
	}
	for aria := range g.trips.fresh {
		if r, ok := g.rows[aria]; ok {
			g.routeAt[aria] = route(r)
		}
	}
	return nil
}

func route(r ariaRow) string { return r.Provider + "/" + r.Model }

func (g *figaroGauge) row(aria string) (ariaRow, bool) {
	if r, ok := g.rows[aria]; ok {
		return r, true
	}
	for id, r := range g.rows {
		if strings.HasPrefix(id, aria) || strings.HasPrefix(aria, id) {
			return r, true
		}
	}
	return ariaRow{}, false
}

func (g *figaroGauge) Read(aria string) (Reading, error) {
	r, ok := g.row(aria)
	if !ok {
		return Reading{}, os.ErrNotExist
	}
	rd := Reading{
		Context:  -1,
		Busy:     r.State == "active",
		Born:     time.UnixMilli(r.CreatedAt),
		LastTrip: g.trips.last[r.ID],
		TTL:      5 * time.Minute,
		Provider: r.Provider,
		Route:    route(r),
		Clock:    g.trips.kind[r.ID],
	}
	if !rd.LastTrip.IsZero() && g.routeAt[r.ID] != rd.Route {
		rd.Fenced = true
	}
	if r.ContextLimit != nil && *r.ContextLimit > 0 {
		rd.Context = 100 * float64(r.ContextTok) / float64(*r.ContextLimit)
	}
	var cc struct {
		CacheControl *string `json:"system.cache_control"`
	}
	if out, _ := figaroJSON(g.fig, "get", "--id", r.ID, "-j", "system.cache_control"); json.Unmarshal(out, &cc) == nil && cc.CacheControl != nil {
		rd.TTL = cacheTTL(*cc.CacheControl)
	}
	var q struct {
		Queue []json.RawMessage `json:"queue"`
	}
	if out, err := figaroJSON(g.fig, "queue", "--id", r.ID, "-j"); err == nil && json.Unmarshal(out, &q) == nil {
		rd.Queued = len(q.Queue)
	}
	return rd, nil
}
