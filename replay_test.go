package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestReplayFrozenGaps(t *testing.T) {
	in, out := os.Getenv("FERMATA_REPLAY_IN"), os.Getenv("FERMATA_REPLAY_OUT")
	if in == "" || out == "" {
		t.Skip("FERMATA_REPLAY_IN/OUT unset")
	}
	budget, _ := time.ParseDuration(os.Getenv("FERMATA_REPLAY_RESPONSES_BUDGET"))
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, k := range recs[0] {
		col[k] = i
	}
	ns := func(r []string, k string) time.Time {
		v, _ := strconv.ParseInt(r[col[k]], 10, 64)
		return time.Unix(0, v)
	}
	cfg := fermataConfig{Dwell: 2 * time.Hour, Lead: 120 * time.Second}
	const every = 20 * time.Second
	o, _ := os.Create(out)
	defer o.Close()
	w := csv.NewWriter(o)
	w.Write([]string{"i", "ping", "why", "at_ns", "left_s", "window_s", "last_why"})
	for i, r := range recs[1:] {
		ctx, _ := strconv.ParseFloat(r[col["ctx"]], 64)
		kind := r[col["kind"]]
		p := Policy{Present: true, Enabled: true, Threshold: 30}
		if kind == "responses" {
			p.Budget = budget
		}
		g := Reading{Context: ctx, Born: ns(r, "born_ns"), TTL: 5 * time.Minute, Fenced: r[col["routechg"]] == "1"}
		if kind != "none" {
			g.LastTrip, g.Clock = ns(r, "trip_ns"), kind
		}
		role := Role{Form: "r", Holder: r[col["seat"]], Policy: p}
		seat := Seat{Holder: role.Holder, Since: g.Born}
		end, next := ns(r, "end_ns"), ns(r, "next_ns")
		tick := end.Truncate(every)
		if tick.Before(end) {
			tick = tick.Add(every)
		}
		row := []string{strconv.Itoa(i), "0", "", "", "", "", ""}
		last := "returned-before-first-tick"
		for ; tick.Before(next); tick = tick.Add(every) {
			v := decide(tick, role, seat, g, cfg)
			last = v.Why
			if v.Ping {
				row = []string{strconv.Itoa(i), "1", v.Why, strconv.FormatInt(tick.UnixNano(), 10), fmt.Sprintf("%.1f", v.Left.Seconds()), fmt.Sprintf("%.0f", v.Window.Seconds()), v.Why}
				break
			}
		}
		if row[1] == "0" {
			row[6] = last
		}
		w.Write(row)
	}
	w.Flush()
}
