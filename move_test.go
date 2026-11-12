package main

import (
	"strings"
	"testing"
	"time"
)

func TestMoveHandsRemindersToTheSuccessor(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	defer reap(t, bin, state, path, "old", "new")
	for _, args := range [][]string{
		{"arm", "--aria", "old", "--in", "3s", "--about", "the kettle", "--name", "k"},
		{"arm", "--aria", "old", "--in", "1h", "--about", "the oven", "--name", "o"},
		{"arm", "--aria", "new", "--in", "1h", "--about", "its own oven", "--name", "o"},
	} {
		if out, err := run(t, bin, state, path, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if out, err := run(t, bin, state, path, "move", "--from", "old", "--to", "new"); err != nil {
		t.Fatalf("move: %v\n%s", err, out)
	}
	if out, _ := run(t, bin, state, path, "list", "--aria", "old"); !strings.Contains(out, "no active reminders") {
		t.Fatalf("old aria still holds reminders:\n%s", out)
	}
	out, _ := run(t, bin, state, path, "list", "--aria", "new")
	for _, want := range []string{"its own oven", "o-old", "the oven"} {
		if !strings.Contains(out, want) {
			t.Fatalf("new aria list lacks %q:\n%s", want, out)
		}
	}
	got := waitSent(t, sent, "the kettle", 20*time.Second)
	if !strings.Contains(got, "--id new") || strings.Contains(got, "--id old") {
		t.Fatalf("delivered to the wrong aria: %s", got)
	}
}

func TestMoveRedirectsARunningCommand(t *testing.T) {
	bin := build(t)
	state, sent, path := env(t)
	defer reap(t, bin, state, path, "old", "new")
	if out, err := run(t, bin, state, path, "run", "--aria", "old", "--about", "slow build", "--", "sleep 2; echo built"); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if out, err := run(t, bin, state, path, "move", "--from", "old", "--to", "new"); err != nil {
		t.Fatalf("move: %v\n%s", err, out)
	}
	got := waitSent(t, sent, "built", 20*time.Second)
	if !strings.Contains(got, "--id new") || strings.Contains(got, "--id old") {
		t.Fatalf("delivered to the wrong aria: %s", got)
	}
}

func TestMoveNeedsBothArias(t *testing.T) {
	bin := build(t)
	state, _, path := env(t)
	if _, err := run(t, bin, state, path, "move", "--from", "a"); err == nil {
		t.Fatal("move without --to succeeded")
	}
	if _, err := run(t, bin, state, path, "move", "--from", "a", "--to", "a"); err == nil {
		t.Fatal("move onto itself succeeded")
	}
}
