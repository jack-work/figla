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
- **Durable on systemd.** Each reminder is an enabled `OnCalendar=` timer on
  disk, anchored to wall-clock time, so reboots, `daemon-reload` and suspend
  cannot move or erase it. `figla-catchup.timer` delivers anything overdue,
  marked late, after boot and every ten minutes.
- **Detached child** everywhere else (Windows, macOS, busless Linux).
- **Delivery is claimed, then confirmed.** A failed send keeps the reminder.
- **Aria-scoped.** `cancel --aria X --all` cannot reach another aria's work.
- **`--watch` carries state**, so a fired reminder is actionable rather than a
  prompt to go looking.

Tests run the real binary against a fake `figaro` on PATH, arm real detached
carriers, and, where a systemd user manager exists, a real calendar timer.
