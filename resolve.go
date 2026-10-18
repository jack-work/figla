package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func canonicalAria(ref string) (string, error) {
	fig, err := exec.LookPath("figaro")
	if err != nil {
		return "", fmt.Errorf("cannot find figaro on PATH: %w", err)
	}
	out, err := exec.Command(fig, "status", ref, "-j").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("figaro cannot resolve aria %q: %s", ref, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	var st struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(out, &st) != nil || st.ID == "" {
		return ref, nil
	}
	return st.ID, nil
}

func loadAria(ref string) ([]*Reminder, error) {
	if ref == "" {
		return load("")
	}
	keys := []string{ref}
	if id, err := canonicalAria(ref); err == nil && id != ref {
		keys = append([]string{id}, keys...)
	}
	var out []*Reminder
	for _, k := range keys {
		rs, err := load(k)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}
