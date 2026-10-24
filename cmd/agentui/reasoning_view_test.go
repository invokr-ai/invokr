package agentui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/invokr-ai/invokr/platform/events"
)

func reasoningEvent(text string) events.Event {
	return events.Event{Type: events.EventReasoning, Reasoning: &events.ReasoningPayload{Text: text}}
}

// TestReasoningShowsBetweenToolCalls: a step's reasoning lands in the log in
// full, ahead of the tool block it led to.
func TestReasoningShowsBetweenToolCalls(t *testing.T) {
	var c chatLog
	long := strings.Repeat("check the branch before pushing ", 20)
	c.ApplyEvent(reasoningEvent("  " + long + "\n"))
	c.ApplyEvent(events.Event{Type: events.EventToolStarted, Tool: &events.ToolPayload{ID: "1", Name: "git_read"}})
	if c.Len() != 2 || c.messages[0].role != "reasoning" || c.messages[1].toolName != "git_read" {
		t.Fatalf("messages = %+v, want reasoning then the tool", c.messages)
	}
	var b strings.Builder
	writeMessage(&b, c.messages[0], 60, "")
	rendered := ansi.Strip(b.String())
	if got := strings.Join(strings.Fields(strings.ReplaceAll(rendered, "│", "")), " "); !strings.Contains(got, strings.TrimSpace(long)) {
		t.Fatalf("reasoning was cut: %q", got)
	}
	// It reads as thinking, not as the answer: a label, and a gutter on every line.
	lines := strings.Split(rendered, "\n")
	if strings.TrimSpace(lines[0]) != "thinking" {
		t.Fatalf("first line = %q, want the thinking label", lines[0])
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(strings.TrimLeft(line, " "), "│ ") {
			t.Fatalf("line without the thinking gutter: %q", line)
		}
	}
}

// TestReasoningGoesAboveStreamedAnswer: on a final turn the answer streams
// first and the reasoning arrives after, but it still reads above the answer.
func TestReasoningGoesAboveStreamedAnswer(t *testing.T) {
	var c chatLog
	c.ApplyEvent(events.Event{Type: events.EventAssistantDelta, Delta: &events.AssistantDeltaPayload{Text: "Done."}})
	c.ApplyEvent(reasoningEvent("wrap up"))
	if c.Len() != 2 || c.messages[0].role != "reasoning" || c.messages[1].text != "Done." || !c.messages[1].streaming {
		t.Fatalf("messages = %+v, want reasoning above the streaming answer", c.messages)
	}
}

func TestBlankReasoningIsNotAToolEvent(t *testing.T) {
	if isToolEvent(reasoningEvent(" \n")) || !isToolEvent(reasoningEvent("x")) {
		t.Fatal("blank reasoning must be dropped, non-blank kept")
	}
}

// TestNoticesHaveTheirOwnLook: invokr's own lines (mode changes, approval
// decisions) carry a marker, not the thinking gutter.
func TestNoticesHaveTheirOwnLook(t *testing.T) {
	var b strings.Builder
	writeMessage(&b, message{role: "tool", text: "mode: auto (future calls; running work keeps its permissions)"}, 80, "")
	if got := ansi.Strip(b.String()); !strings.HasPrefix(got, "• mode: auto") || strings.Contains(got, "│") {
		t.Fatalf("notice = %q", got)
	}
}
