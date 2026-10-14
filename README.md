# figla

Fire-and-forget reminders for a [figaro](https://github.com/jack-work/figaro) aria.

    go build -o ~/.local/bin/figla .        # ~0.1s

Full documentation is in [SKILL.md](SKILL.md), which is also installed as a
figaro skill at `~/.config/figaro/skills/figla.md`.

    figla arm --aria <id> --in 25m --about "what you are waiting on" \
          --watch "git -C /path/to/worktree log --oneline -1"
    figla list --aria <id>
    figla cancel --aria <id> --all

## Design notes

- **No daemon.** The state directory is the registry.
- **Two carriers**: transient systemd `--user` timers where a user bus is
  actually reachable, a detached child everywhere else. The detached path is
  the common one — plenty of Linux hosts run `systemd --user` with no bus.
- **Aria-scoped.** `cancel --aria X --all` cannot reach another aria's work.
- **`--watch` carries state**, so a fired reminder is actionable rather than a
  prompt to go looking.

Tests run the real binary and arm a real detached carrier against a fake
`figaro` on PATH. Nothing important is doubled, because every test double that
diverged from production in this project's history diverged by being tidier
than reality.
