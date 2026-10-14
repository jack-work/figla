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

const version = "0.2.1"

const usage = `figla — fire-and-forget reminders for a figaro aria

USAGE
  figla arm --aria <id> (--in <dur> | --at <time>) --about <text> [--watch <cmd>] [--name <n>]
  figla list [--aria <id>] [--json]
  figla cancel <name> | --aria <id> --all
  figla catchup
  figla version

ARM
  --aria   <id>    aria to message when it fires (required; reminders are
                   scoped to an aria and every other verb filters by it)
  --in     <dur>   Go duration: 90s, 25m, 1h30m
  --at     <time>  wall-clock deadline, local time: "2026-10-18 08:55",
                   "2026-10-18 08:55:30", or RFC 3339
  --about  <text>  what you are waiting on — written to be actionable later
  --watch  <cmd>   optional shell command run AT FIRE TIME; its output is
                   included, so the reminder carries state instead of asking
                   you to go find it. Bounded: 10s, 2KB.
  --name   <n>     stable handle (default: derived from --about)

DURABILITY
  With a reachable systemd user manager, each reminder is a calendar timer
  in ~/.config/systemd/user, anchored to wall-clock time. It survives
  reboots, daemon-reloads and suspend. figla-catchup.timer runs a minute
  after boot and every ten minutes, delivering anything that came due while
  its carrier was gone and retrying any failed send. Reminders delivered
  late say by how much.

CATCHUP
  Deliver every reminder more than 30s overdue. Run automatically by
  figla-catchup.timer; safe to run by hand.

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
	case "catchup", "sweep":
		err = cmdCatchup()
	case "fire": // internal: deliver now
		err = cmdFire(os.Args[2:])
	case "wait": // internal: sleep then deliver (detach backend)
		err = cmdWait(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("figla " + version)
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
	aria, about := f["aria"], f["about"]
	if aria == "" || about == "" || (f["in"] == "") == (f["at"] == "") {
		return errors.New("arm needs --aria, --about, and exactly one of --in or --at")
	}
	fireAt, err := deadline(f["in"], f["at"], time.Now())
	if err != nil {
		return err
	}
	name := f["name"]
	if name == "" {
		name = slug(about)
	}
	fig, err := exec.LookPath("figaro")
	if err != nil {
		return fmt.Errorf("cannot find figaro on PATH: %w", err)
	}
	if old, err := find(aria, name); err == nil {
		disarm(old)
		_ = old.remove()
	}
	r := &Reminder{
		Name: name, Aria: aria, About: about, Watch: f["watch"],
		Created: time.Now(), FireAt: fireAt, Figaro: fig,
	}
	if err := r.save(); err != nil {
		return err
	}
	if err := arm(r); err != nil {
		_ = r.remove()
		return err
	}
	if err := r.save(); err != nil {
		disarm(r)
		_ = r.remove()
		return err
	}
	fmt.Printf("armed %s/%s via %s, fires %s (in %s)\n",
		aria, name, r.Backend, r.FireAt.Format("Mon 15:04:05 MST"), time.Until(r.FireAt).Round(time.Second))
	return nil
}

func deadline(in, at string, now time.Time) (time.Time, error) {
	if in != "" {
		d, err := time.ParseDuration(in)
		if err != nil {
			return time.Time{}, fmt.Errorf("bad --in %q: %w", in, err)
		}
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--in must be positive, got %s", d)
		}
		return now.Add(d).Round(0), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, at, time.Local); err == nil {
			return future(t, now)
		}
	}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return future(t, now)
	}
	return time.Time{}, fmt.Errorf("bad --at %q: want \"2006-01-02 15:04\" or RFC 3339", at)
}

func future(t, now time.Time) (time.Time, error) {
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("--at %s is not in the future", t.Format(time.RFC3339))
	}
	return t, nil
}

func cmdList(args []string) error {
	f, _ := flags(args)
	rs, err := load(f["aria"])
	if err != nil {
		return err
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].FireAt.Before(rs[j].FireAt) })
	if f["json"] == "true" {
		if rs == nil {
			rs = []*Reminder{}
		}
		return json.NewEncoder(os.Stdout).Encode(rs)
	}
	if len(rs) == 0 {
		fmt.Println("no active reminders")
		return nil
	}
	fmt.Printf("%-10s %-22s %-11s %-8s %s\n", "ARIA", "NAME", "IN", "VIA", "ABOUT")
	for _, r := range rs {
		via := r.Backend
		if !pending(r) {
			via += "!"
		}
		fmt.Printf("%-10s %-22s %-11s %-8s %s\n",
			r.Aria, trunc(r.Name, 22), until(r.FireAt), via, trunc(r.About, 40))
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

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func until(t time.Time) string {
	d := time.Until(t).Round(time.Second)
	if d < 0 {
		return "overdue"
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
