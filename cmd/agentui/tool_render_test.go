package agentui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRendererSelection(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want blockRenderer
	}{
		{tool: "shell", want: shellRenderer{}},
		{tool: "apply_patch", want: patchRenderer{}},
		{tool: "todo", want: todoRenderer{}},
		{tool: "web_search", want: genericRenderer{}},
		{tool: "mcp_github_list", want: genericRenderer{}},
		{tool: "some-toolbox-script", want: genericRenderer{}},
		{tool: "", want: genericRenderer{}},
	} {
		if got := rendererFor(tc.tool); got != tc.want {
			t.Fatalf("rendererFor(%q) = %T, want %T", tc.tool, got, tc.want)
		}
	}
}

// Golden for the shell renderer: command chip, exit glyph, output tail with a
// fold indicator counting the hidden lines.
func TestShellRendererGolden(t *testing.T) {
	m := message{
		role: "tool", toolName: "shell", toolStatus: toolDone,
		toolArgs:   `{"command":"ls"}`,
		toolOutput: "l1\nl2\nl3\nl4\nl5\nl6\nl7",
	}
	got := ansi.Strip(renderSettledBlock(m, 60))
	for _, want := range []string{glyphOK, "shell", "ls", "▸ 2 more lines", "l3", "l7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("shell summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "l2") {
		t.Fatalf("shell summary shows a line beyond the tail:\n%s", got)
	}
	detail := ansi.Strip(rendererFor("shell").Detail(m, 60))
	if !strings.Contains(detail, "l1") || strings.Contains(detail, "more lines") {
		t.Fatalf("shell detail does not show the full output:\n%s", detail)
	}
}

// Golden for the apply_patch renderer: colored diff body under the line,
// preview-capped collapsed and uncapped expanded.
func TestPatchRendererGolden(t *testing.T) {
	diff := strings.TrimSuffix(strings.Repeat("+line\n", maxDiffLines+3), "\n")
	m := message{role: "tool", toolName: "apply_patch", toolStatus: toolDone, toolDiff: diff}
	got := ansi.Strip(renderSettledBlock(m, 60))
	if !strings.Contains(got, "3 more lines") {
		t.Fatalf("collapsed patch not capped at maxDiffLines:\n%s", got)
	}
	detail := ansi.Strip(rendererFor("apply_patch").Detail(m, 60))
	if strings.Contains(detail, "more lines") {
		t.Fatalf("expanded patch still capped:\n%s", detail)
	}
	if strings.Count(detail, "+line") != maxDiffLines+3 {
		t.Fatalf("expanded patch missing lines:\n%s", detail)
	}
}

// Golden for the todo renderer: one checklist glyph per item by status.
func TestTodoRendererGolden(t *testing.T) {
	m := message{
		role: "tool", toolName: "todo", toolStatus: toolDone,
		toolArgs: `{"items":[{"task":"read","status":"done"},{"task":"write","status":"in_progress"},{"task":"lint","status":"pending"}]}`,
	}
	got := ansi.Strip(renderSettledBlock(m, 60))
	for _, want := range []string{glyphOK + " read", "◐ write", "○ lint"} {
		if !strings.Contains(got, want) {
			t.Fatalf("todo checklist missing %q:\n%s", want, got)
		}
	}
}

// An unknown tool must render byte-identically to the pre-seam generic path.
func TestGenericRendererUnchanged(t *testing.T) {
	m := message{
		role: "tool", toolName: "custom_tool", toolStatus: toolFailed,
		toolArgs: `{"query":"x"}`, toolError: "boom", toolOutput: "should not show collapsed",
	}
	got := ansi.Strip(renderSettledBlock(m, 60))
	want := ansi.Strip(settledToolLine(m, 60)) + "\n" + ansi.Strip(foldIndicator(1, 60))
	if got != want {
		t.Fatalf("generic summary diverged from line+indicator:\n%q\nwant\n%q", got, want)
	}
	if !strings.Contains(got, "boom") {
		t.Fatalf("failure detail lost: %s", got)
	}
	if strings.Contains(got, "should not show") {
		t.Fatalf("collapsed generic block leaked output: %s", got)
	}
	// A tool with no recorded output stays exactly the pre-seam line.
	m.toolOutput = ""
	if got := ansi.Strip(renderSettledBlock(m, 60)); got != ansi.Strip(settledToolLine(m, 60)) {
		t.Fatalf("no-output generic block gained chrome:\n%q", got)
	}
}

// TestShellTextDropsExitCodeFact: the status line carries the exit, so the
// output block must not repeat the tool's "[exit code N]" line. The command's
// own echo of its status stays.
func TestShellTextDropsExitCodeFact(t *testing.T) {
	got := shellText("FAIL ./calc\nexit: 1\n[exit code 1]\n[cwd sub]")
	if strings.Contains(got, "[exit code") || !strings.Contains(got, "exit: 1") || !strings.Contains(got, "[cwd sub]") {
		t.Fatalf("shellText = %q", got)
	}
}
