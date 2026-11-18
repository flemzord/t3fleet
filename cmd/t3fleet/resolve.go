package main

import (
	"fmt"
	"strings"

	"github.com/flemzord/t3fleet/internal/fleet"
)

const minIDPrefix = 4

func validateMachineRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "/\\?#\x00\r\n") {
		return hinted("run 't3fleet list' to see managed machines", "invalid machine reference %q", ref)
	}
	return nil
}

// resolveMachine finds one managed machine by exact ID, exact name (with or
// without the t3fleet- prefix) or, when allowPrefix is set, a unique ID prefix.
// Destructive commands pass allowPrefix=false so a short typo cannot select a
// different machine.
func resolveMachine(machines []fleet.Machine, ref string, allowPrefix bool) (fleet.Machine, error) {
	if err := validateMachineRef(ref); err != nil {
		return fleet.Machine{}, err
	}
	for _, m := range machines {
		if m.ID == ref {
			return m, nil
		}
	}
	var named []fleet.Machine
	for _, m := range machines {
		if m.Name != "" && (m.Name == ref || m.Name == "t3fleet-"+ref) {
			named = append(named, m)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if len(named) > 1 {
		return fleet.Machine{}, ambiguous(ref, named)
	}
	if allowPrefix && len(ref) >= minIDPrefix {
		var matches []fleet.Machine
		for _, m := range machines {
			if strings.HasPrefix(m.ID, ref) {
				matches = append(matches, m)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return fleet.Machine{}, ambiguous(ref, matches)
		}
	}
	hint := "run 't3fleet list' to see managed machines"
	if !allowPrefix {
		hint = "this command needs a full ID or exact name; run 't3fleet list' to see them"
	}
	return fleet.Machine{}, hinted(hint, "no managed machine matches %q", ref)
}

func ambiguous(ref string, matches []fleet.Machine) error {
	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%s)", displayName(m), m.ID)
	}
	return hinted("use a longer ID prefix or the full ID", "%q matches several machines: %s", ref, b.String())
}

func displayName(m fleet.Machine) string {
	if m.Name != "" {
		return m.Name
	}
	return shortID(m.ID)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
