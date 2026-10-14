package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const lateAfter = 30 * time.Second

func arm(r *Reminder) error {
	if backendAllowed("systemd") && systemdUsable() {
		if err := armSystemd(r); err == nil {
			r.Backend = "systemd"
			return nil
		}
	}
	if !backendAllowed("detach") {
		return errors.New("FIGLA_BACKEND forbids the detach carrier and systemd is unusable")
	}
	if err := armDetached(r); err != nil {
		return err
	}
	r.Backend = "detach"
	return nil
}

func backendAllowed(b string) bool {
	want := os.Getenv("FIGLA_BACKEND")
	return want == "" || want == b
}

func disarm(r *Reminder) {
	switch r.Backend {
	case "systemd":
		_ = exec.Command("systemctl", "--user", "stop", r.Unit+".timer", r.Unit+".service").Run()
		removeUnitFiles(r.Unit)
	case "detach":
		killPID(r.PID)
	}
}

func retire(r *Reminder) {
	if r.Backend != "systemd" || r.Unit == "" {
		return
	}
	_ = exec.Command("systemctl", "--user", "stop", r.Unit+".timer").Run()
	removeUnitFiles(r.Unit)
}

func pending(r *Reminder) bool {
	switch r.Backend {
	case "systemd":
		_, err := os.Stat(filepath.Join(unitDir(), r.Unit+".timer"))
		return err == nil
	case "detach":
		return pidAlive(r.PID)
	}
	return false
}

func systemdUsable() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	out, err := exec.Command("systemctl", "--user", "show", "--property=Version").CombinedOutput()
	return err == nil && strings.Contains(string(out), "Version=")
}

func unitDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "systemd", "user")
}

func unitQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}

func unitText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "%", "%%")
}

func armSystemd(r *Reminder) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := installCatchup(self); err != nil {
		return err
	}
	r.Unit = fmt.Sprintf("figla-%s-%s-%d", unitSafe(r.Aria), unitSafe(r.Name), r.FireAt.Unix())
	dir := unitDir()

	service := strings.Join([]string{
		"[Unit]",
		"Description=figla reminder for aria " + unitText(r.Aria) + ": " + unitText(r.About),
		"",
		"[Service]",
		"Type=oneshot",
		"Environment=" + unitQuote("FIGLA_STATE="+stateDir()),
		"ExecStart=" + strings.Join([]string{unitQuote(self), "fire", "--aria", unitQuote(r.Aria), "--name", unitQuote(r.Name)}, " "),
		"TimeoutStartSec=300",
		"",
	}, "\n")
	timer := strings.Join([]string{
		"[Unit]",
		"Description=figla reminder for aria " + unitText(r.Aria) + ": " + unitText(r.About),
		"",
		"[Timer]",
		"OnCalendar=" + r.FireAt.UTC().Format("2006-01-02 15:04:05") + " UTC",
		"AccuracySec=1s",
		"Unit=" + r.Unit + ".service",
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n")

	if err := os.MkdirAll(filepath.Join(dir, "timers.target.wants"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, r.Unit+".service"), []byte(service), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, r.Unit+".timer"), []byte(timer), 0o644); err != nil {
		removeUnitFiles(r.Unit)
		return err
	}
	link := filepath.Join(dir, "timers.target.wants", r.Unit+".timer")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Join("..", r.Unit+".timer"), link); err != nil {
		removeUnitFiles(r.Unit)
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "start", r.Unit+".timer").CombinedOutput(); err != nil {
		removeUnitFiles(r.Unit)
		return fmt.Errorf("systemctl start: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func removeUnitFiles(unit string) {
	dir := unitDir()
	_ = os.Remove(filepath.Join(dir, "timers.target.wants", unit+".timer"))
	_ = os.Remove(filepath.Join(dir, unit+".timer"))
	_ = os.Remove(filepath.Join(dir, unit+".service"))
}

func unitSafe(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

func catchupName() string {
	if os.Getenv("FIGLA_STATE") == "" {
		return "figla-catchup"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(stateDir()))
	return fmt.Sprintf("figla-catchup-%08x", h.Sum32())
}

func installCatchup(self string) error {
	dir := unitDir()
	catchupUnit := catchupName()
	files := map[string]string{
		catchupUnit + ".service": strings.Join([]string{
			"[Unit]",
			"Description=figla: deliver reminders that came due while their carrier was gone",
			"",
			"[Service]",
			"Type=oneshot",
			"Environment=" + unitQuote("FIGLA_STATE="+stateDir()),
			"ExecStart=" + unitQuote(self) + " catchup",
			"TimeoutStartSec=900",
			"",
		}, "\n"),
		catchupUnit + ".timer": strings.Join([]string{
			"[Unit]",
			"Description=figla: overdue reminder sweep",
			"",
			"[Timer]",
			"OnBootSec=1min",
			"OnCalendar=*:0/10",
			"",
			"[Install]",
			"WantedBy=timers.target",
			"",
		}, "\n"),
	}
	changed := false
	for name, body := range files {
		path := filepath.Join(dir, name)
		if old, err := os.ReadFile(path); err == nil && string(old) == body {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		changed = true
	}
	link := filepath.Join(dir, "timers.target.wants", catchupUnit+".timer")
	if _, err := os.Lstat(link); err != nil {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join("..", catchupUnit+".timer"), link); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		_ = exec.Command("systemctl", "--user", "restart", catchupUnit+".timer").Run()
	}
	return nil
}

func armDetached(r *Reminder) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "wait", "--aria", r.Aria, "--name", r.Name)
	detach(cmd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	r.PID = cmd.Process.Pid
	return cmd.Process.Release()
}

func cmdWait(args []string) error {
	f, _ := flags(args)
	r, err := find(f["aria"], f["name"])
	if err != nil {
		return err
	}
	for {
		d := time.Until(r.FireAt.Round(0))
		if d <= 0 {
			break
		}
		time.Sleep(min(d, time.Minute))
	}
	if _, err := find(r.Aria, r.Name); err != nil {
		return nil
	}
	return deliver(r)
}

func cmdFire(args []string) error {
	f, _ := flags(args)
	r, err := find(f["aria"], f["name"])
	if err != nil {
		return nil
	}
	return deliver(r)
}

func cmdCatchup() error {
	rs, err := load("")
	if err != nil {
		return err
	}
	reclaimStale()
	var failed []string
	n := 0
	for _, r := range rs {
		if time.Since(r.FireAt) < lateAfter {
			continue
		}
		if err := deliver(r); err != nil {
			failed = append(failed, r.Aria+"/"+r.Name+": "+err.Error())
			continue
		}
		n++
	}
	fmt.Printf("delivered %d overdue reminder(s)\n", n)
	if len(failed) > 0 {
		return errors.New("could not deliver:\n  " + strings.Join(failed, "\n  "))
	}
	return nil
}

func reclaimStale() {
	_ = filepath.WalkDir(stateDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json.firing") {
			return nil
		}
		if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.Rename(p, strings.TrimSuffix(p, ".firing"))
		}
		return nil
	})
}

func deliver(r *Reminder) error {
	claimed := r.path() + ".firing"
	if err := os.Rename(r.path(), claimed); err != nil {
		return nil
	}
	msg := r.message(runWatch(r.Watch))
	fig := r.Figaro
	if fig == "" {
		fig = "figaro"
	}
	var err error
	wait := retryBase()
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		out, e := exec.Command(fig, "send", "-f", "--id", r.Aria, "--", msg).CombinedOutput()
		if e == nil {
			_ = os.Remove(claimed)
			retire(r)
			return nil
		}
		err = fmt.Errorf("send to %s: %s", r.Aria, strings.TrimSpace(string(out)))
	}
	_ = os.Rename(claimed, r.path())
	return err
}

func retryBase() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("FIGLA_RETRY_BASE_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 2 * time.Second
}

func runWatch(watch string) string {
	if watch == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, shell(), shellFlag(), watch).CombinedOutput()
	s := string(out)
	if len(s) > 2048 {
		s = s[:2048] + "\n…(truncated)"
	}
	return s
}

func find(aria, name string) (*Reminder, error) {
	if aria == "" || name == "" {
		return nil, errors.New("need --aria and --name")
	}
	rs, err := load(aria)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no reminder %s/%s", aria, name)
}
