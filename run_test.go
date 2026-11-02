package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitSent(t *testing.T, sent, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(sent); err == nil && strings.Contains(string(b), want) {
			return string(b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(sent)
	t.Fatalf("never sent %q; sent so far:\n%s", want, b)
	return ""
}

func TestRunReportsExitAndOutputWhenTheCommandExits(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	reap(t, bin, state, path, "ar")
	start := time.Now()
	out, err := run(t, bin, state, path, "run", "--aria", "ar", "--about", "the build", "--name", "b",
		"--", "sleep 1; echo OUT-MARK; echo ERR-MARK >&2; pwd; exit 3")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("run blocked on the command (%s)", time.Since(start))
	}
	msg := waitSent(t, sent, "RUN DONE", 10*time.Second)
	cwd, _ := os.Getwd()
	for _, want := range []string{"--id ar", "the build", "exit status 3", "OUT-MARK", "ERR-MARK", cwd} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if out, _ := run(t, bin, state, path, "list", "--aria", "ar"); !strings.Contains(out, "no active reminders") {
		t.Fatalf("finished run still listed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "ar", "b.log")); err != nil {
		t.Fatalf("log named in the message is gone: %v", err)
	}
}

func TestRunIsListedWhileRunning(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	reap(t, bin, state, path, "al")
	run(t, bin, state, path, "run", "--aria", "al", "--name", "slow", "--", "sleep 30")
	out, _ := run(t, bin, state, path, "list", "--aria", "al")
	if !strings.Contains(out, "slow") || !strings.Contains(out, "run ") || strings.Contains(out, "run!") {
		t.Fatalf("running command not listed as a live run:\n%s", out)
	}
}

func TestCancelKillsTheCommandAndSendsNothing(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	reap(t, bin, state, path, "ak")
	marker := filepath.Join(t.TempDir(), "survived")
	run(t, bin, state, path, "run", "--aria", "ak", "--name", "k", "--", "sleep 2; touch "+marker)
	if out, err := run(t, bin, state, path, "cancel", "--aria", "ak", "k"); err != nil {
		t.Fatalf("cancel: %v\n%s", err, out)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("cancel left the command running")
	}
	if b, _ := os.ReadFile(sent); strings.Contains(string(b), "RUN") {
		t.Fatalf("cancelled run still reported: %s", b)
	}
}

func TestRunNeedsACommand(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	if out, err := run(t, bin, state, path, "run", "--aria", "a1"); err == nil {
		t.Fatalf("run without a command must fail:\n%s", out)
	}
}

func TestCatchupReportsARunWhoseCarrierDied(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	fig := filepath.Join(strings.SplitN(path, string(os.PathListSeparator), 2)[0], "figaro")
	writeRecord(t, state, Reminder{
		Name: "lost", Aria: "ad", About: "nightly export", Run: "export.sh", Backend: "run",
		Created: time.Now().Add(-time.Hour), Figaro: fig, PID: 0,
	})
	if out, err := run(t, bin, state, path, "catchup"); err != nil {
		t.Fatalf("catchup: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(sent)
	if !strings.Contains(string(b), "RUN LOST") || !strings.Contains(string(b), "nightly export") {
		t.Fatalf("lost run not reported: %q", b)
	}
}

func TestCatchupLeavesALiveRunAlone(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	fig := filepath.Join(strings.SplitN(path, string(os.PathListSeparator), 2)[0], "figaro")
	writeRecord(t, state, Reminder{
		Name: "live", Aria: "av", About: "still going", Run: "sleep 99", Backend: "run",
		Created: time.Now().Add(-time.Hour), Figaro: fig, PID: os.Getpid(),
	})
	if out, err := run(t, bin, state, path, "catchup"); err != nil {
		t.Fatalf("catchup: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(sent); strings.Contains(string(b), "still going") {
		t.Fatalf("catchup reported a run that is still running: %s", b)
	}
}
