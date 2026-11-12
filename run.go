package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const runTail = 2048

func splitCommand(args []string) (opts, command []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func cmdRun(args []string) error {
	opts, command := splitCommand(args)
	f, _ := flags(opts)
	line := strings.TrimSpace(strings.Join(command, " "))
	if f["aria"] == "" || line == "" {
		return errors.New("run needs --aria and a command after --")
	}
	aria, err := canonicalAria(f["aria"])
	if err != nil {
		return err
	}
	about := f["about"]
	if about == "" {
		about = line
	}
	name := f["name"]
	if name == "" {
		name = slug(about)
	}
	fig, err := exec.LookPath("figaro")
	if err != nil {
		return fmt.Errorf("cannot find figaro on PATH: %w", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if old, err := find(aria, name); err == nil {
		disarm(old)
		_ = old.remove()
	}
	r := &Reminder{
		Name: name, Aria: aria, About: about, Watch: f["watch"],
		Run: line, Dir: dir, Created: time.Now(), Figaro: fig, Backend: "run",
	}
	r.Log = r.logPath()
	if err := r.save(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		_ = r.remove()
		return err
	}
	if backendAllowed("systemd") && systemdUsable() {
		_ = installCatchup(self)
	}
	carrier := exec.Command(self, "runner", "--aria", r.Aria, "--name", r.Name)
	detach(carrier)
	gate, err := carrier.StdinPipe()
	if err != nil {
		_ = r.remove()
		return err
	}
	if err := carrier.Start(); err != nil {
		_ = r.remove()
		return err
	}
	r.PID = carrier.Process.Pid
	_ = carrier.Process.Release()
	if err := r.save(); err != nil {
		killGroup(r.PID)
		_ = r.remove()
		return err
	}
	gate.Close()
	fmt.Printf("running %s/%s (pid %d), log %s\n", aria, name, r.PID, r.Log)
	return nil
}

func (r *Reminder) logPath() string {
	return filepath.Join(ariaDir(r.Aria), safe(r.Name)+".log")
}

func cmdRunner(args []string) error {
	f, _ := flags(args)
	_, _ = io.Copy(io.Discard, os.Stdin)
	r, err := find(f["aria"], f["name"])
	if err != nil {
		return nil
	}
	log, err := os.Create(r.Log)
	if err != nil {
		return err
	}
	c := exec.Command(shell(), shellFlag(), r.Run)
	c.Dir, c.Stdout, c.Stderr = r.Dir, log, log
	start := time.Now()
	runErr := c.Run()
	log.Close()
	end := time.Now()

	r, err = findMoved(r.Aria, r.Name)
	if err != nil {
		return nil
	}
	r.FireAt = end
	r.Exit = exitStatus(c, runErr)
	r.Took = end.Sub(start).Round(time.Second).String()
	if err := r.save(); err != nil {
		return err
	}
	return deliver(r)
}

func exitStatus(c *exec.Cmd, err error) string {
	if c.ProcessState != nil {
		return c.ProcessState.String()
	}
	if err != nil {
		return "did not start: " + err.Error()
	}
	return "unknown"
}

func (r *Reminder) runMessage(watchOut string) string {
	var b strings.Builder
	if r.Exit == "" {
		fmt.Fprintf(&b, "RUN LOST (figla) — %s\n", r.About)
		fmt.Fprintf(&b, "$ %s\nstarted %s ago; its carrier died before the command finished (reboot?). Exit status unknown.\n",
			r.Run, time.Since(r.Created).Round(time.Second))
	} else {
		fmt.Fprintf(&b, "RUN DONE (figla) — %s\n", r.About)
		fmt.Fprintf(&b, "$ %s\n%s after %s", r.Run, r.Exit, r.Took)
		if late := time.Since(r.FireAt); late > lateAfter {
			fmt.Fprintf(&b, "; delivered LATE by %s", late.Round(time.Minute))
		}
		b.WriteString(".\n")
	}
	if tail := tailFile(r.Log, runTail); strings.TrimSpace(tail) != "" {
		fmt.Fprintf(&b, "\noutput (tail, full log %s):\n%s\n", r.Log, strings.TrimRight(tail, "\n"))
	} else {
		b.WriteString("\nno output.\n")
	}
	if strings.TrimSpace(watchOut) != "" {
		fmt.Fprintf(&b, "\n--watch %s\n%s\n", r.Watch, strings.TrimRight(watchOut, "\n"))
	}
	return b.String()
}

func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	cut := info.Size() > n
	if cut {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	s := string(b)
	if cut {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = "…(truncated)\n" + s
	}
	return s
}
