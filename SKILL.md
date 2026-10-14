---
name: figla
description: Fire-and-forget reminders for a figaro aria. Use figla instead of polling a background process in a loop, sleeping inside a tool call, or spawning ad-hoc `sleep && …` shells, arm a reminder for when you expect work to finish, cancel it if the work reports first, and get a message carrying live state if it does not. Reminders are scoped per aria.
---

# figla: "Figaro là"

Figaro sings *"Figaro qua, Figaro là"*: here, over there. A reminder is figaro
calling to himself from a little further along in time.

## When to reach for it

**Any time you are waiting on something you did not start synchronously.**
Background builds, subagents, long test runs, a worker aria you dispatched.

**Do not** poll in a loop. **Do not** `sleep 300` inside a tool call. **Do not**
spawn `setsid bash -c 'sleep … && …'`. Those three habits burn turns, hold
context hostage, and leak processes: one agent session left 230 orphaned
daemons and tripped a memory-pressure alert doing exactly that.

## The whole surface

```bash
figla arm --aria <id> (--in <dur> | --at <time>) --about <text> [--watch <cmd>] [--name <n>]
figla list [--aria <id>] [--json]
figla cancel <name> | --aria <id> --all
figla catchup
```

`--in` takes a Go duration: `90s`, `25m`, `1h30m`.
`--at` takes local wall-clock time, `"2026-10-18 08:55"`, or RFC 3339. Use it
for anything tied to the calendar (a kickoff, a deadline, a nightly job):
computing `--in` by hand from the current time is arithmetic you can get wrong.

Re-arming an existing `--name` replaces it.

## The pattern

```bash
# dispatch work, then arm a reminder for when you expect it back
figla arm --aria $MY_ARIA --in 25m --about "FX: footer + exit hang" \
      --watch "git -C /home/gluck/dev/figaro-qua/fx log --oneline -1"

# … the worker reports back in time …
figla cancel --aria $MY_ARIA fx-footer-exit-hang

# … or it does not, and a message arrives carrying the state already gathered
```

Your own aria id is in your form as `<system-reminder name="aria_id">`.

## Why `--watch` matters

A reminder that only says *"check on X"* costs you another round trip to find
out anything. `--watch` runs a command **at fire time** and folds its output
into the message, so what arrives is state, not a prompt. Bounded to 10s and
2KB so a hung watch cannot become its own incident.

Good watches: `git -C <wt> log --oneline -1`, `figaro list -a | grep <id>`,
`tail -3 /tmp/build.log`.

## Rules that came from getting it wrong

1. **Calibrate from observed durations, not optimism.** Arm at roughly 1.5× the
   time you expect. A reminder that fires on merely-slow work is noise, and
   noise gets ignored.
2. **Cancel on report.** Make `figla cancel` the first thing you do when a
   worker answers. An uncancelled reminder is a lie waiting to be told.
3. **Reap when a phase ends**: `figla cancel --aria <id> --all`. Reminders are
   aria-scoped precisely so this is safe.
4. **A fired reminder is gone.** There is nothing to cancel afterwards; arm a
   fresh one if the work is still running.
5. **Repeat by re-arming, not by looping.** If you want nagging, arm the next
   reminder when the previous one fires.

## How it works, in case it misbehaves

No daemon. The state directory **is** the registry:
`$XDG_STATE_HOME/figla/<aria>/<name>.json` (`%LOCALAPPDATA%\figla` on Windows),
overridable with `FIGLA_STATE`.

Two carriers, chosen automatically (`FIGLA_BACKEND=systemd|detach` forces one):

- **systemd**, when a user manager is reachable. Each reminder is a pair of
  unit files in `~/.config/systemd/user`, `figla-<aria>-<name>-<unix>.timer`
  and `.service`, with `OnCalendar=` set to the absolute deadline and the timer
  enabled under `timers.target`. Wall-clock anchoring is the point: it survives
  reboots, `daemon-reload` (which every home-manager switch runs) and suspend.
  The unit files delete themselves after delivery.
- **detach**, a detached child process, everywhere else. It re-checks the wall
  clock every minute, so suspend cannot stretch it, but it does not survive a
  reboot. On those hosts run `figla catchup` from a login hook.

**Catchup.** `figla-catchup.timer` runs one minute after boot and every ten
minutes. It delivers every reminder more than 30s overdue, whatever happened to
its carrier, and the message says it was delivered **LATE** and by how much.

**Delivery is claimed, not deleted.** The record is renamed to `.json.firing`
while it sends, so a timer and a catchup cannot both deliver it. Five attempts
with backoff; if all fail, the record goes back and the next catchup retries.
A reminder still undeliverable a day past due (its aria is gone) is dropped,
and catchup says so in its journal.
A reminder is only removed once it has actually been sent.

`list` shows overdue reminders as `overdue`, and marks a reminder whose carrier
is gone with `!` after the carrier name. Both will be delivered by the next
catchup; neither is silently dropped.

`figaro` is resolved to an absolute path when the reminder is **armed**, because
a systemd unit or a detached child inherits a minimal environment. Looking it up
later is how a reminder silently fails to remind.

### Why 0.1.0 lost reminders

0.1.0 used transient `systemd-run --on-active` timers. Those count from when the
timer was (re)loaded, so each `daemon-reload` pushed every pending reminder back
by however long it had already waited, and a reboot erased them all. On
2026-10-12..14 that silently dropped five scheduled jobs. 0.1.0 also deleted the
record before sending, so a failed send left no trace.
