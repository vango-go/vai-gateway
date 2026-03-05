package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func writeAudioUnavailableWarning(w io.Writer, warned *bool, reason, message string) {
	if warned != nil && *warned {
		return
	}
	if warned != nil {
		*warned = true
	}
	if w == nil {
		w = os.Stderr
	}
	reason = strings.TrimSpace(reason)
	message = strings.TrimSpace(message)
	switch {
	case reason != "" && message != "":
		fmt.Fprintf(w, "audio unavailable (reason=%s): %s\n", reason, message)
	case reason != "":
		fmt.Fprintf(w, "audio unavailable (reason=%s)\n", reason)
	case message != "":
		fmt.Fprintf(w, "audio unavailable: %s\n", message)
	default:
		fmt.Fprintln(w, "audio unavailable")
	}
}

func writeToolExecutionMarker(out io.Writer, name string, closeOpenLines func()) {
	if isTalkToUserTool(name) {
		return
	}
	if out == nil {
		out = os.Stdout
	}
	if closeOpenLines != nil {
		closeOpenLines()
	}
	fmt.Fprintf(out, "[tool] %s\n", name)
}

func writeLabeledTextDelta(out io.Writer, lineOpen *bool, label, text string) {
	if text == "" {
		return
	}
	if out == nil {
		out = os.Stdout
	}
	if lineOpen != nil && !*lineOpen {
		fmt.Fprint(out, label)
		*lineOpen = true
	}
	fmt.Fprint(out, text)
}

func closeOpenLabeledLine(out io.Writer, lineOpen *bool) {
	if lineOpen == nil || !*lineOpen {
		return
	}
	if out == nil {
		out = os.Stdout
	}
	fmt.Fprintln(out)
	*lineOpen = false
}
