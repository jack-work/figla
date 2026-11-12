package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func movedPath(aria, name string) string {
	return filepath.Join(ariaDir(aria), safe(name)+".moved")
}

func findMoved(aria, name string) (*Reminder, error) {
	var hops []string
	for range 8 {
		r, err := find(aria, name)
		if err == nil {
			for _, h := range hops {
				_ = os.Remove(h)
			}
			return r, nil
		}
		hop := movedPath(aria, name)
		b, e := os.ReadFile(hop)
		if e != nil {
			return nil, err
		}
		hops = append(hops, hop)
		aria, name, _ = strings.Cut(strings.TrimSpace(string(b)), " ")
	}
	return nil, fmt.Errorf("reminder %s/%s moved too many times", aria, name)
}

func cmdMove(args []string) error {
	f, rest := flags(args)
	if f["from"] == "" || f["to"] == "" || len(rest) > 0 {
		return errors.New("move needs --from <aria> and --to <aria>")
	}
	from, err := canonicalAria(f["from"])
	if err != nil {
		return err
	}
	to, err := canonicalAria(f["to"])
	if err != nil {
		return err
	}
	if from == to {
		return errors.New("move: --from and --to are the same aria")
	}
	rs, err := load(from)
	if err != nil {
		return err
	}
	held, err := load(to)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, r := range held {
		taken[r.Name] = true
	}
	if len(rs) == 0 {
		fmt.Printf("%s holds no reminders\n", from)
		return nil
	}
	var failed []string
	for _, r := range rs {
		name := r.Name
		if taken[name] {
			name += "-" + trunc(from, 8)
		}
		taken[name] = true
		if err := moveOne(r, to, name); err != nil {
			failed = append(failed, r.Name+": "+err.Error())
			continue
		}
		fmt.Printf("moved %s/%s -> %s/%s\n", from, r.Name, to, name)
	}
	if len(failed) > 0 {
		return errors.New("move: " + strings.Join(failed, "; "))
	}
	return nil
}

func moveOne(r *Reminder, to, name string) error {
	m := *r
	m.Aria, m.Name = to, name
	if r.Backend == "run" {
		if err := os.MkdirAll(ariaDir(r.Aria), 0o755); err != nil {
			return err
		}
		if err := m.save(); err != nil {
			return err
		}
		if err := os.WriteFile(movedPath(r.Aria, r.Name), []byte(to+" "+name+"\n"), 0o644); err != nil {
			_ = m.remove()
			return err
		}
		return r.remove()
	}
	m.Unit, m.PID = "", 0
	if err := m.save(); err != nil {
		return err
	}
	disarm(r)
	_ = r.remove()
	if err := arm(&m); err != nil {
		return fmt.Errorf("moved but not re-armed (catchup will deliver it): %w", err)
	}
	return m.save()
}
