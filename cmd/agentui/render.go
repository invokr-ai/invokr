package agentui

import (
	"cmp"
	"encoding/json"
	"fmt"
	"image/color"
	"runtime/debug"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	humanize "github.com/dustin/go-humanize"

	"github.com/antonikliment/tuikit"
	"github.com/invokr-ai/invokr/capability/approval"
	"github.com/invokr-ai/invokr/constants"
	"github.com/invokr-ai/invokr/platform/events"
	"github.com/invokr-ai/invokr/platform/turn"
	"github.com/invokr-ai/invokr/platform/version"
	"github.com/invokr-ai/invokr/surface/ui"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// rule is the accent bar that marks a turn or a banner as belonging to the
// agent, standing in for the design's left border. It is empty in the current
// style, so nothing paints it.
const rule = ""

// renderIntro draws the startup banner: the wordmark and the build badge, and
// nothing that is already on screen. The model rides the statusline under the
// input, and the keys ride the footer above it, so a banner repeating either
// would only push the transcript down.
// logoMinWidth is how wide the terminal needs to be before renderIntro
// swaps the compact "INVOKR" gradient text for the wider Logo() wordmark —
// wide enough to fit the logo plus the badges that follow it on the same
// line.
const logoMinWidth = 70

// logoMinHeight is the matching vertical budget: on a short terminal the
// multi-line wordmark would eat most of the visible transcript, so the compact
// one-line gradient stands in for it.
const logoMinHeight = 20

func renderIntro(width, height int) string {
	brand := ui.Invokr.GradientText("Invokr")
	if width >= logoMinWidth && height >= logoMinHeight {
		brand = ui.Invokr.Logo()
	}
	meta := ui.Invokr.Muted.Render("agent started") + "  " + ui.Invokr.Pill(ui.Invokr.Info, buildVersion())
	// Two columns joined at their vertical centers: the logo can grow as tall as it
	// likes without pushing the badges onto a line of their own.
	head := lipgloss.JoinHorizontal(lipgloss.Center, brand, "  ", wrapText(meta, max(0, width-lipgloss.Width(brand)-2)))
	// A gradient rule closes the header off from the transcript below it.
	return head + "\n" + ui.BlendText(strings.Repeat("─", max(1, width)), ui.Invokr.Palette.Brand, ui.Invokr.Palette.Blue)
}

// versionBadge is the header badge: the build stamp, plus a muted hint when the
// update checker reports a newer release. The hint is the literal command that
// fetches it, so the user does not have to look up how to act on it. The
// checker never blocks, so the hint appears on a later render, once its
// background fetch has landed.
func versionBadge(stamp, update string) string {
	if update == "" {
		return stamp
	}
	return stamp + " · " + ui.Invokr.Muted.Render("run invokr update "+update)
}

// buildVersion is the badge text, including the update hint when the checker
// reports a newer release than this build.
func buildVersion() string {
	return versionBadge(versionStamp(), version.LatestVersion())
}

// versionStamp is the build identity: a stamped build shows its tag, commit and
// date; an unstamped one shows "dev".
func versionStamp() string {
	// A dev/release build stamped through -ldflags (see the Makefile's install
	// and dev targets) knows its commit and build date; that beats anything
	// debug.ReadBuildInfo can tell us, so show it verbatim.
	if version.Commit != "none" {
		// An untagged build stamps the same short sha into both fields, since
		// `git describe --always` falls back to exactly what `rev-parse --short`
		// prints. Saying it twice is noise; a real tag still shows both.
		stamp := version.Version
		if !strings.HasPrefix(version.Version, version.Commit) {
			stamp += " · " + version.Commit
		}
		return stamp + " · " + version.Date
	}
	info, ok := debug.ReadBuildInfo()
	// An untagged build stamps a pseudo-version ("v0.0.0-20260808202732-75baf45…"),
	// which is 40 characters of noise in a banner badge. Only a real tag is worth
	// showing; everything else is "dev".
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" || strings.HasPrefix(info.Main.Version, "v0.0.0-") {
		return "dev"
	}
	return info.Main.Version
}

// renderPrompt draws the caret marker. It uses the brand color rather than the
// frame color so it stays legible inside the input box, where the frame color
// is already spent on the border.
func renderPrompt() string { return ui.Invokr.Accent.Render("›") + " " }

// responseMeta is the per-response provenance line under an answer: which model
// produced it, how long it took, and how many tokens it cost. Every field is
// optional — the plain renderer has no elapsed time, and a provider may not
// report tokens.
type responseMeta struct {
	model   string
	elapsed time.Duration
	tokens  int64
}

func renderResponseWithUsage(resp string, usage turn.TokenUsage, width int) string {
	if resp = strings.TrimSpace(resp); resp == "" {
		return ""
	}
	return renderResponseHeader(width) + "\n" + renderMarkdown(resp, width) + renderPlainUsage(usage, width)
}

// renderPlainUsage is the non-TUI renderer's context line. The TUI has no need
// for it — its status line carries context continuously — so it is not part of
// the transcript's response footer.
func renderPlainUsage(usage turn.TokenUsage, width int) string {
	line := "usage unavailable"
	if usage.Available {
		line = contextUsageLine(usage)
	}
	return "\n" + wrapText(ui.Invokr.Muted.Render(line), width)
}

func renderResponse(resp string, meta responseMeta, width int) string {
	if resp = strings.TrimSpace(resp); resp == "" {
		return ""
	}
	return renderMarkdown(resp, width) + renderResponseFooter(meta, width)
}

// renderResponseHeader exists for the plain streaming renderer, which prints a
// marker before the answer starts because it cannot redraw in place.
func renderResponseHeader(width int) string {
	return wrapText(ui.Invokr.Muted.Render("response"), width)
}

// silentServerAfter is how long a turn may go without any output before the
// thinking line says the model server has not replied: long enough for a
// local model's prompt processing, short enough that a dead server is not
// mistaken for a slow one.
const silentServerAfter = 30 * time.Second

func renderThinking(frame string, since, heard time.Time, width int) string {
	if !heard.IsZero() && time.Since(heard) >= silentServerAfter {
		line := ui.Invokr.Warn.Render(frame + " waiting for the model server")
		return wrapText(line+"   "+ui.Invokr.Muted.Render("no output for "+formatDuration(time.Since(heard))+" · esc to stop"), width)
	}
	return renderRunning(frame, "thinking", ui.Invokr.Info, since, width)
}

// renderRunning is renderThinking's shape with a caller-chosen label and style,
// so a turn blocked on approval says so instead of claiming to think.
func renderRunning(frame, label string, style lipgloss.Style, since time.Time, width int) string {
	line := style.Render(frame + " " + label)
	if since.IsZero() {
		return line
	}
	return wrapText(line+"   "+ui.Invokr.Muted.Render(formatDuration(time.Since(since))+" · esc to interrupt"), width)
}

// renderResponseFooter renders "ornith-9b · 6.2s · 412 tok" under an answer,
// or nothing when the turn reported none of the three.
func renderResponseFooter(meta responseMeta, width int) string {
	parts := make([]string, 0, 3)
	if model := strings.TrimSpace(meta.model); model != "" {
		parts = append(parts, model)
	}
	if meta.elapsed > 0 {
		parts = append(parts, formatDuration(meta.elapsed))
	}
	if meta.tokens > 0 {
		parts = append(parts, humanize.Comma(meta.tokens)+" tok")
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n" + wrapText(ui.Invokr.Muted.Render(strings.Join(parts, " · ")), width)
}

// renderToolBlock renders a single live tool block from a chat message. While
// running it shows the spinner frame + live elapsed; once done/failed it shows a
// status glyph + duration and an optional [code] / [error] suffix.
func renderToolBlock(m message, spinnerFrame string, width int) string {
	preview := primaryArg(m.toolName, m.toolArgs)
	if m.toolStatus == toolRunning {
		elapsed := "running " + formatDuration(time.Since(m.toolStartedAt)) + " · esc stop"
		if m.toolProgressLine != "" {
			elapsed = m.toolProgressLine + " · " + elapsed
		}
		return renderToolLineWidth(spinnerFrame, m.toolName, preview, elapsed, toolRunning, width)
	}
	return renderSettledBlock(m, width)
}

// settledToolLine is the shared head of every settled block: the outcome glyph
// (from the ledger's status flags, never renderer inference), the tool line,
// and the failure sub-block. Per-tool renderers compose their bodies under it.
func settledToolLine(m message, width int) string {
	preview := primaryArg(m.toolName, m.toolArgs)
	state, lead := toolDone, glyphOK
	// A non-empty code is "completed but flagged" — the model still saw the
	// output, but the UI marks it as a failure (see core.ToolError.Payload).
	if m.toolStatus == toolFailed || m.toolCode != "" {
		state, lead = toolFailed, glyphFail
	}
	// A cancelled call is settled, not broken: it keeps the failed styling but
	// says so with its own glyph and label.
	if isCancelledTool(m) {
		lead = glyphStop
	}
	line := renderToolLineWidth(lead, m.toolName, preview, toolTrailing(m, state), state, width)
	if detail := toolFailureDetail(m); state == toolFailed && detail != "" {
		line += "\n" + renderToolDetail(detail, width)
	}
	return line
}

// toolTrailing is the right-aligned metadata cell: "65ms · 3.1 KB" for a call
// that worked, "0.3s · failed" for one that did not — the reason moves to the
// sub-block below rather than being crammed into brackets here.
func toolTrailing(m message, state toolStatus) string {
	dur := formatDuration(m.toolDuration)
	switch {
	case isCancelledTool(m):
		return dur + " · cancelled"
	case state == toolFailed:
		return dur + " · failed"
	case m.toolBytes > 0:
		return dur + " · " + formatBytes(int64(m.toolBytes))
	}
	return dur
}

// isCancelledTool reports whether a settled block is one the user interrupted,
// which the runner marks with the canceled code (see agent.ToolCodeCanceled).
func isCancelledTool(m message) bool {
	return m.toolStatus != toolRunning && strings.TrimSpace(m.toolCode) == "canceled"
}

// toolFailureDetail is what the sub-block says: the error text, or the
// classification code when the tool reported nothing more specific.
func toolFailureDetail(m message) string {
	detail, code := strings.TrimSpace(m.toolError), strings.TrimSpace(m.toolCode)
	switch {
	case detail == "":
		return code
	case code == "":
		return detail
	}
	return code + " — " + detail
}

// maxToolDetailLines caps a failure sub-block so a stack trace in an error
// string cannot flood the transcript. The full text is in the logs pane.
const maxToolDetailLines = 3

// renderToolDetail is the design's error sub-block: the failure reason under
// the tool line, indented and marked with a red rule.
func renderToolDetail(detail string, width int) string {
	const indent = "   "
	body := wrapText(strings.Join(strings.Fields(detail), " "), max(1, width-len(indent)-2))
	lines := strings.Split(body, "\n")
	if len(lines) > maxToolDetailLines {
		lines = lines[:maxToolDetailLines]
		lines[maxToolDetailLines-1] = ansi.Truncate(lines[maxToolDetailLines-1], max(1, width-len(indent)-3), "") + "…"
	}
	for i, line := range lines {
		lines[i] = indent + ui.Invokr.Danger.Render("│ ") + ui.Invokr.Muted.Render(line)
	}
	return strings.Join(lines, "\n")
}

// maxDiffLines caps an inline edit diff so a large patch cannot flood the
// transcript. ponytail: a hard cap, not a scrollable pane — expand only if
// users ask to read long diffs in place.
const maxDiffLines = 40

// appliedPatchDiff pulls the unified diff out of an apply_patch call's
// arguments, which already carry the full patch text on the tool event. Dry
// runs change nothing, so they get no diff.
func appliedPatchDiff(name, argsJSON string) string {
	if name != "apply_patch" {
		return ""
	}
	var fields struct {
		Patch  string `json:"patch"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &fields); err != nil || fields.DryRun {
		return ""
	}
	return strings.TrimSpace(fields.Patch)
}

// editCallOp is one replacement a settled edit call carried in its arguments.
type editCallOp struct {
	Old string `json:"oldText"`
	New string `json:"newText"`
}

// diffLines is one side of a rebuilt hunk: text prefixed line by line with
// mark, no line at all for empty text. A delete carries an empty newText:
// nothing to render as removed.
func diffLines(text, mark string) []string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	for i, line := range lines {
		// A CRLF file carries the \r at the end of every line; it would render ^M.
		lines[i] = mark + strings.TrimSuffix(line, "\r")
	}
	return lines
}

// editedFileDiff rebuilds an edit's diff from the call's arguments — the oldText/newText
// pairs, or the whole content for a write — as apply_patch text, so it renders through the
// same DiffView as apply_patch. Gutter numbers count within the fragment, not the file.
func editedFileDiff(name, argsJSON string) string {
	if name != "edit" {
		return ""
	}
	var fields struct {
		Path    string       `json:"path"`
		Edits   []editCallOp `json:"edits"`
		Content *string      `json:"content"`
		Deleted bool         `json:"delete"`
		DryRun  bool         `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &fields); err != nil || fields.DryRun {
		return ""
	}
	if fields.Path == "" || fields.Deleted {
		return ""
	}
	// A full file write has no before text to pair against, and apply_patch labels
	// such a section "Add" rather than "Update".
	written := fields.Content != nil
	ops := fields.Edits
	if written {
		ops = []editCallOp{{New: *fields.Content}}
	}
	sections := make([]string, 0, 2*len(ops)+1)
	for _, op := range ops {
		sections = append(sections, "@@")
		sections = append(sections, diffLines(op.Old, "-")...)
		sections = append(sections, diffLines(op.New, "+")...)
	}
	// One "@@" per op survives when every op changed nothing.
	if len(sections) == len(ops) {
		return ""
	}
	header := "Update"
	if written {
		header = "Add"
	}
	return "*** Begin Patch\n*** " + header + " File: " + fields.Path +
		"\n" + strings.Join(sections, "\n") + "\n*** End Patch"
}

// textEventLine is the plain line for a notice or reasoning event.
func textEventLine(ev events.Event) (string, bool) {
	switch {
	case ev.Type == events.EventNotice && ev.Notice != nil:
		return ev.Notice.Text, true
	case ev.Type == events.EventReasoning && ev.Reasoning != nil:
		return ui.Invokr.Muted.Render(strings.TrimSpace(ev.Reasoning.Text)), true
	}
	return "", false
}

// renderToolEvent renders one tool event as a standalone line for the plain
// (non-TUI) streaming renderer, which can't update in place.
func renderToolEvent(ev events.Event) string {
	if line, ok := textEventLine(ev); ok {
		return line
	}
	if ev.Tool == nil {
		return fmt.Sprintf("tool unknown event %s", ev.Type)
	}
	t := *ev.Tool
	preview := primaryArg(t.Name, t.Arguments)
	switch ev.Type {
	case events.EventToolStarted:
		return renderToolLine(glyphRun, t.Name, preview, "running...", toolRunning)
	case events.EventToolProgress:
		return renderToolLine(glyphRun, t.Name, preview, t.Progress, toolRunning)
	case events.EventToolCompleted:
		dur := formatDuration(t.Duration)
		if t.Code != "" {
			return renderToolLine(glyphFail, t.Name, preview, dur+" ["+t.Code+"]", toolFailed)
		}
		return renderToolLine(glyphOK, t.Name, preview, dur+" · "+formatBytes(int64(t.OutputBytes)), toolDone)
	case events.EventToolFailed:
		code := t.Code
		if code == "denied" {
			reason := strings.TrimPrefix(t.Error, "approval denied for "+t.Name+": ")
			label := "denied: "
			if strings.HasPrefix(reason, "standing denial ") {
				label = "auto-denied: "
			}
			return renderToolLine(glyphFail, t.Name, preview, label+reason, toolFailed)
		}
		if code == "" {
			code = trimErr(t.Error)
		}
		return renderToolLine(glyphFail, t.Name, preview, formatDuration(t.Duration)+" ["+code+"]", toolFailed)
	default:
		return renderToolLine(glyphRun, t.Name, preview, string(ev.Type), toolRunning)
	}
}

func trimErr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "error"
	}
	if r := []rune(s); len(r) > 40 {
		return string(r[:37]) + "..."
	}
	return s
}

func renderApprovalQueuePanel(items []approval.QueueItem, cursor int, expanded bool, detailOffset int, keys keyMap, hints help.Model, width int) string {
	inner := max(1, width-6) // margin + border + padding, as inputBox
	var b strings.Builder
	b.WriteString(renderApprovalHeader(len(items), cursor) + "\n")
	for i, item := range items {
		b.WriteString(renderQueueItemLine(item, i == cursor, expanded && i == cursor, detailOffset, inner) + "\n")
		if i == cursor && i < len(items)-1 {
			b.WriteString("\n")
		}
	}
	bindings := []key.Binding{keys.ApproveItem}
	if pattern, err := grantPattern(items[min(cursor, len(items)-1)].Pending.Request); err == nil {
		always := keys.AlwaysAllow
		always.SetHelp("a", fmt.Sprintf("always %q", pattern))
		bindings = append(bindings, always)
	}
	bindings = append(bindings, keys.DenyItem, keys.ExpandItem)
	if len(items) > 1 {
		bindings = append(bindings, keys.ApproveAll, keys.DenyAll)
	}
	if expanded {
		bindings = append(bindings, keys.PageUp, keys.PageDn)
	}
	hints.ShortSeparator = "  "
	b.WriteString("\n" + wrapText(hints.ShortHelpView(bindings), inner))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Invokr.Palette.Yellow).Padding(0, 1).Width(max(20, width-2))
	return box.Render(b.String())
}

// renderApprovalHeader is "APPROVAL", plus "2 of 3" and a pip per queued item
// (settled green, current amber, rest dim) once there is more than one.
func renderApprovalHeader(total, cursor int) string {
	head := ui.Invokr.Warn.Bold(true).Render("APPROVAL")
	if total < 2 {
		return head
	}
	pipStyles := []lipgloss.Style{ui.Invokr.Success, ui.Invokr.Warn, ui.Invokr.Muted}
	pips := make([]string, total)
	for i := range total {
		pips[i] = pipStyles[cmp.Compare(i, cursor)+1].Render("━━")
	}
	return head + "  " + ui.Invokr.Muted.Render(fmt.Sprintf("%d of %d", cursor+1, total)) + "  " + strings.Join(pips, " ")
}

// renderQueueItemLine is the selected request — headline, the command in bold,
// why it asks in dim, or the details when expanded — or a dim neighbour row.
func renderQueueItemLine(item approval.QueueItem, selected, expanded bool, detailOffset, width int) string {
	req := item.Pending.Request
	if !selected {
		return renderQueueNeighbour(req, width)
	}
	lines := []string{"", renderApprovalHeadline(req), ""}
	subject := strings.TrimSpace(req.Subject)
	for _, line := range elideSubject(subject, max(1, width-4), approvalSubjectLines) {
		lines = append(lines, "    "+ui.Invokr.Text.Bold(true).Render(line))
	}
	// "Run <subject>" is the subject again with a verb in front.
	if why := approvalWhy(req); why != "" && why != "Run "+subject && !expanded {
		for _, line := range wrapLines(why, max(1, width-4)) {
			lines = append(lines, "    "+ui.Invokr.Muted.Render(line))
		}
	}
	if expanded {
		details := approvalDetailLines(req, width)
		start := min(max(0, detailOffset), max(0, len(details)-approvalDetailHeight))
		end := min(len(details), start+approvalDetailHeight)
		for _, line := range details[start:end] {
			lines = append(lines, "    "+renderApprovalDetailLine(line))
		}
		if len(details) > approvalDetailHeight {
			lines = append(lines, "    "+ui.Invokr.Muted.Render(fmt.Sprintf("details %d-%d/%d", start+1, end, len(details))))
		}
	}
	return strings.Join(lines, "\n")
}

// renderApprovalHeadline is the design's selected-request line: the tool in
// body text, its tier on a pill rather than in square brackets.
func renderApprovalHeadline(req approval.Request) string {
	head := ui.Invokr.Accent.Render("► ") + ui.Invokr.Text.Bold(true).Render(req.Tool)
	if tier := strings.TrimSpace(req.Tier); tier != "" {
		// execute_write_command → write: the pill names the risk, not the enum.
		tier = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(tier, "execute_"), "_command"), "_", " ")
		head += "  " + ui.Invokr.Pill(ui.Invokr.Warn, tier)
	}
	return head
}

// renderQueueNeighbour is a request that is not the one being decided: a single
// dim row, enough to see what else is queued without competing with the
// selected request's detail.
func renderQueueNeighbour(req approval.Request, width int) string {
	line := req.Tool
	if why := approvalWhy(req); why != "" {
		line += "  " + strings.Join(strings.Fields(why), " ")
	}
	return "    " + ui.Invokr.Muted.Render(ansi.Truncate(line, max(1, width-4), "…"))
}

// approvalSubjectLines is how many lines of the command the collapsed row
// spends before deferring to the details view.
const approvalSubjectLines = 3

// elideSubject breaks a command line at its pipeline boundaries so a long
// pipeline reads as one stage per line instead of wrapping mid-token, then
// caps the result — the full text stays in the details view.
func elideSubject(subject string, width, maxLines int) []string {
	lines := []string{}
	for i, segment := range splitPipeline(subject) {
		if i > 0 {
			segment = "  " + segment
		}
		lines = append(lines, wrapLines(segment, width)...)
	}
	if len(lines) > maxLines {
		lines = append(lines[:maxLines-1:maxLines-1], "… (v for full)")
	}
	return lines
}

// splitPipeline cuts after each `|`, `&&` or `;`, keeping the operator on the
// line it ends. Quoting is not tracked: a boundary inside a quoted string only
// costs a line break in a display string.
func splitPipeline(subject string) []string {
	segments := []string{}
	start := 0
	for i := 0; i < len(subject); i++ {
		width := 0
		switch {
		case strings.HasPrefix(subject[i:], "&&"), strings.HasPrefix(subject[i:], "||"):
			width = 2
		case subject[i] == '|', subject[i] == ';':
			width = 1
		default:
			continue
		}
		segments = append(segments, strings.TrimSpace(subject[start:i+width]))
		i += width - 1
		start = i + 1
	}
	if rest := strings.TrimSpace(subject[start:]); rest != "" {
		segments = append(segments, rest)
	}
	return segments
}

func approvalWhy(req approval.Request) string {
	return cmp.Or(strings.TrimSpace(req.Summary), strings.TrimSpace(req.Reason))
}

const approvalDetailHeight = 12

func approvalDetailLines(req approval.Request, width int) []string {
	// Tool and tier are already on the headline.
	fields := [][2]string{{"Summary", req.Summary}, {"Reason", req.Reason}}
	contentWidth := max(1, width-4)
	out := make([]string, 0, len(fields)+1)
	for _, field := range fields {
		if value := strings.TrimSpace(field[1]); value != "" {
			out = append(out, wrapLines(field[0]+": "+value, contentWidth)...)
		}
	}
	if details := strings.TrimSpace(req.Details); details != "" {
		out = append(out, "Details:")
		for _, line := range strings.Split(details, "\n") {
			out = append(out, wrapLines(line, contentWidth)...)
		}
	}
	return out
}

func renderApprovalDetailLine(line string) string {
	switch {
	case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
		return ui.Invokr.Accent.Render(line)
	case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
		return ui.Invokr.Danger.Render(line)
	case strings.HasPrefix(line, "***"), strings.HasPrefix(line, "@@"):
		return ui.Invokr.Frame.Render(line)
	default:
		return ui.Invokr.Text.Render(line)
	}
}

// wrapLines is wrapText split into the lines a caller lays out one by one.
func wrapLines(text string, width int) []string { return strings.Split(wrapText(text, width), "\n") }

func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	// ansi.Wrap can exceed the visible limit when a styled token's reset code
	// follows a long word; Hardwrap normalizes that narrow-terminal edge case.
	return ansi.Hardwrap(ansi.Wrap(text, width, ""), width, false)
}

const maxContextBarWidth = 40

// contextMeterWidth is the footer meter's full width, matching the design's
// sixteen cells. It shrinks on a narrow pane and is dropped before the numbers.
const contextMeterWidth = 16

// renderContextStatus is the footer's right-hand cell: "ctx ████░░░░ 12.4k / 128k".
// It replaces the pre-redesign full-width bar plus "context: …" line, which
// together took two rows above the input.
func renderContextStatus(usage turn.TokenUsage, width, compactPercent int) string {
	if !usage.Available {
		return ""
	}
	used := usage.LatestPromptTokens + usage.LatestCompletionTokens
	label := ui.Invokr.Muted.Render("ctx")
	if usage.ContextWindowTokens <= 0 {
		return label + " " + ui.Invokr.Muted.Render(formatTokensCompact(used)+" / ?")
	}
	count := formatTokensCompact(used) + " / " + formatTokensCompact(usage.ContextWindowTokens)
	style, fill := contextColors(contextPercent(usage), compactPercent)
	barWidth := min(contextMeterWidth, width-ansi.StringWidth("ctx  "+count))
	if barWidth < 4 {
		return label + " " + style.Render(count)
	}
	return label + " " + tuikit.NewMeter(barWidth, fill).View(int(contextPercent(usage)+0.5)) + " " + style.Render(count)
}

func contextColors(percent float64, compactPercent int) (lipgloss.Style, color.Color) {
	switch {
	case percent >= float64(compactPercent):
		return ui.Invokr.Danger, ui.Invokr.Palette.Red
	case percent >= constants.ContextWarningPercent:
		return ui.Invokr.Warn, ui.Invokr.Palette.Yellow
	default:
		return ui.Invokr.Info, ui.Invokr.Palette.Blue
	}
}

func contextPercent(usage turn.TokenUsage) float64 {
	used := usage.LatestPromptTokens + usage.LatestCompletionTokens
	return float64(used) / float64(usage.ContextWindowTokens) * 100
}

func sectionRule(label string, width int) string {
	head := ui.Invokr.Warn.Render(label)
	if width <= 0 {
		width = 44
	}
	if fill := width - ansi.StringWidth(label) - 1; fill >= 1 {
		return head + " " + ui.Invokr.Frame.Render(strings.Repeat("─", fill))
	}
	return head
}

// cursorDisplayRow reports the wrapped row the cursor sits on. The textarea
// indexes prompt rows by display line while Line() counts logical lines, so a
// soft-wrapped draft makes the two disagree; walking the cursor up on a copy
// borrows the textarea's own wrap logic rather than re-deriving it.
func cursorDisplayRow(ta textarea.Model) int {
	// The rows above the cursor inside its own line come straight from
	// LineInfo; stepping up through them would rehash that whole line once per
	// row, which is what made a long one-line draft slow to type into. Only the
	// earlier lines are walked.
	rows := ta.LineInfo().RowOffset
	ta.CursorStart()
	for ta.Line() > 0 || ta.LineInfo().RowOffset > 0 {
		ta.CursorUp()
		rows++
	}
	return rows
}
