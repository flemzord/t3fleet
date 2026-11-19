package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/flemzord/t3fleet/internal/fleet"
)

// The picker only returns IDs from this snapshot. The caller shows the exact
// selection and asks for confirmation after the terminal is back in line mode.
func selectDeleteMachines(ctx context.Context, in, out *os.File, machines []fleet.Machine) (_ []string, err error) {
	if err := ctx.Err(); err != nil || len(machines) == 0 {
		return nil, err
	}
	inFD, outFD := int(in.Fd()), int(out.Fd())
	if !term.IsTerminal(inFD) || !term.IsTerminal(outFD) || os.Getenv("TERM") == "dumb" {
		return nil, hinted("use a terminal or pass --ids ID,ID --yes", "the machine selector needs an interactive terminal")
	}
	old, err := term.MakeRaw(inFD)
	if err != nil {
		return nil, fmt.Errorf("cannot open machine selector: %w", err)
	}
	defer func() {
		_, leave := io.WriteString(out, "\x1b[?2004l\x1b[?25h\x1b[?1049l")
		err = errors.Join(err, leave, term.Restore(inFD, old))
	}()
	if _, err := io.WriteString(out, "\x1b[?1049h\x1b[?25l\x1b[?2004h"); err != nil {
		return nil, err
	}
	return runDeletePicker(ctx, in, out, machines, func() (int, int) {
		w, h, _ := term.GetSize(outFD)
		if w <= 0 || h <= 0 {
			return 80, 24
		}
		return w, h
	})
}

func runDeletePicker(ctx context.Context, in *os.File, out io.Writer, machines []fleet.Machine, size func() (int, int)) ([]string, error) {
	checked := make([]bool, len(machines))
	cursor, first, count := 0, 0, 0
	for {
		width, height := size()
		visible := max(1, height-4)
		first = max(0, min(first, cursor), cursor-visible+1)
		if _, err := io.WriteString(out, deletePickerView(machines, checked, cursor, first, visible, count, width)); err != nil {
			return nil, err
		}
		key, err := deletePickerKey(ctx, in)
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		switch key {
		case 'q', 0x03, 0x04, 0x1b:
			return nil, nil
		case 'k':
			cursor = max(0, cursor-1)
		case 'j':
			cursor = min(len(machines)-1, cursor+1)
		case ' ':
			checked[cursor] = !checked[cursor]
			if checked[cursor] {
				count++
			} else {
				count--
			}
		case '\r', '\n':
			ids := make([]string, 0, count)
			for i, selected := range checked {
				if selected {
					ids = append(ids, machines[i].ID)
				}
			}
			return ids, nil
		}
	}
}

func deletePickerView(machines []fleet.Machine, checked []bool, cursor, first, visible, count, width int) string {
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	line := func(s string) {
		// All text is ASCII (worker identifiers are ASCII too). Keep each
		// machine on one physical line, including on narrow terminals.
		b.WriteString(s[:min(len(s), max(0, width-1))])
		b.WriteString("\r\n")
	}
	line(fmt.Sprintf("Select workers to delete   %d selected", count))
	compact := width < 65
	if compact {
		line("Up/Dn: move  Space: mark  Enter: review  Esc: cancel")
	} else {
		line("Up/Down: move   Space: toggle   Enter: review   Esc: cancel")
	}
	line("")
	maxProvider, maxState := len("PROVIDER"), len("FLEET STATE")
	for _, m := range machines {
		maxProvider = max(maxProvider, len(m.Provider))
		maxState = max(maxState, len(m.State))
	}
	pw, sw := min(maxProvider, 15), min(maxState, 15)
	nw := max(8, min(32, width-25-pw-sw))
	row := func(marker, name, prov, state, id string) {
		if compact {
			// Reserve space for the ID first: generated names share a long
			// prefix and would otherwise be indistinguishable when clipped.
			line(fmt.Sprintf("%s  %-12s  %s  %s  %s", marker, id, strings.TrimPrefix(name, "t3fleet-"), prov, state))
			return
		}
		line(fmt.Sprintf("%s  %-*.*s  %-*.*s  %-*.*s  %s", marker, nw, nw, name, pw, pw, prov, sw, sw, state, id))
	}
	row("     ", "NAME", "PROVIDER", "FLEET STATE", "ID")
	for i := first; i < min(first+visible, len(machines)); i++ {
		m := machines[i]
		point, check := " ", " "
		if i == cursor {
			point = ">"
		}
		if checked[i] {
			check = "x"
		}
		row(point+" ["+check+"]", displayName(m), orDash(m.Provider), orDash(m.State), shortID(m.ID))
	}
	return strings.TrimSuffix(b.String(), "\r\n")
}

// Read exactly one key. In particular, no buffered reader or background
// goroutine may swallow characters belonging to the subsequent confirmation.
func deletePickerKey(ctx context.Context, in *os.File) (byte, error) {
	fd := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
	read := func(waitForKey bool) (byte, error) {
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			n, err := unix.Poll(fd, 100)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if n == 0 {
				if waitForKey {
					continue
				}
				return 0, nil
			}
			var buf [1]byte
			_, err = io.ReadFull(in, buf[:])
			return buf[0], err
		}
	}
	key, err := read(true)
	if err != nil || key != 0x1b {
		return key, err
	}
	// Escape is itself a cancel key; wait briefly for an arrow sequence.
	prefix, err := read(false)
	if err != nil || (prefix != '[' && prefix != 'O') {
		return 0x1b, err
	}
	// Consume a complete CSI/SS3 sequence; e.g. Home or Page Down must
	// not leave parts of the sequence behind as individual keystrokes.
	var seq strings.Builder
	for {
		key, err = read(false)
		if err != nil || key == 0 || seq.Len() >= 16 {
			return 0x1b, err
		}
		seq.WriteByte(key)
		if key >= '@' && key <= '~' {
			break
		}
	}
	switch seq.String() {
	case "A":
		return 'k', nil
	case "B":
		return 'j', nil
	case "200~":
		// Pasted text is not keyboard input: it may contain Space, Enter,
		// and even the confirmation word. Drain it before resuming.
		const end = "\x1b[201~"
		interrupted := false
		for matched := 0; matched < len(end); {
			b, err := read(!interrupted)
			if err != nil {
				return 0, err
			}
			if interrupted && b == 0 {
				return 0x03, nil // No more input from an interrupted paste.
			}
			if b == 0x03 {
				interrupted = true // Drain queued paste bytes before handing the terminal back.
			}
			switch b {
			case end[matched]:
				matched++
			case end[0]:
				matched = 1
			default:
				matched = 0
			}
		}
		if interrupted {
			return 0x03, nil
		}
	}
	return 0, nil
}
