package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests build and run the REAL binary and let it arm a REAL detached
// carrier. That is deliberate. This project's parent spent a night discovering
// that every test double which diverged from production diverged by being
// tidier than reality — a fake paginator, whole strings instead of keystrokes,
// counts instead of byte budgets. A reminder tool whose tests never detach a
// process would be the same mistake in a new package.
//
// The delivery target is a fake `figaro` on PATH that appends to a file, so we
// assert on what was actually sent without touching a real aria.

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "figla")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// env isolates the registry and installs a fake figaro. Nothing here can reach
// a real reminder or a real aria.
func env(t *testing.T) (state, sent, path string) {
	t.Helper()
	dir := t.TempDir()
	state = filepath.Join(dir, "state")
	sent = filepath.Join(dir, "sent.txt")
	bindir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + sent + "\n"
	if err := os.WriteFile(filepath.Join(bindir, "figaro"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return state, sent, bindir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func run(t *testing.T, bin, state, path string, args ...string) (string, error) {
	t.Helper()
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(), "FIGLA_STATE="+state, "PATH="+path)
	out, err := c.CombinedOutput()
	return string(out), err
}

// reap cancels everything a test armed, whatever the test's outcome.
//
// This is not hygiene theatre. The first run of this suite left two detached
// carriers alive for ten minutes because two tests armed reminders and never
// cancelled them — figla's own tests committing the exact orphan-leak this
// tool exists to end. A test that leaks the thing it is testing has no
// standing to complain about anyone else's leaks.
func reap(t *testing.T, bin, state, path string, arias ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, a := range arias {
			_, _ = run(t, bin, state, path, "cancel", "--aria", a, "--all")
		}
	})
}

func TestArmListCancel(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	reap(t, bin, state, path, "a1")

	if out, err := run(t, bin, state, path, "arm", "--aria", "a1", "--in", "10m",
		"--about", "worker FX late", "--name", "fx"); err != nil {
		t.Fatalf("arm: %v\n%s", err, out)
	}
	out, err := run(t, bin, state, path, "list", "--aria", "a1")
	if err != nil || !strings.Contains(out, "fx") {
		t.Fatalf("list did not show the reminder: %v\n%s", err, out)
	}
	if _, err := run(t, bin, state, path, "cancel", "--aria", "a1", "fx"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	out, _ = run(t, bin, state, path, "list", "--aria", "a1")
	if !strings.Contains(out, "no active reminders") {
		t.Fatalf("reminder survived cancel:\n%s", out)
	}
}

// The user asked for aria scoping explicitly: one aria's reminders must be
// invisible to another, and `cancel --all` must not reach across.
func TestAriaScoping(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	reap(t, bin, state, path, "a1", "a2")
	run(t, bin, state, path, "arm", "--aria", "a1", "--in", "10m", "--about", "one", "--name", "n1")
	run(t, bin, state, path, "arm", "--aria", "a2", "--in", "10m", "--about", "two", "--name", "n2")

	out, _ := run(t, bin, state, path, "list", "--aria", "a1")
	if strings.Contains(out, "n2") {
		t.Fatalf("a1's list leaked a2's reminder:\n%s", out)
	}
	if _, err := run(t, bin, state, path, "cancel", "--aria", "a1", "--all"); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, bin, state, path, "list", "--aria", "a2")
	if !strings.Contains(out, "n2") {
		t.Fatalf("cancel --all on a1 destroyed a2's reminder:\n%s", out)
	}
}

func TestCancelAllRequiresAria(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	out, err := run(t, bin, state, path, "cancel", "--all")
	if err == nil {
		t.Fatalf("cancel --all without --aria must fail, got:\n%s", out)
	}
	if !strings.Contains(out, "aria-scoped") {
		t.Fatalf("error should explain the scoping rule, got:\n%s", out)
	}
}

// The end-to-end property: a real detached carrier survives the arming
// process, wakes, and delivers. Nothing here is stubbed but figaro itself.
func TestFiresAndDelivers(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	if out, err := run(t, bin, state, path, "arm", "--aria", "a9", "--in", "2s",
		"--about", "the kettle", "--name", "k"); err != nil {
		t.Fatalf("arm: %v\n%s", err, out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(sent); err == nil && strings.Contains(string(b), "the kettle") {
			if !strings.Contains(string(b), "--id a9") {
				t.Fatalf("delivered to the wrong aria: %s", b)
			}
			// A fired reminder is not pending; the registry must not claim it is.
			out, _ := run(t, bin, state, path, "list", "--aria", "a9")
			if !strings.Contains(out, "no active reminders") {
				t.Fatalf("fired reminder still listed:\n%s", out)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("reminder never delivered")
}

// --watch is what makes a reminder carry state instead of asking the agent to
// go find it. If the output does not arrive, the flag is decorative.
func TestWatchOutputIsDelivered(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	run(t, bin, state, path, "arm", "--aria", "aw", "--in", "2s",
		"--about", "watched", "--name", "w", "--watch", "echo MARKER-7")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(sent); err == nil && strings.Contains(string(b), "MARKER-7") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("--watch output never reached the message")
}

// Cancelling must actually stop the carrier, not merely delete the record.
// Deleting alone would leave a process that wakes to find nothing — quiet, but
// still an orphan, which is the habit this tool exists to end.
func TestCancelPreventsDelivery(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	run(t, bin, state, path, "arm", "--aria", "ac", "--in", "3s", "--about", "doomed", "--name", "d")
	if _, err := run(t, bin, state, path, "cancel", "--aria", "ac", "d"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second)
	if b, err := os.ReadFile(sent); err == nil && strings.Contains(string(b), "doomed") {
		t.Fatalf("cancelled reminder still fired: %s", b)
	}
}

func TestArmValidation(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	for _, c := range [][]string{
		{"arm", "--aria", "x", "--in", "5m"},                       // no --about
		{"arm", "--aria", "x", "--about", "y"},                     // no --in
		{"arm", "--in", "5m", "--about", "y"},                      // no --aria
		{"arm", "--aria", "x", "--in", "nonsense", "--about", "y"}, // bad duration
		{"arm", "--aria", "x", "--in", "-5m", "--about", "y"},      // past
	} {
		if out, err := run(t, bin, state, path, c...); err == nil {
			t.Fatalf("%v should have failed, got:\n%s", c, out)
		}
	}
}

// A name is a filesystem path component. A reminder called ../../etc must not
// be able to write there.
func TestNameCannotEscapeTheRegistry(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	reap(t, bin, state, path, "../../evil")
	run(t, bin, state, path, "arm", "--aria", "../../evil", "--in", "10m",
		"--about", "traversal", "--name", "../../oops")
	var found []string
	filepath.WalkDir(state, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".json") {
			found = append(found, p)
		}
		return nil
	})
	for _, p := range found {
		if !strings.HasPrefix(p, state) {
			t.Fatalf("escaped the registry: %s", p)
		}
	}
}

func TestListJSONIsMachineReadable(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	reap(t, bin, state, path, "aj")
	run(t, bin, state, path, "arm", "--aria", "aj", "--in", "10m", "--about", "json", "--name", "j")
	out, err := run(t, bin, state, path, "list", "--aria", "aj", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rs []Reminder
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if len(rs) != 1 || rs[0].Aria != "aj" {
		t.Fatalf("unexpected payload: %s", out)
	}
}

// figaro is resolved when the reminder is ARMED, because a detached carrier
// inherits a minimal environment. Failing to find it an hour later is a silent
// failure of the very thing meant to catch silent failures.
func TestFigaroPathIsResolvedAtArmTime(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	reap(t, bin, state, path, "ap")
	run(t, bin, state, path, "arm", "--aria", "ap", "--in", "10m", "--about", "path", "--name", "p")
	b, err := os.ReadFile(filepath.Join(state, "ap", "p.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Reminder
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(r.Figaro) {
		t.Fatalf("figaro path not absolute at arm time: %q", r.Figaro)
	}
}

// The message must tell the agent to use figla again rather than fall back to
// polling loops or ad-hoc sleep shells. That instruction is the tool's whole
// behavioural purpose and must not be quietly dropped.
func TestMessageDiscouragesPolling(t *testing.T) {
	r := &Reminder{Name: "n", Aria: "a", About: "thing", Created: time.Now()}
	m := r.message("")
	for _, want := range []string{"REMINDER (figla)", "thing", "figla arm", "polling"} {
		if !strings.Contains(m, want) {
			t.Fatalf("message missing %q:\n%s", want, m)
		}
	}
}
