package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// hintError separates what went wrong from the next action to take. Error()
// keeps both on one line for logs and tests; the CLI renders them on two.
type hintError struct {
	msg  string
	hint string
	code string // daemon error code, when the error came from the daemon
}

func (e *hintError) Error() string {
	if e.hint == "" {
		return e.msg
	}
	return e.msg + "; " + e.hint
}

func hinted(hint, format string, args ...any) error {
	return &hintError{msg: fmt.Sprintf(format, args...), hint: hint}
}

func hasDaemonCode(err error, code string) bool {
	var h *hintError
	return errors.As(err, &h) && h.code == code
}

func printError(w io.Writer, err error, color bool) {
	label := "Error:"
	if color {
		label = "\x1b[1;31mError:\x1b[0m"
	}
	var h *hintError
	if errors.As(err, &h) {
		fmt.Fprintln(w, label, h.msg)
		if h.hint != "" {
			hintLabel := "Hint: "
			if color {
				hintLabel = "\x1b[2mHint: \x1b[0m"
			}
			fmt.Fprintln(w, hintLabel, h.hint)
		}
		return
	}
	message := err.Error()
	fmt.Fprintln(w, label, strings.TrimRight(message, "\n"))
	if strings.HasPrefix(message, "unknown command") {
		fmt.Fprintln(w, "Run 't3fleet --help' to list commands.")
	}
}
