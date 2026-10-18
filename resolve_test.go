package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fullID = "a5647bfbc216b8a09ffea9586393c289"

// envResolving is env with a fake figaro that also answers `status <ref> -j`
// the way the real one does: a unique prefix resolves to the full id, an
// ambiguous one exits 1 naming the candidates.
func envResolving(t *testing.T) (state, sent, path string) {
	t.Helper()
	state, sent, path = env(t)
	bindir := strings.SplitN(path, string(os.PathListSeparator), 2)[0]
	fake := `#!/bin/sh
if [ "$1" = status ]; then
  case "$2" in
    0a76) echo "error: 0a76 is ambiguous: 0a765245, 0a76f484" >&2; exit 1 ;;
  esac
  case "` + fullID + `" in
    "$2"*) printf '{"id":"` + fullID + `"}\n'; exit 0 ;;
  esac
  echo "error: no aria or form \"$2\"" >&2; exit 1
fi
printf '%s\n' "$*" >> ` + sent + "\n"
	if err := os.WriteFile(filepath.Join(bindir, "figaro"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return state, sent, path
}

// A reminder armed with a short form of an aria id must be the same reminder
// to list and cancel under the full id, and the reverse. Before, the registry
// keyed on the literal string, so the two forms never met, and a stored prefix
// could become ambiguous before the timer fired.
func TestShortAndFullIDsNameTheSameReminders(t *testing.T) {
	bin := build(t)
	state, _, path := envResolving(t)
	reap(t, bin, state, path, fullID)

	if out, err := run(t, bin, state, path, "arm", "--aria", fullID[:9], "--in", "10m",
		"--about", "short form", "--name", "short"); err != nil {
		t.Fatalf("arm by prefix: %v\n%s", err, out)
	}
	out, err := run(t, bin, state, path, "list", "--aria", fullID, "--json")
	if err != nil || !strings.Contains(out, `"aria":"`+fullID+`"`) || !strings.Contains(out, "short") {
		t.Fatalf("a reminder armed by prefix is not stored under the full id: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(state, fullID[:9])); err == nil {
		t.Fatalf("a registry directory was made for the prefix")
	}

	if out, err := run(t, bin, state, path, "arm", "--aria", fullID, "--in", "10m",
		"--about", "full form", "--name", "full"); err != nil {
		t.Fatalf("arm by full id: %v\n%s", err, out)
	}
	if out, err := run(t, bin, state, path, "cancel", "--aria", fullID[:6], "full"); err != nil {
		t.Fatalf("cancel by prefix missed a reminder armed by full id: %v\n%s", err, out)
	}
	out, _ = run(t, bin, state, path, "cancel", "--aria", fullID[:6], "--all")
	if !strings.Contains(out, "cancelled 1 reminder") {
		t.Fatalf("cancel --all by prefix: %s", out)
	}
}

func TestArmRefusesAnAriaFigaroCannotResolve(t *testing.T) {
	bin := build(t)
	state, _, path := envResolving(t)
	for _, ref := range []string{"0a76", "deadbeef"} {
		out, err := run(t, bin, state, path, "arm", "--aria", ref, "--in", "10m", "--about", "x")
		if err == nil {
			reap(t, bin, state, path, ref)
			t.Fatalf("arm accepted %q, which figaro cannot resolve:\n%s", ref, out)
		}
		if !strings.Contains(out, "cannot resolve") {
			t.Fatalf("arm %q: the error does not say why:\n%s", ref, out)
		}
	}
}
