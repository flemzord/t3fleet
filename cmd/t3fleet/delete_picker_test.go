package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/fleet"
)

func pickerPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}

func TestDeletePickerMovesChecksAndLeavesConfirmationUnread(t *testing.T) {
	machines := []fleet.Machine{
		{ID: "one", Name: "first", State: "ready"},
		{ID: "two", Name: "second"},
		{ID: "three", Name: "third"},
		{ID: "four", Name: "fourth"},
	}
	r, w := pickerPipe(t)
	// Select the last worker, then the second. Toggle the first twice.
	_, err := io.WriteString(w, "\x1b[B\x1b[B\x1b[B \x1bOA\x1bOA \x1b[A  \rdelete\n")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ids, err := runDeletePicker(t.Context(), r, &out, machines, func() (int, int) { return 72, 6 })
	if err != nil || !slices.Equal(ids, []string{"two", "four"}) {
		t.Fatalf("selected %v: %v", ids, err)
	}
	remainder := make([]byte, len("delete\n"))
	if _, err := io.ReadFull(r, remainder); err != nil || string(remainder) != "delete\n" {
		t.Fatalf("confirmation input = %q, %v", remainder, err)
	}
	frames := strings.Split(out.String(), "\x1b[H\x1b[2J")
	sawLast := false
	for _, f := range frames {
		if strings.Contains(f, "> [x]  fourth") {
			sawLast = true
			if strings.Contains(f, "first") || strings.Contains(f, "second") {
				t.Fatal("picker exceeded its two-row viewport")
			}
		}
	}
	if !sawLast {
		t.Fatal("cursor moved beyond the visible rows")
	}
}

func TestDeletePickerNeverImplicitlySelects(t *testing.T) {
	machines := []fleet.Machine{{ID: "one"}}
	for _, keys := range []string{"\r", "j\n", " \x03", " \x1b", "\x1b[200~ \rdelete\n\x1b[201~\r", " \x1b[200~truncated\x03", " q"} {
		t.Run(keys, func(t *testing.T) {
			r, w := pickerPipe(t)
			if _, err := io.WriteString(w, keys); err != nil {
				t.Fatal(err)
			}
			// Leave w open. A standalone Esc must not wait for more input.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			ids, err := runDeletePicker(ctx, r, io.Discard, machines, func() (int, int) { return 80, 24 })
			if err != nil || len(ids) != 0 {
				t.Fatal("unexpected selection", ids, err)
			}
		})
	}
}

func TestNarrowDeletePickerKeepsWorkerIDsVisible(t *testing.T) {
	machines := []fleet.Machine{
		{Name: "t3fleet-thread-east-01", ID: "aaaa1111222200000000000000000000"},
		{Name: "t3fleet-thread-west-02", ID: "bbbb3333444400000000000000000000"},
	}
	view := deletePickerView(machines, []bool{false, false}, 0, 0, 2, 0, 32)
	for _, machine := range machines {
		if !strings.Contains(view, shortID(machine.ID)) {
			t.Fatal("worker ID was truncated in narrow terminal:", view)
		}
	}
}

func TestDeletePickerCancellationWithoutKey(t *testing.T) {
	r, _ := pickerPipe(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	ids, err := runDeletePicker(ctx, r, io.Discard, []fleet.Machine{{ID: "one"}}, func() (int, int) { return 80, 24 })
	if !errors.Is(err, context.DeadlineExceeded) || len(ids) != 0 {
		t.Fatal("expected cancellation with no selection", ids, err)
	}
}
