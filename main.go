// Command figla arms fire-and-forget reminders that message a figaro aria.
//
// Figaro sings "Figaro là" — over there. A reminder is figaro calling to
// himself from a little further along in time.
//
// WHY THIS EXISTS. An agent supervising background work has two bad habits:
// polling in a loop (which burns turns and context) and spawning ad-hoc
// `sleep && …` shells (which orphan, leak memory and cannot be listed). figla
// replaces both. Arm a reminder, forget it, get a message if the work is late,
// cancel it if the work reports first.
//
// PORTABILITY. Uses transient systemd user timers when a user bus is actually
// reachable, and a detached child process everywhere else — Windows, macOS,
// containers, and Linux boxes whose user bus is missing. The state directory is
// the registry either way, so `list` and `cancel` behave identically.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const usage = `figla — fire-and-forget reminders for a figaro aria

USAGE
  figla arm --aria <id> --in <dur> --about <text> [--watch <cmd>] [--name <n>]
  figla list [--aria <id>] [--json]
  figla cancel <name> | --aria <id> --all
  figla sweep
  figla version

ARM
  --aria   <id>    aria to message when it fires (required; reminders are
                   scoped to an aria and every other verb filters by it)
  --in     <dur>   Go duration: 90s, 25m, 1h30m
  --about  <text>  what you are waiting on — written to be actionable later
  --watch  <cmd>   optional shell command run AT FIRE TIME; its output is
                   included, so the reminder carries state instead of asking
                   you to go find it. Bounded: 10s, 2KB.
  --name   <n>     stable handle (default: derived from --about)

EXAMPLES
  figla arm --aria 86296c96 --in 25m --about "FX: footer + exit hang" \
        --watch "git -C /home/gluck/dev/figaro-qua/fx log --oneline -1"
  figla list --aria 86296c96
  figla cancel --aria 86296c96 --all
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "arm":
		err = cmdArm(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "cancel", "rm":
		err = cmdCancel(os.Args[2:])
	case "sweep":
		err = cmdSweep()
	case "fire": // internal: deliver now
		err = cmdFire(os.Args[2:])
	case "wait": // internal: sleep then deliver (detach backend)
		err = cmdWait(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("figla 0.1.0")
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "figla: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "figla: "+err.Error())
		os.Exit(1)
	}
}

// flags is a tiny parser: figla's surface is small enough that the stdlib
// flag package's per-subcommand ceremony costs more than it saves.
func flags(args []string) (map[string]string, []string) {
	out, rest := map[string]string{}, []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			rest = append(rest, a)
			continue
		}
		k := strings.TrimPrefix(a, "--")
		if j := strings.Index(k, "="); j >= 0 {
			out[k[:j]] = k[j+1:]
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[k] = args[i+1]
			i++
			continue
		}
		out[k] = "true"
	}
	return out, rest
}

func cmdArm(args []string) error {
	f, _ := flags(args)
	aria, about, in := f["aria"], f["about"], f["in"]
	if aria == "" || in == "" || about == "" {
		return errors.New("arm needs --aria, --in and --about")
	}
	d, err := time.ParseDuration(in)
	if err != nil {
		return fmt.Errorf("bad --in %q: %w", in, err)
	}
	if d <= 0 {
		return fmt.Errorf("--in must be positive, got %s", d)
	}
	name := f["name"]
	if name == "" {
		name = slug(about)
	}
	// Resolve figaro NOW, not at fire time. A detached child or a systemd unit
	// inherits a minimal environment, and "figaro: not found" an hour later is
	// a silent failure of the exact thing meant to catch silent failures.
	fig, err := exec.LookPath("figaro")
	if err != nil {
		return fmt.Errorf("cannot find figaro on PATH: %w", err)
	}
	r := &Reminder{
		Name: name, Aria: aria, About: about, Watch: f["watch"],
		Created: time.Now(), FireAt: time.Now().Add(d), Figaro: fig,
	}
	if err := arm(r); err != nil {
		return err
	}
	if err := r.save(); err != nil {
		return err
	}
	fmt.Printf("armed %s/%s via %s, fires in %s (%s)\n",
		aria, name, r.Backend, d, r.FireAt.Format("15:04:05"))
	return nil
}

func cmdList(args []string) error {
	f, _ := flags(args)
	rs, err := load(f["aria"])
	if err != nil {
		return err
	}
	// Sweeping on read keeps the registry honest without a background process:
	// a reminder whose carrier is gone is not pending, it is dead.
	live := rs[:0]
	for _, r := range rs {
		if !alive(r) {
			_ = r.remove()
			continue
		}
		live = append(live, r)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].FireAt.Before(live[j].FireAt) })
	if f["json"] == "true" {
		return json.NewEncoder(os.Stdout).Encode(live)
	}
	if len(live) == 0 {
		fmt.Println("no active reminders")
		return nil
	}
	fmt.Printf("%-10s %-22s %-9s %-8s %s\n", "ARIA", "NAME", "IN", "VIA", "ABOUT")
	for _, r := range live {
		fmt.Printf("%-10s %-22s %-9s %-8s %s\n",
			r.Aria, trunc(r.Name, 22), until(r.FireAt), r.Backend, trunc(r.About, 40))
	}
	return nil
}

func cmdCancel(args []string) error {
	f, rest := flags(args)
	if f["all"] == "true" {
		if f["aria"] == "" {
			return errors.New("cancel --all needs --aria (reminders are aria-scoped)")
		}
		rs, err := load(f["aria"])
		if err != nil {
			return err
		}
		for _, r := range rs {
			disarm(r)
			_ = r.remove()
		}
		fmt.Printf("cancelled %d reminder(s) for %s\n", len(rs), f["aria"])
		return nil
	}
	if len(rest) == 0 {
		return errors.New("cancel needs a name, or --aria <id> --all")
	}
	rs, err := load(f["aria"])
	if err != nil {
		return err
	}
	n := 0
	for _, r := range rs {
		if r.Name != rest[0] {
			continue
		}
		disarm(r)
		_ = r.remove()
		n++
	}
	if n == 0 {
		return fmt.Errorf("no reminder named %q", rest[0])
	}
	fmt.Printf("cancelled %s\n", rest[0])
	return nil
}

func cmdSweep() error {
	rs, err := load("")
	if err != nil {
		return err
	}
	n := 0
	for _, r := range rs {
		if !alive(r) {
			_ = r.remove()
			n++
		}
	}
	fmt.Printf("swept %d dead reminder(s)\n", n)
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func until(t time.Time) string {
	d := time.Until(t).Round(time.Second)
	if d < 0 {
		return "due"
	}
	return d.String()
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == ':':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 24 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
