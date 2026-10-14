package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// arm chooses a carrier and starts it. systemd transient timers are preferred
// where they genuinely work: they cost nothing while pending, survive the
// session, and self-collect. But a user bus is frequently absent — this very
// machine has `systemd --user` running with no reachable bus — so the detached
// child is not a fallback of last resort, it is the common path, and it is
// tested first.
func arm(r *Reminder) error {
	if systemdUsable() {
		if err := armSystemd(r); err == nil {
			r.Backend = "systemd"
			return nil
		}
		// fall through: a broken timer is worse than a plain child
	}
	if err := armDetached(r); err != nil {
		return err
	}
	r.Backend = "detach"
	return nil
}

func disarm(r *Reminder) {
	switch r.Backend {
	case "systemd":
		// Stop the TIMER, not the service. `--on-active` creates both, and
		// stopping the service is a no-op before it fires — a footgun that
		// looks like a successful cancel.
		_ = exec.Command("systemctl", "--user", "stop", r.Unit+".timer").Run()
		_ = exec.Command("systemctl", "--user", "stop", r.Unit+".service").Run()
	case "detach":
		killPID(r.PID)
	}
}

// alive reports whether the carrier still exists. `list` uses it to sweep, so
// a crashed child or a reaped unit does not linger in the registry claiming to
// be pending.
func alive(r *Reminder) bool {
	if time.Now().After(r.FireAt.Add(2 * time.Minute)) {
		return false // overdue with no delivery: the carrier is gone
	}
	switch r.Backend {
	case "systemd":
		out, _ := exec.Command("systemctl", "--user", "is-active", r.Unit+".timer").Output()
		return strings.TrimSpace(string(out)) != "inactive"
	case "detach":
		return pidAlive(r.PID)
	}
	return false
}

func systemdUsable() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	// is-system-running exits non-zero in degraded states but still answers;
	// what we actually care about is whether the BUS responds at all.
	out, err := exec.Command("systemctl", "--user", "show", "--property=Version").CombinedOutput()
	return err == nil && strings.Contains(string(out), "Version=")
}

func armSystemd(r *Reminder) error {
	r.Unit = "figla-" + safe(r.Aria) + "-" + safe(r.Name)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	d := time.Until(r.FireAt).Round(time.Second)
	// --collect reaps the unit even if the payload fails. Without it a failed
	// reminder lingers forever, which is the orphan problem in miniature.
	// RuntimeMaxSec stops a --watch that hangs from becoming its own incident.
	cmd := exec.Command("systemd-run", "--user", "--collect",
		"--unit="+r.Unit,
		fmt.Sprintf("--on-active=%ds", int(d.Seconds())),
		"--property=RuntimeMaxSec=120",
		"--description=figla reminder for aria "+r.Aria,
		self, "fire", "--aria", r.Aria, "--name", r.Name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func armDetached(r *Reminder) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "wait", "--aria", r.Aria, "--name", r.Name)
	detach(cmd) // platform-specific: new session / new process group
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	r.PID = cmd.Process.Pid
	// Release so the child is reparented to init rather than becoming a zombie
	// when this short-lived `arm` process exits.
	return cmd.Process.Release()
}

// cmdWait is the detached carrier: sleep, then deliver. It re-reads the record
// at fire time so a cancel that raced the sleep is still honoured.
func cmdWait(args []string) error {
	f, _ := flags(args)
	r, err := find(f["aria"], f["name"])
	if err != nil {
		return err
	}
	if d := time.Until(r.FireAt); d > 0 {
		time.Sleep(d)
	}
	if _, err := find(r.Aria, r.Name); err != nil {
		return nil // cancelled while we slept; say nothing
	}
	return deliver(r)
}

func cmdFire(args []string) error {
	f, _ := flags(args)
	r, err := find(f["aria"], f["name"])
	if err != nil {
		return nil // already cancelled
	}
	return deliver(r)
}

// deliver runs --watch (bounded), sends the message, and removes the record.
// The record goes FIRST-ish on purpose: a reminder that fired is not pending,
// and leaving it would make `list` lie.
func deliver(r *Reminder) error {
	var watchOut string
	if r.Watch != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, shell(), shellFlag(), r.Watch).CombinedOutput()
		watchOut = string(out)
		if len(watchOut) > 2048 {
			watchOut = watchOut[:2048] + "\n…(truncated)"
		}
	}
	_ = r.remove()
	fig := r.Figaro
	if fig == "" {
		fig = "figaro"
	}
	// -f is fire-and-forget: the reminder must not attach to a stream or wait
	// for a turn. A plain send (never --steer) so it opens its own turn rather
	// than folding into whatever the aria is mid-thought about.
	cmd := exec.Command(fig, "send", "-f", "--id", r.Aria, "--", r.message(watchOut))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("send to %s: %s", r.Aria, strings.TrimSpace(string(out)))
	}
	return nil
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
