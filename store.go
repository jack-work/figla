package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Reminder is one armed reminder. It is also the on-disk registry record:
// there is no daemon, so the state directory IS the source of truth, and both
// backends write the same shape so `list` and `cancel` do not care which armed
// it.
type Reminder struct {
	Name    string    `json:"name"`
	Aria    string    `json:"aria"`
	About   string    `json:"about"`
	Watch   string    `json:"watch,omitempty"`
	Created time.Time `json:"created"`
	FireAt  time.Time `json:"fire_at"`

	// Figaro is resolved at ARM time. A detached child inherits a minimal
	// environment and a systemd unit gets almost none, so looking it up later
	// is how a reminder silently fails to remind.
	Figaro string `json:"figaro"`

	Backend string `json:"backend"`        // "systemd" | "detach"
	Unit    string `json:"unit,omitempty"` // systemd transient unit
	PID     int    `json:"pid,omitempty"`  // detached child
}

// stateDir is the registry root. FIGLA_STATE overrides it, which is what the
// tests use — no test may touch a real user's reminders.
func stateDir() string {
	if d := os.Getenv("FIGLA_STATE"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "figla")
		}
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "figla")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "figla")
}

// ariaDir scopes reminders by aria. The user asked for this explicitly and it
// is what makes `cancel --aria X --all` safe: a phase can be reaped without
// touching another aria's pending work.
func ariaDir(aria string) string { return filepath.Join(stateDir(), safe(aria)) }

func (r *Reminder) path() string {
	return filepath.Join(ariaDir(r.Aria), safe(r.Name)+".json")
}

func (r *Reminder) save() error {
	if err := os.MkdirAll(filepath.Dir(r.path()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path(), b, 0o644)
}

func (r *Reminder) remove() error { return os.Remove(r.path()) }

// load reads every reminder, or only one aria's when aria != "".
func load(aria string) ([]*Reminder, error) {
	root := stateDir()
	if aria != "" {
		root = ariaDir(aria)
	}
	var out []*Reminder
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a missing registry is an empty one, not a failure
		}
		if d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var r Reminder
		if json.Unmarshal(b, &r) != nil {
			return nil
		}
		out = append(out, &r)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

// safe keeps a name from escaping the registry directory. Aria ids and slugs
// are already tame, but a path separator here would write anywhere.
func safe(s string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", "..", "_", ":", "_")
	s = r.Replace(s)
	if s == "" {
		return "unnamed"
	}
	return s
}

// message is what lands in the aria. It carries state rather than a nudge —
// the whole point of --watch — and it tells the agent to use figla instead of
// falling back to the habits this tool replaces.
func (r *Reminder) message(watchOut string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REMINDER (figla) — %s\n", r.About)
	fmt.Fprintf(&b, "armed %s ago; due now.\n", time.Since(r.Created).Round(time.Second))
	if strings.TrimSpace(watchOut) != "" {
		fmt.Fprintf(&b, "\n--watch %s\n%s\n", r.Watch, strings.TrimRight(watchOut, "\n"))
	}
	b.WriteString("\nThis reminder has fired and is gone; nothing to cancel. ")
	b.WriteString("If the work is still running, arm another with `figla arm` ")
	b.WriteString("rather than polling in a loop or spawning a sleep shell — ")
	b.WriteString("that is what figla is for.")
	return b.String()
}
