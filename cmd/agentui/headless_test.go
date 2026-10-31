package agentui

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestHeadlessCommands(t *testing.T) {
	var names []string
	for _, c := range HeadlessCommands() {
		names = append(names, c.Name)
	}
	for _, want := range []string{"usage", "help", "jobs", "model"} {
		if !slices.Contains(names, want) {
			t.Errorf("headless commands %v lack %q", names, want)
		}
	}
	for _, tuiOnly := range []string{"mode", "clear", "logs", "recorded", "settings", "memory"} {
		if slices.Contains(names, tuiOnly) {
			t.Errorf("headless commands offer TUI-only %q", tuiOnly)
		}
	}
	if help, _, _ := RunHeadless(context.Background(), nil, nil, "/help"); !strings.Contains(help, "/usage — show token usage") || strings.Contains(help, "/logs") {
		t.Errorf("headless /help = %q", help)
	}
	for _, text := range []string{"/jobs run nightly", "/skills install x"} {
		if reply, _, handled := RunHeadless(context.Background(), nil, nil, text); !handled || !strings.Contains(reply, "takes no arguments") {
			t.Errorf("%s = %q, %v; want refused", text, reply, handled)
		}
	}
	if _, _, handled := RunHeadless(context.Background(), nil, nil, "/logs"); handled {
		t.Error("RunHeadless handled the TUI-only /logs")
	}
	if _, _, handled := RunHeadless(context.Background(), nil, nil, "/nosuch x"); handled {
		t.Error("RunHeadless handled an unknown command")
	}
	if reply, _, handled := RunHeadless(context.Background(), nil, nil, "/jobs"); !handled || reply != "command unavailable in this runtime" {
		t.Errorf("/jobs without a backend = %q, %v", reply, handled)
	}
}
