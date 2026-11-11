package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Policy struct {
	Present   bool
	Enabled   bool
	Threshold float64
}

type Role struct {
	Form   string
	Holder string
	Policy Policy
}

type Seat struct {
	Holder string    `json:"holder"`
	Since  time.Time `json:"since"`
	Pinged time.Time `json:"pinged,omitzero"`
}

type fermataConfig struct {
	Dwell time.Duration
	Lead  time.Duration
}

type Verdict struct {
	Ping bool
	Why  string
	Left time.Duration
}

func decide(now time.Time, r Role, s Seat, g Reading, c fermataConfig) Verdict {
	if !s.Pinged.IsZero() {
		return Verdict{Why: "pinged"}
	}
	if now.Sub(s.Since) < c.Dwell {
		return Verdict{Why: "dwell"}
	}
	if g.TTL == 0 {
		return Verdict{Why: "cache-off"}
	}
	if g.Context < 0 {
		return Verdict{Why: "context-unknown"}
	}
	if g.Context < r.Policy.Threshold {
		return Verdict{Why: "below-threshold"}
	}
	if g.LastTrip.IsZero() {
		return Verdict{Why: "no-trip"}
	}
	left := g.LastTrip.Add(g.TTL).Sub(now)
	switch {
	case left > 0 && g.Busy:
		return Verdict{Why: "busy", Left: left}
	case left > c.Lead:
		return Verdict{Why: "warm", Left: left}
	case left > 0:
		return Verdict{Ping: true, Why: "expiring", Left: left}
	case g.Queued > 0:
		return Verdict{Ping: true, Why: "expired-queued", Left: left}
	}
	return Verdict{Why: "expired", Left: left}
}

func fermataMessage(ctx float64) string {
	return fmt.Sprintf("fermata: your prompt cache expires in <60 s and you are at %d%%; succeed now per your succession policy", int(math.Round(ctx)))
}

func parsePolicy(raw json.RawMessage) Policy {
	if len(raw) == 0 || string(raw) == "null" {
		return Policy{Threshold: 30}
	}
	var sp struct {
		Cache     json.RawMessage `json:"cache-succession"`
		Threshold json.RawMessage `json:"cache-threshold"`
	}
	_ = json.Unmarshal(raw, &sp)
	p := Policy{Present: true, Threshold: 30}
	var b bool
	var s string
	if json.Unmarshal(sp.Cache, &b) == nil {
		p.Enabled = b
	} else if json.Unmarshal(sp.Cache, &s) == nil {
		p.Enabled = strings.EqualFold(strings.TrimSpace(s), "true")
	}
	var n float64
	if json.Unmarshal(sp.Threshold, &n) == nil {
		p.Threshold = n
	} else if json.Unmarshal(sp.Threshold, &s) == nil {
		if v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64); err == nil {
			p.Threshold = v
		}
	}
	return p
}

func figaroJSON(fig string, args ...string) ([]byte, error) {
	out, err := exec.Command(fig, args...).Output()
	if err != nil && len(out) > 0 && json.Valid(out) {
		return out, nil
	}
	return out, err
}

type fermata struct {
	fig     string
	gauge   *figaroGauge
	cfg     fermataConfig
	dry     bool
	rescan  time.Duration
	scanned time.Time
	known   map[string]bool
	roles   map[string]Role
	seats   map[string]Seat
	said    map[string]string
	seatsAt string
	log     *os.File
}

func (f *fermata) readRole(form string) (Role, bool) {
	out, err := figaroJSON(f.fig, "get", "--id", form, "-j", "target-aria", "succession-policy")
	if err != nil {
		return Role{}, false
	}
	var v struct {
		Target string          `json:"target-aria"`
		Policy json.RawMessage `json:"succession-policy"`
	}
	if json.Unmarshal(out, &v) != nil || v.Target == "" {
		return Role{}, false
	}
	return Role{Form: form, Holder: v.Target, Policy: parsePolicy(v.Policy)}, true
}

func (f *fermata) deriveRoles(now time.Time) {
	full := now.Sub(f.scanned) >= f.rescan
	if full {
		f.scanned = now
		f.known = map[string]bool{}
		f.roles = map[string]Role{}
	}
	for _, form := range f.gauge.forms {
		if f.known[form] {
			continue
		}
		f.known[form] = true
		if r, ok := f.readRole(form); ok && r.Policy.Present {
			f.roles[form] = r
		}
	}
	if full {
		return
	}
	for form := range f.roles {
		if r, ok := f.readRole(form); ok && r.Policy.Present {
			f.roles[form] = r
		} else {
			delete(f.roles, form)
		}
	}
}

func (f *fermata) seat(r Role, g Reading, now time.Time) Seat {
	s, ok := f.seats[r.Form]
	switch {
	case !ok:
		s = Seat{Holder: r.Holder, Since: g.Born}
	case s.Holder != r.Holder:
		s = Seat{Holder: r.Holder, Since: now}
	}
	f.seats[r.Form] = s
	return s
}

func (f *fermata) tick(now time.Time) error {
	if err := f.gauge.Refresh(); err != nil {
		return err
	}
	f.deriveRoles(now)
	for _, r := range f.roles {
		if !r.Policy.Enabled && !f.dry {
			continue
		}
		g, err := f.gauge.Read(r.Holder)
		if err != nil {
			f.record(now, r, Seat{Holder: r.Holder}, g, Verdict{Why: "holder-missing"}, "")
			continue
		}
		s := f.seat(r, g, now)
		v := decide(now, r, s, g, f.cfg)
		sendErr := ""
		if v.Ping {
			sendErr = f.ping(r, g)
			if sendErr == "" {
				s.Pinged = now
				f.seats[r.Form] = s
			}
		}
		f.record(now, r, s, g, v, sendErr)
	}
	for form := range f.seats {
		if _, ok := f.roles[form]; !ok {
			delete(f.seats, form)
		}
	}
	return f.saveSeats()
}

func (f *fermata) ping(r Role, g Reading) string {
	if f.dry {
		return ""
	}
	out, err := exec.Command(f.fig, "send", "-f", "--id", r.Holder, "--", fermataMessage(g.Context)).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func (f *fermata) record(now time.Time, r Role, s Seat, g Reading, v Verdict, sendErr string) {
	if !v.Ping && f.said[r.Form] == r.Holder+" "+v.Why {
		return
	}
	f.said[r.Form] = r.Holder + " " + v.Why
	line := map[string]any{
		"t":       now.Format(time.RFC3339),
		"role":    r.Form,
		"holder":  r.Holder,
		"why":     v.Why,
		"ping":    v.Ping,
		"dry":     f.dry,
		"enabled": r.Policy.Enabled,
		"ctx":     math.Round(g.Context*10) / 10,
		"thresh":  r.Policy.Threshold,
		"ttl_s":   int(g.TTL.Seconds()),
		"busy":    g.Busy,
		"queued":  g.Queued,
		"seat_s":  int(now.Sub(s.Since).Seconds()),
	}
	if !g.LastTrip.IsZero() {
		line["left_s"] = int(g.LastTrip.Add(g.TTL).Sub(now).Seconds())
	}
	if sendErr != "" {
		line["send_err"] = sendErr
	}
	b, _ := json.Marshal(line)
	_, _ = f.log.Write(append(b, '\n'))
}

func (f *fermata) loadSeats() {
	f.seats = map[string]Seat{}
	if b, err := os.ReadFile(f.seatsAt); err == nil {
		_ = json.Unmarshal(b, &f.seats)
	}
}

func (f *fermata) saveSeats() error {
	b, err := json.MarshalIndent(f.seats, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.seatsAt + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.seatsAt)
}

func durFlag(f map[string]string, k string, def time.Duration) (time.Duration, error) {
	if f[k] == "" {
		return def, nil
	}
	d, err := time.ParseDuration(f[k])
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--%s: want a positive duration, got %q", k, f[k])
	}
	return d, nil
}

func cmdFermata(args []string) error {
	fl, rest := flags(args)
	if len(rest) > 0 {
		return fmt.Errorf("fermata: unexpected %q", rest[0])
	}
	every, err := durFlag(fl, "every", 20*time.Second)
	if err != nil {
		return err
	}
	dwell, err := durFlag(fl, "dwell", 2*time.Hour)
	if err != nil {
		return err
	}
	lead, err := durFlag(fl, "lead", 60*time.Second)
	if err != nil {
		return err
	}
	rescan, err := durFlag(fl, "rescan", 5*time.Minute)
	if err != nil {
		return err
	}
	fig, err := exec.LookPath("figaro")
	if err != nil {
		return errors.New("cannot find figaro on PATH")
	}
	figLog := fl["figaro-log"]
	if figLog == "" {
		figLog = figaroLogPath()
	}
	dry := fl["dry-run"] == "true"
	name := "fermata"
	if dry {
		name = "fermata-dry"
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	logPath := fl["log"]
	if logPath == "" {
		logPath = filepath.Join(stateDir(), name+".jsonl")
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	f := &fermata{
		fig:     fig,
		gauge:   newFigaroGauge(fig, figLog),
		cfg:     fermataConfig{Dwell: dwell, Lead: lead},
		dry:     dry,
		rescan:  rescan,
		said:    map[string]string{},
		seatsAt: filepath.Join(stateDir(), name+".json"),
		log:     lf,
	}
	f.loadSeats()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := f.tick(time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "figla fermata: "+err.Error())
		}
		select {
		case <-stop:
			return nil
		case <-t.C:
		}
	}
}
