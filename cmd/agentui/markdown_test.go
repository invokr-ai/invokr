package agentui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/invokr-ai/invokr/surface/ui"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderMarkdownDropsSyntaxCharacters(t *testing.T) {
	const width = 80
	source := "## Findings\n\n> *To be, or not to be*\n\n" +
		"| File | Type |\n|---|---|\n| `main.go` | file |\n\n" +
		"- first\n- second\n"
	out := ansi.Strip(renderMarkdown(source, width))

	for _, want := range []string{"Findings", "To be, or not to be", "main.go", "first", "second"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"## ", "> *", "|---|", "`main.go`"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("raw markdown syntax %q survived:\n%s", unwanted, out)
		}
	}
	// The quote keeps the design's left rule in place of the ">" marker.
	if !strings.Contains(out, "│ To be") {
		t.Fatalf("block quote lost its left rule:\n%s", out)
	}
	assertLinesWithinWidth(t, renderMarkdown(source, width), width)
}

func TestRenderMarkdownFallsBackToRawText(t *testing.T) {
	if got := renderMarkdown("plain", 0); got != "plain" {
		t.Fatalf("zero width = %q, want the raw text", got)
	}
	if got := renderMarkdown("   ", 40); got != "" {
		t.Fatalf("blank input = %q, want empty", got)
	}
}

func TestStreamingAnswerRendersAsItArrives(t *testing.T) {
	// Settled blocks go through glamour while the answer is still arriving, and
	// the unfinished fence is rendered too rather than showing its syntax.
	log := newChatLog(progressAll)
	log.Append(message{role: "assistant", text: "## Head\n\n```go\nfunc ma", streaming: true})
	partial := ansi.Strip(log.Content(60, ""))
	if strings.Contains(partial, "## Head") || strings.Contains(partial, "```") {
		t.Fatalf("streaming answer kept raw syntax: %q", partial)
	}
	if !strings.Contains(partial, "Head") || !strings.Contains(partial, "func ma") {
		t.Fatalf("streaming answer lost text: %q", partial)
	}

	// Settling the message re-renders it through glamour.
	log.messages[0].text = "## Head\n\n```go\nfunc main() {}\n```"
	log.messages[0].streaming = false
	settled := ansi.Strip(log.Content(60, ""))
	if strings.Contains(settled, "## Head") || strings.Contains(settled, "```") {
		t.Fatalf("settled answer kept raw syntax: %q", settled)
	}
}

// Fenced code arrived uniformly gray once, from a stylesheet name chroma had no
// style for. That particular cause is now caught upstream — tuikit corrects an
// unknown name — so this guards the symptom rather than the cause: distinct
// tokens must not all share one color, whatever turns the colors off.
func TestRenderMarkdownHighlightsFencedCode(t *testing.T) {
	out := renderMarkdown("```go\nfunc greet(n int) string { return \"hi\" }\n```", 60)
	colors := map[string]bool{}
	for _, m := range regexp.MustCompile(`\x1b\[38;5;\d+m`).FindAllString(out, -1) {
		colors[m] = true
	}
	if len(colors) < 3 {
		t.Fatalf("fenced code used %d colors, want several:\n%q", len(colors), out)
	}
}

// A palette change has to reach the markdown: the renderer captures the palette
// when it is built and the streamer caches what it has drawn, so both are
// replaced rather than told about the change. Without that, an answer already on
// screen keeps the colors of the palette it was first drawn in, forever.
func TestResetMarkdownFollowsThePalette(t *testing.T) {
	original := ui.Invokr
	t.Cleanup(func() { ui.Invokr = original; resetMarkdown() })

	const code = "```go\nfunc main() {}\n```"
	ui.Invokr = ui.NewTheme(true)
	resetMarkdown()
	dark := streamer.Render(code, 60)

	ui.Invokr = ui.NewTheme(false)
	resetMarkdown()
	if light := streamer.Render(code, 60); light == dark {
		t.Fatal("the rendered code did not follow the palette")
	}
}

// Applying a theme has to reach the rendered code, and "auto" has to go back to
// following the terminal rather than pinning the literal string.
func TestSetSyntaxThemeChangesRenderedCode(t *testing.T) {
	t.Cleanup(func() { setSyntaxTheme("") })

	const code = "```go\nfunc main() {}\n```"
	setSyntaxTheme("")
	auto := renderMarkdown(code, 60)

	setSyntaxTheme("catppuccin-latte")
	picked := renderMarkdown(code, 60)
	if picked == auto {
		t.Fatal("picking a stylesheet did not change the rendered code")
	}

	setSyntaxTheme("")
	if renderMarkdown(code, 60) != auto {
		t.Fatal("clearing the stylesheet did not restore the automatic choice")
	}
}

// TestCodeBlockKeepsTabIndentation: a tab-indented Go body must not print
// flush left (R8).
func TestCodeBlockKeepsTabIndentation(t *testing.T) {
	got := ansi.Strip(renderMarkdown("```go\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```", 80))
	if !strings.Contains(got, "    return a * b") {
		t.Fatalf("code block = %q, want the body indented", got)
	}
}
