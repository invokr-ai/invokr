package agentui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/invokr-ai/invokr/platform/events"
	"github.com/invokr-ai/invokr/platform/turn"
	"github.com/invokr-ai/invokr/surface/ui"
)

type toolStatus int

const (
	toolRunning toolStatus = iota
	toolDone
	toolFailed
)

type message struct {
	id, version uint64
	role, text  string
	streaming   bool
	retries     int // stream retries a retry notice line counts

	// meta (role == "assistant") is the provenance line under the answer:
	// model, elapsed, tokens.
	meta responseMeta

	// tool fields (role == "tool"): a single block keyed by toolID that mutates
	// in place from running -> done/failed.
	toolID, toolName, toolArgs string
	toolStatus                 toolStatus
	// toolProgressLine is the latest incremental status a running tool
	// reported (EventToolProgress); shown beside the live elapsed.
	toolProgressLine    string
	toolStartedAt       time.Time
	toolDuration        time.Duration
	toolBytes           int
	toolCode, toolError string
	// toolDiff is the unified diff of an applied edit, rendered under the tool
	// line so ordinary edits are visible without an approval prompt.
	toolDiff string
	// editFile is an applied edit's whole file before and after, so its diff
	// numbers lines by their place in the file. Empty when it could not be
	// rebuilt; the diff then falls back to the patch fragments.
	editFile patchSection
	// toolOutput is the recorded result as the model saw it (capped upstream).
	// Per-tool renderers show a tail of it collapsed, all of it expanded.
	toolOutput string
	// expanded marks a settled block the user unfolded to its full recorded
	// result. It survives streaming updates because the block mutates in place.
	expanded bool
}

// live reports whether the message still changes frame to frame — a streaming
// answer or a running tool block (whose spinner and elapsed tick).
func (m message) live() bool {
	return m.streaming || (m.role == "tool" && m.text == "" && m.toolStatus == toolRunning)
}

// chatLog owns the chat transcript: user/assistant/tool/help/error messages,
// including the tool-event state machine that mutates a running tool block in
// place as it completes rather than appending a new line per event. The
// model composes chatLog's messages with intro/sending/queue/help chrome in a
// windowed MemoList, which owns scrolling and the render cache.
type chatLog struct {
	messages []message
	// recordedTools retains request events even when progress display drops blocks.
	recordedTools []message
	nextID        uint64
	toolProgress  progressMode
}

func newChatLog(toolProgress progressMode) chatLog { return chatLog{toolProgress: toolProgress} }

func (c *chatLog) Append(msg message) {
	c.nextID, msg.id = c.nextID+1, c.nextID+1
	c.messages = append(c.messages, msg)
}

func (c *chatLog) appendTranscript(item turn.TranscriptMessage) {
	c.Append(message{role: item.Role, text: turn.AppendHistoricalExecution(turn.AppendToolOutcomeGuard(item.Text, item.ToolCalls), item.ExecutionEvidence)})
}

func (c *chatLog) Len() int { return len(c.messages) }

// Last returns a pointer to the last message, or nil if the log is empty.
func (c *chatLog) Last() *message {
	if len(c.messages) == 0 {
		return nil
	}
	return &c.messages[len(c.messages)-1]
}

// ApplyEvent updates the chat with a tool/notice/assistant-delta event,
// mutating the matching block in place (keyed by tool-call ID) rather than
// appending a new line per event.
func (c *chatLog) ApplyEvent(ev events.Event) {
	if c.applyAssistantDelta(ev) || c.applyTextEvent(ev) {
		return
	}
	if ev.Tool == nil {
		return
	}
	t := *ev.Tool
	if ev.Type == events.EventToolProgress {
		c.setToolProgress(t)
		return
	}
	if ev.Type == events.EventToolStarted {
		started := ev.Timestamp
		if started.IsZero() {
			started = time.Now()
		}
		c.Append(message{role: "tool", toolID: t.ID, toolName: t.Name,
			toolArgs: t.Arguments, toolStatus: toolRunning, toolStartedAt: started})
		c.recordedTools = append(c.recordedTools, c.messages[len(c.messages)-1])
		return
	}

	idx := c.finishToolMessage(t, ev.Type == events.EventToolFailed)
	settled := c.messages[idx]
	// Keep only evidence fields; full tool output stays in the transcript owner.
	settled.toolOutput, settled.toolDiff, settled.toolError = "", "", ""
	settled.editFile = patchSection{}
	if prior := slices.IndexFunc(c.recordedTools, func(m message) bool { return m.id == settled.id }); prior >= 0 {
		c.recordedTools[prior] = settled
	} else {
		c.recordedTools = append(c.recordedTools, settled)
	}

	switch c.toolProgress {
	case progressOff:
		// Drop the finished block; slices.Delete clears the freed tail.
		c.messages = slices.Delete(c.messages, idx, idx+1)
	case progressNew:
		// ponytail: collapse repeats — drop earlier finished blocks of the same
		// tool, keeping the latest. Not strictly consecutive-only; good enough.
		c.dropFinishedToolBlocks(t.Name, idx)
	}
}

// setToolProgress matches call IDs, falling back to names for legacy events.
func (c *chatLog) setToolProgress(t events.ToolPayload) {
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].toolStatus == toolRunning && c.messages[i].toolName == t.Name && (t.ID == "" || c.messages[i].toolID == t.ID) {
			c.messages[i].toolProgressLine = t.Progress
			c.messages[i].version++
			return
		}
	}
}

// finishToolMessage settles the block for a completed/failed tool call,
// mutating the running block in place when one exists, and returns its index.
func (c *chatLog) finishToolMessage(t events.ToolPayload, failed bool) int {
	msg := message{
		role: "tool", toolID: t.ID, toolName: t.Name, toolArgs: t.Arguments,
		toolStatus: toolDone, toolDuration: t.Duration, toolBytes: t.OutputBytes,
		toolCode: t.Code, toolError: t.Error, toolOutput: t.Output,
	}
	if failed {
		msg.toolStatus = toolFailed
	}
	idx := c.findToolMessage(t.ID)
	if idx >= 0 {
		// Keep the started event's name/args: some emitters omit them on completion.
		prev := c.messages[idx]
		msg.toolStartedAt, msg.toolName, msg.toolArgs, msg.expanded = prev.toolStartedAt, prev.toolName, prev.toolArgs, prev.expanded
		msg.id, msg.version = prev.id, prev.version+1
	}
	if msg.toolStatus == toolDone && t.Code == "" {
		// A flagged result may not have applied, so it gets no diff.
		msg.toolDiff = cmp.Or(appliedPatchDiff(msg.toolName, msg.toolArgs),
			editedFileDiff(msg.toolName, msg.toolArgs))
		msg.editFile = editedFile(msg.toolName, msg.toolArgs)
	}
	if idx < 0 {
		// No running block to update (e.g. event arrived out of order); append one.
		c.Append(msg)
		return len(c.messages) - 1
	}
	c.messages[idx] = msg
	return idx
}

func (c *chatLog) applyAssistantDelta(ev events.Event) bool {
	if ev.Type != events.EventAssistantDelta || ev.Delta == nil {
		return false
	}
	if last := c.Last(); last != nil && last.role == "assistant" && last.streaming {
		last.text += ev.Delta.Text
		last.version++
	} else {
		c.Append(message{role: "assistant", text: ev.Delta.Text, streaming: true})
	}
	return true
}

func (c *chatLog) findToolMessage(id string) int {
	for i := len(c.messages) - 1; i >= 0; i-- {
		m := c.messages[i]
		if id != "" && m.role == "tool" && m.toolID == id && m.toolStatus == toolRunning {
			return i
		}
	}
	return -1
}

func (c *chatLog) dropFinishedToolBlocks(name string, keepIdx int) {
	i := -1
	c.messages = slices.DeleteFunc(c.messages, func(msg message) bool {
		i++
		return i != keepIdx && msg.role == "tool" && msg.toolName == name && msg.toolStatus != toolRunning
	})
}

// renderMessage renders one message; MemoList owns the viewport render memo.
func (c *chatLog) renderMessage(i, width int, spinnerFrame string) string {
	var mb strings.Builder
	writeMessage(&mb, c.messages[i], width, spinnerFrame)
	return mb.String()
}

// foldable reports whether a message is a settled tool block the user can
// expand and collapse.
func (m message) foldable() bool {
	return m.role == "tool" && m.text == "" && m.toolStatus != toolRunning
}

// Format the latest request for the existing modal, without changing the prose.
func (c *chatLog) executionHistory(start uint64, failures []turn.ToolOutcome, evidence []turn.ExecutionEvidence) string {
	evidence = slices.Clone(evidence)
	for i, item := range evidence {
		preview := item.Command
		if idx := slices.IndexFunc(c.recordedTools, func(m message) bool {
			return m.id > start && item.CallID != "" && m.role == "tool" && m.toolID == item.CallID
		}); idx >= 0 {
			preview = cmp.Or(preview, primaryArgLimit(c.recordedTools[idx].toolName, c.recordedTools[idx].toolArgs, 160))
		}
		evidence[i].CallID, evidence[i].Command = cmp.Or(preview, "arguments unavailable"), ""
	}
	details := turn.AppendExecutionGuard(turn.AppendToolOutcomeGuard("", failures), evidence)
	if details == "" {
		return "No tool calls recorded for this request"
	}
	return details
}

// observedEvidence is UI-only fallback when a failed/canceled runner returns
// no ledger. It describes drained events; a running call remains incomplete.
func (c *chatLog) observedEvidence(start uint64) []turn.ExecutionEvidence {
	var observed []turn.ExecutionEvidence
	for _, msg := range c.recordedTools {
		if msg.id <= start {
			continue
		}
		item := turn.ExecutionEvidence{CallID: msg.toolID, Tool: msg.toolName, Code: msg.toolCode, Outcome: "incomplete"}
		switch msg.toolStatus {
		case toolDone:
			item.Outcome = "completed"
			if msg.toolCode != "" {
				item.Outcome = "flagged"
			}
		case toolFailed:
			item.Outcome = "failed"
		}
		observed = append(observed, item)
	}
	return observed
}

// markFocused appends the transcript-focus cursor to a block's header line.
func markFocused(rendered string) string {
	lines := strings.SplitN(rendered, "\n", 2)
	lines[0] += " " + ui.Invokr.Warn.Bold(true).Render("◀")
	return strings.Join(lines, "\n")
}

// answerIndent aligns an answer under the accent bar of the user turn above it.
const answerIndent = "  "

// indentBlock prefixes every line of text with indent, leaving an empty block
// empty so it does not become a line of trailing spaces.
func indentBlock(text, indent string) string {
	if text == "" {
		return ""
	}
	return indent + strings.ReplaceAll(text, "\n", "\n"+indent)
}

func writeMessage(b *strings.Builder, msg message, width int, spinnerFrame string) {
	width = max(20, width)
	switch msg.role {
	case "user":
		// The design marks a user turn with an accent bar rather than a divider,
		// so consecutive turns read as separate without chrome between them.
		bar := ui.Invokr.Accent.Render("› ")
		indent := strings.Repeat(" ", 4)
		b.WriteString(bar + ui.Invokr.Accent.Render(strings.ReplaceAll(wrapText(msg.text, width-4), "\n", "\n"+indent)))
	case "assistant":
		// The design indents an answer under the accent bar of the turn it
		// answers, which is what makes a user line and its response read as one
		// group rather than as two unrelated blocks.
		body := width - len(answerIndent)
		if msg.streaming {
			b.WriteString(indentBlock(renderPartialMarkdown(msg.text, body), answerIndent))
			return
		}
		b.WriteString(indentBlock(renderResponse(msg.text, msg.meta, body), answerIndent))
	case "tool":
		if msg.text == "" {
			b.WriteString(renderToolBlock(msg, spinnerFrame, width))
			return
		}
		// Non-block tool lines (approval requests/decisions) keep plain text.
		if strings.HasPrefix(msg.text, "approval denied") {
			b.WriteString(ui.Invokr.Danger.Render(wrapText(msg.text, width)))
		} else {
			b.WriteString(ui.Invokr.Muted.Render(wrapText(msg.text, width)))
		}
	case "reasoning":
		b.WriteString(ui.Invokr.Muted.Italic(true).Render(indentBlock(wrapText(msg.text, width-len(answerIndent)), answerIndent)))
	case "error":
		b.WriteString(ui.Invokr.Danger.Render("error: ") + wrapText(msg.text, width-7))
	}
}

// applyTextEvent handles the reasoning and notice events, which carry text
// rather than a tool block.
func (c *chatLog) applyTextEvent(ev events.Event) bool {
	switch {
	case ev.Type == events.EventReasoning && ev.Reasoning != nil:
		c.appendReasoning(ev.Reasoning.Text)
	case ev.Type == events.EventNotice && ev.Notice != nil && ev.Notice.Text == events.StreamRetryNotice:
		c.noteStreamRetry()
	case ev.Type == events.EventNotice && ev.Notice != nil:
		c.Append(message{role: "tool", text: ev.Notice.Text})
	default:
		return false
	}
	return true
}

// appendReasoning adds a step's reasoning in full. It arrives once the reply is
// complete, so on a final turn it goes above the answer already streamed.
func (c *chatLog) appendReasoning(text string) {
	c.Append(message{role: "reasoning", text: strings.TrimSpace(text)})
	if n := len(c.messages); n > 1 && c.messages[n-2].role == "assistant" && c.messages[n-2].streaming {
		c.messages[n-2], c.messages[n-1] = c.messages[n-1], c.messages[n-2]
	}
}

// noteStreamRetry handles a reply stream that was cut short and re-issued: the
// partial reply is dropped, since the retry streams a fresh one, and a single
// line counts the retries instead of one line per attempt.
func (c *chatLog) noteStreamRetry() {
	if last := c.Last(); last != nil && last.role == "assistant" && last.streaming {
		c.messages = c.messages[:len(c.messages)-1]
	}
	if last := c.Last(); last != nil && last.role == "tool" && strings.HasPrefix(last.text, streamRetryLine) {
		last.retries++
		last.text = fmt.Sprintf("%s (%d)", streamRetryLine, last.retries)
		last.version++
		return
	}
	c.Append(message{role: "tool", text: streamRetryLine, retries: 1})
}

const streamRetryLine = "reply cut off · retrying"
