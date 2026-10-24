package agentui

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/invokr-ai/invokr/support/strutil"
	"github.com/invokr-ai/invokr/surface/ui"
	toolcore "github.com/invokr-ai/invokr/tools/core"

	"github.com/charmbracelet/x/ansi"
)

// blockRenderer renders the body of a settled tool block in the transcript.
// Summary is the collapsed form (the default); Detail is the expanded form a
// fold toggle reveals. Both compose on top of settledToolLine, which is where
// the outcome ledger's status flags (toolStatus/toolCode) become the glyph —
// renderers never infer success or failure from output text.
type blockRenderer interface {
	Summary(m message, width int) string
	Detail(m message, width int) string
}

// toolRenderers maps a tool name to its dedicated renderer. Unknown tools —
// toolbox scripts, MCP servers, anything not listed — fall back to the generic
// renderer, which is the pre-seam rendering unchanged.
var toolRenderers = map[string]blockRenderer{
	"shell": shellRenderer{}, "apply_patch": patchRenderer{}, "edit": patchRenderer{}, "todo": todoRenderer{},
	"git_read": gitReadRenderer{},
}

func rendererFor(name string) blockRenderer {
	return cmp.Or(toolRenderers[name], blockRenderer(genericRenderer{}))
}

// renderSettledBlock renders a finished (done/failed) tool block through the
// registry, honoring the block's fold state.
func renderSettledBlock(m message, width int) string {
	r := rendererFor(m.toolName)
	if m.expanded {
		return r.Detail(m, width)
	}
	return r.Summary(m, width)
}

// genericRenderer is the fallback: the tool line plus the failure sub-block —
// exactly what every tool block looked like before the seam existed.
type genericRenderer struct{}

func (genericRenderer) Summary(m message, width int) string {
	line := settledToolLine(m, width)
	if text := strings.TrimRight(m.toolOutput, "\n"); text != "" {
		line += "\n" + foldIndicator(strings.Count(text, "\n")+1, width)
	}
	return line
}

func (genericRenderer) Detail(m message, width int) string {
	return withOutputBlock(settledToolLine(m, width), m.toolOutput, width, 0)
}

// shellRenderer shows the command line (exit glyph from the ledger flags) and
// a short tail of the recorded output, so the result of a command is visible
// without unfolding it.
type shellRenderer struct{}

// shellTailLines is how many trailing output lines a collapsed shell block
// shows. The rest hides behind the fold indicator.
const shellTailLines = 5

func (shellRenderer) Summary(m message, width int) string {
	line := settledToolLine(m, width)
	tail, hidden := tailLines(shellText(m.toolOutput), shellTailLines)
	if hidden > 0 {
		line += "\n" + foldIndicator(hidden, width)
	}
	return withOutputBlock(line, strings.Join(tail, "\n"), width, 0)
}

func (shellRenderer) Detail(m message, width int) string {
	return withOutputBlock(settledToolLine(m, width), shellText(m.toolOutput), width, 0)
}

// shellText unwraps the shell tool's recorded JSON envelope into the streams a
// human wants to read — stdout then stderr. Anything that is not the envelope
// (e.g. an error payload) is shown as recorded.
func shellText(output string) string {
	var res struct {
		Stdout *string `json:"stdout"`
		Stderr string  `json:"stderr"`
	}
	if err := json.Unmarshal([]byte(output), &res); err != nil || res.Stdout == nil {
		return exitCodeLine.ReplaceAllString(output, "")
	}
	streams := []string{strings.TrimRight(*res.Stdout, "\n"), strings.TrimRight(res.Stderr, "\n")}
	return strings.Join(slices.DeleteFunc(streams, func(s string) bool { return s == "" }), "\n")
}

// exitCodeLine is the shell tool's "[exit code N]" fact for the model. The
// block's status line already shows the exit, so the human copy drops it.
var exitCodeLine = regexp.MustCompile(`(?m)\n?^\[exit code -?\d+\]$`)

// patchRenderer owns the diff sub-block under a writing tool: apply_patch
// records its patch text, edit rebuilds a patch-shaped diff from the call's
// arguments (see editedFileDiff), and both render through the same DiffView.
// Collapsed it keeps the maxDiffLines preview cap; expanded it shows the whole
// recorded diff.
type patchRenderer struct{}

func (patchRenderer) Summary(m message, width int) string {
	line := settledToolLine(m, width)
	if m.toolDiff == "" {
		return line
	}
	return line + "\n" + renderPatchDiff(m.toolDiff, width, maxDiffLines)
}

func (patchRenderer) Detail(m message, width int) string {
	line := settledToolLine(m, width)
	if m.toolDiff == "" {
		return withOutputBlock(line, m.toolOutput, width, 0)
	}
	return line + "\n" + renderPatchDiff(m.toolDiff, width, 0)
}

// gitReadRenderer renders git_read results. A diff/show envelope carries the
// full diff even when the model-facing copy was spilled (the completed event
// records the ungated output), so the human always sees the whole thing:
// collapsed shows the stat summary, expanded renders the diff through
// DiffView. Non-diff ops fall back to the plain output text.
type gitReadRenderer struct{}

func (gitReadRenderer) Summary(m message, width int) string {
	line := settledToolLine(m, width)
	stat, diff, plain := gitReadParts(m.toolOutput)
	if diff == "" {
		return withOutputBlock(line, plain, width, maxDiffLines)
	}
	if hidden := strings.Count(diff, "\n") + 1; hidden > 0 {
		line += "\n" + foldIndicator(hidden, width) + " " + ui.Invokr.Muted.Render("(diff)")
	}
	return withOutputBlock(line, stat, width, maxDiffLines)
}

func (gitReadRenderer) Detail(m message, width int) string {
	line := settledToolLine(m, width)
	_, diff, plain := gitReadParts(m.toolOutput)
	if diff == "" {
		return withOutputBlock(line, plain, width, 0)
	}
	return line + "\n" + renderGitDiff(diff, width, 0)
}

// gitReadParts unwraps a git_read envelope: {stat, diff} for diff/show,
// {output} for the text ops. Anything else renders as recorded.
func gitReadParts(output string) (stat, diff, plain string) {
	var res struct {
		Stat   string  `json:"stat"`
		Diff   *string `json:"diff"`
		Output *string `json:"output"`
	}
	err := json.Unmarshal([]byte(output), &res)
	if err != nil {
		// Result filters (tokf) may wrap the envelope in decoration lines;
		// retry on the first JSON object found in the text.
		err = json.Unmarshal([]byte(strutil.ExtractJSONObject(output)), &res)
	}
	if err != nil {
		return "", "", output
	}
	if res.Diff != nil {
		return res.Stat, *res.Diff, ""
	}
	if res.Output != nil {
		return "", "", *res.Output
	}
	return "", "", output
}

// todoRenderer renders the task list a todo call recorded, one checklist glyph
// per item, from the call's own arguments (the complete replacement list).
type todoRenderer struct{}

func (t todoRenderer) Summary(m message, width int) string {
	line := settledToolLine(m, width)
	items := todoItemsFromArgs(m.toolArgs)
	if len(items) == 0 {
		return line
	}
	rows := toolcore.MapSlice(items, func(item todoArgItem) string {
		return "    " + todoGlyph(item.Status) + " " + ui.Invokr.Muted.Render(ansi.Truncate(item.Task, max(1, width-8), "…"))
	})
	return line + "\n" + strings.Join(rows, "\n")
}

func (t todoRenderer) Detail(m message, width int) string { return t.Summary(m, width) }

type todoArgItem struct {
	Task   string `json:"task"`
	Status string `json:"status"`
}

func todoItemsFromArgs(argsJSON string) []todoArgItem {
	var fields struct {
		Items []todoArgItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &fields); err != nil {
		return nil
	}
	return fields.Items
}

// todoGlyph is the checklist mark per status: done green, in-progress the
// running blue, pending dim.
func todoGlyph(status string) string {
	switch status {
	case "done":
		return ui.Invokr.Success.Render(glyphOK)
	case "in_progress":
		return ui.Invokr.Info.Render("◐")
	default:
		return ui.Invokr.Muted.Render("○")
	}
}

// tailLines returns the last n lines of text plus how many were hidden.
func tailLines(text string, n int) ([]string, int) {
	if text = strings.TrimRight(text, "\n"); text == "" {
		return nil, 0
	}
	lines := strings.Split(text, "\n")
	n = min(n, len(lines))
	return lines[len(lines)-n:], len(lines) - n
}

// foldIndicator is the collapsed block's "there is more" marker.
func foldIndicator(hidden, width int) string {
	return "    " + ui.Invokr.Muted.Render(ansi.Truncate(fmt.Sprintf("▸ %d more lines", hidden), max(1, width-4), "…"))
}

// withOutputBlock appends an indented, muted output block under a tool line.
// maxLines caps it (0 = uncapped); each line is truncated to the pane width.
func withOutputBlock(line, output string, width, maxLines int) string {
	if output = strings.TrimRight(output, "\n"); output == "" {
		return line
	}
	return line + "\n" + strings.Join(cappedLines(output, width, maxLines, func(s string) string { return ui.Invokr.Muted.Render(s) }), "\n")
}

// cappedLines indents and styles each line of text, truncating to the pane
// width and capping the count (0 = uncapped) with a fold indicator.
func cappedLines(text string, width, maxLines int, style func(string) string) []string {
	lines := strings.Split(text, "\n")
	truncated := 0
	if maxLines > 0 && len(lines) > maxLines {
		truncated, lines = len(lines)-maxLines, lines[:maxLines]
	}
	for i, l := range lines {
		lines[i] = "    " + style(ansi.Truncate(l, max(1, width-4), "…"))
	}
	if truncated > 0 {
		lines = append(lines, foldIndicator(truncated, width))
	}
	return lines
}
