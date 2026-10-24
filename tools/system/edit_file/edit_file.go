// Package edit_file implements a search/replace file edit tool: a low-ceremony
// alternative to the apply_patch envelope. The edit structure lives in typed
// schema fields (path/old/new) rather than in a freeform patch string, which
// weaker models handle far more reliably than diff syntax.
package edit_file

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/invokr-ai/invokr/capability/approval"
	"github.com/invokr-ai/invokr/platform/events"
	"github.com/invokr-ai/invokr/support/fsutil"
	"github.com/invokr-ai/invokr/tools/core"
	"github.com/invokr-ai/invokr/tools/system/workspace"
)

type editArgs struct {
	Path    string   `json:"path" desc:"Workspace-relative or absolute path to edit."`
	Content *string  `json:"content,omitempty" desc:"Full file contents. Set this instead of edits to create or overwrite the whole file, including an empty file."`
	Edits   []editOp `json:"edits,omitempty" desc:"One or more replacements against the original file; required unless content or delete is set. Overlapping replacements fail atomically."`
	DryRun  bool     `json:"dry_run,omitempty" desc:"Validate the edit and report the outcome without writing."`
	Delete  bool     `json:"delete,omitempty" desc:"Delete the file at path instead of editing it. Give no edits with it."`
}

type editOp struct {
	Old        string `json:"oldText" desc:"Exact text to find in the original file; must be nonempty."`
	New        string `json:"newText" desc:"Replacement text. Empty deletes matched text."`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"Replace every match instead of requiring a unique one."`
}

func (op *editOp) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, ok := fields["newText"]; !ok {
		return errors.New("edit requires newText")
	}
	type plain editOp
	return json.Unmarshal(data, (*plain)(op))
}

type preparedEdit struct {
	args                                     editArgs
	displayPath, absPath, updated, operation string
	original                                 string // the file before the write, for the approval diff
	mode                                     os.FileMode
	replaced                                 int
}

type editResponse struct {
	Path     string `json:"path"`
	Action   string `json:"action"`
	Replaced int    `json:"replaced"`
	DryRun   bool   `json:"dry_run,omitempty"`
}

// Keep the edit pair in declaration order on the provider wire. JSON maps
// otherwise sort "new" before "old", despite editOp's field order.
func editSchema() map[string]any {
	schema := core.SchemaFor[editArgs]()
	items := schema["properties"].(map[string]any)["edits"].(map[string]any)["items"].(map[string]any)
	properties := items["properties"].(map[string]any)
	ordered := struct {
		Old        any `json:"oldText"`
		New        any `json:"newText"`
		ReplaceAll any `json:"replace_all"`
	}{properties["oldText"], properties["newText"], properties["replace_all"]}
	data, _ := json.Marshal(ordered)
	items["properties"] = json.RawMessage(data)
	return schema
}

func RegisterEditFileTool(c core.NativeRegistrar, dispatcher events.Dispatcher, wsp workspace.Policy) {
	core.Register(c, core.Tool[preparedEdit]{
		Spec: core.ToolSpec{
			Name:        "edit",
			Description: "Modify one file at path. Choose content (create/overwrite, including empty files; creates parent directories), edits (existing file), or delete. Batch replacements in edits: each oldText matches the original file uniquely unless replace_all=true; newText replaces it. Overlaps fail atomically. Never combine content, edits, or delete. dry_run validates without writing.",
			Example:     `{"name":"edit","arguments":{"path":"README.md","edits":[{"oldText":"## Usage","newText":"## Usage\n\nRun the CLI."}]}}`,
			Parameters:  editSchema(),
			Metadata:    core.WriteWorkspaceAskMetadata("workspace", core.ToolCapabilities{ReadsWorkspace: true, WritesWorkspace: true}),
		},
		Decode: func(call *core.ToolCall) (preparedEdit, error) {
			return prepareEdit(call, wsp)
		},
		Approve: func(ctx context.Context, plan preparedEdit) core.ApprovalDecision {
			req := editApprovalRequest(plan)
			req.Confined, req.Writes, req.ReadOnly = true, !plan.args.DryRun, plan.args.DryRun
			req.Paths, req.Protected = []string{plan.displayPath}, !plan.args.DryRun && wsp.IsProtected(plan.absPath)
			// A dry run writes nothing, so it never needs a prompt.
			if plan.args.DryRun {
				return core.Automatic(req)
			}
			return core.Ask(req)
		},
		Approver: core.ApproverFrom(c),
		AuditFailure: func(ctx context.Context, plan preparedEdit, err error) {
			dispatchEditAudit(ctx, dispatcher, plan, "rejected", err)
		},
		Execute: func(ctx context.Context, plan preparedEdit) (string, error) {
			return runPreparedEdit(ctx, dispatcher, plan)
		},
	})
}

func prepareEdit(call *core.ToolCall, wsp workspace.Policy) (preparedEdit, error) {
	if err := validateArgKeys(call); err != nil {
		return preparedEdit{}, err
	}
	args, err := core.DecodeOptionalToolArgs[editArgs](call)
	if err != nil {
		return preparedEdit{}, fmt.Errorf("parse edit arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return preparedEdit{}, fmt.Errorf("path is required")
	}
	abs, err := wsp.ResolveWrite(args.Path)
	if err != nil {
		return preparedEdit{}, err
	}
	plan := preparedEdit{args: args, displayPath: wsp.RelDisplayPath(abs, args.Path), absPath: abs, mode: 0o644}

	if args.Delete {
		return prepareDelete(plan)
	}
	if args.Content != nil {
		return prepareContent(plan)
	}
	return prepareReplacement(plan)
}

func prepareContent(plan preparedEdit) (preparedEdit, error) {
	if len(plan.args.Edits) > 0 {
		return preparedEdit{}, fmt.Errorf("edit %s: content cannot be combined with edits", plan.displayPath)
	}
	info, err := os.Stat(plan.absPath)
	switch {
	case os.IsNotExist(err):
		plan.operation = "create"
	case err != nil:
		return preparedEdit{}, fmt.Errorf("stat %s: %w", plan.displayPath, err)
	case info.IsDir():
		return preparedEdit{}, fmt.Errorf("edit %s: path is a directory", plan.displayPath)
	default:
		plan.operation, plan.mode = "overwrite", info.Mode().Perm()
		if content, readErr := os.ReadFile(plan.absPath); readErr == nil {
			plan.original = string(content)
		}
	}
	plan.updated, plan.replaced = *plan.args.Content, 1
	return plan, nil
}

func prepareReplacement(plan preparedEdit) (preparedEdit, error) {
	args := plan.args
	if len(args.Edits) == 0 {
		return preparedEdit{}, fmt.Errorf("edit: edits must contain at least one replacement")
	}
	content, err := os.ReadFile(plan.absPath)
	switch {
	case os.IsNotExist(err):
		return preparedEdit{}, fmt.Errorf("edit %s: file does not exist; use content to create it", plan.displayPath)
	case err != nil:
		return preparedEdit{}, fmt.Errorf("read %s: %w", plan.displayPath, err)
	}
	if info, statErr := os.Stat(plan.absPath); statErr == nil {
		plan.mode = info.Mode().Perm()
	}
	original := string(content)
	updated, replaced, err := applyOriginalOps(original, args, plan.displayPath, workspace.DominantEOL(original))
	if err != nil {
		return preparedEdit{}, err
	}
	plan.updated, plan.replaced, plan.original = updated, replaced, original
	return plan, nil
}

// runDelete removes the file, or reports what it would remove on a dry run.
func runDelete(ctx context.Context, dispatcher events.Dispatcher, plan preparedEdit) (string, error) {
	if !plan.args.DryRun {
		if err := os.Remove(plan.absPath); err != nil {
			dispatchEditAudit(ctx, dispatcher, plan, "failed", err)
			return "", fmt.Errorf("delete %s: %w", plan.displayPath, err)
		}
	}
	resp := editResponse{Path: plan.displayPath, Action: "delete", DryRun: plan.args.DryRun}
	return finishEdit(ctx, dispatcher, plan, "completed", resp)
}

// finishEdit encodes the result and records the audit outcome of a call that
// did what it was asked to.
func finishEdit(ctx context.Context, dispatcher events.Dispatcher, plan preparedEdit, outcome string, resp editResponse) (string, error) {
	data, err := core.MarshalResult("edit", resp)
	if err != nil {
		return "", err
	}
	dispatchEditAudit(ctx, dispatcher, plan, outcome, nil)
	return data, nil
}

// prepareDelete validates a delete request: the path must name an existing
// regular file inside the workspace (wsp.Resolve already enforced that), and
// no edit fields may accompany it, so "delete" can never be a silent
// alternative outcome of a call that also asked for an edit.
func prepareDelete(plan preparedEdit) (preparedEdit, error) {
	args := plan.args
	if len(args.Edits) > 0 || args.Content != nil {
		return preparedEdit{}, fmt.Errorf("delete %s: give delete on its own, without content or edits", plan.displayPath)
	}
	info, err := os.Stat(plan.absPath)
	if os.IsNotExist(err) {
		return preparedEdit{}, fmt.Errorf("cannot delete %s: file does not exist", plan.displayPath)
	}
	if err != nil {
		return preparedEdit{}, fmt.Errorf("stat %s: %w", plan.displayPath, err)
	}
	if info.IsDir() {
		return preparedEdit{}, fmt.Errorf("cannot delete %s: it is a directory; only files can be deleted", plan.displayPath)
	}
	plan.operation, plan.mode = "delete", info.Mode().Perm()
	return plan, nil
}

// applyOriginalOps locates all replacements in the original file and writes
// them only after every match and overlap has been validated.
func applyOriginalOps(content string, args editArgs, display, eol string) (string, int, error) {
	ops, err := editOpsFor(args, display)
	if err != nil {
		return "", 0, err
	}
	type change struct {
		span [2]int
		text string
	}
	var changes []change
	for i, op := range ops {
		spans, lineRung, err := replacementSpans(content, op.Old, op.ReplaceAll)
		if err != nil {
			if errors.Is(err, errOldNotFound) {
				err = notFoundError(content, op.Old)
			}
			return "", 0, fmt.Errorf("edit %s: edits[%d]: %w", display, i, err)
		}
		text, spans := replacementFor(content, op.New, spans, lineRung, eol)
		for _, span := range spans {
			changes = append(changes, change{span, text})
		}
	}
	slices.SortFunc(changes, func(a, b change) int { return cmp.Compare(a.span[0], b.span[0]) })
	for i := 1; i < len(changes); i++ {
		if changes[i].span[0] < changes[i-1].span[1] {
			return "", 0, fmt.Errorf("edit %s: overlapping edits; merge them into one replacement. No changes were written", display)
		}
	}
	for _, c := range slices.Backward(changes) {
		content = content[:c.span[0]] + c.text + content[c.span[1]:]
	}
	return content, len(changes), nil
}

// editOpsFor validates replacements on an existing file.
func editOpsFor(args editArgs, display string) ([]editOp, error) {
	for i, op := range args.Edits {
		if op.Old == "" {
			return nil, fmt.Errorf("edit %s: edits[%d]: empty oldText; use content to create or overwrite the file", display, i)
		}
		args.Edits[i].Old = stripLinePrefixes(op.Old)
	}
	return args.Edits, nil
}

// toEOL rewrites every line ending in s to eol.
func toEOL(s, eol string) string {
	lf := strings.ReplaceAll(s, "\r\n", "\n")
	if eol == "\n" {
		return lf
	}
	return strings.ReplaceAll(lf, "\n", eol)
}

// replacementSpans tries each rung in turn; StripCR lets LF `old` match a CRLF
// file without also tolerating trailing-whitespace drift.
func replacementSpans(content, oldText string, all bool) ([][2]int, bool, error) {
	rungs := []func(string) string{nil, core.StripCR, trimRightLine, strings.TrimSpace, collapseSpacesLine}
	for i, norm := range rungs {
		spans := locate(content, oldText, norm)
		if len(spans) == 0 {
			continue
		}
		// The loosest rung ignores the model's spacing altogether, so it is
		// only trusted when it lands in exactly one place: two regions equal
		// modulo spacing are a coin flip, and an edit written to the wrong one
		// is worse than the "old not found" the model can retry from.
		if i == len(rungs)-1 && len(spans) > 1 {
			break
		}
		if len(spans) > 1 && !all {
			return nil, false, fmt.Errorf("old matches %d locations (first at offsets %d and %d); include surrounding lines to make it unique, or set replace_all", len(spans), spans[0][0], spans[1][0])
		}
		if !all {
			spans = spans[:1]
		}
		return spans, norm != nil, nil
	}
	if spans := locateBlockAnchor(content, oldText); len(spans) == 1 && !all {
		return spans, true, nil
	}
	return nil, false, errOldNotFound
}

// Block-anchor budgets (#577). The five ladder rungs all require every line of
// old to match once whitespace is normalized, so a block whose interior the
// model paraphrased fails whole — 24% of the failed multi-line edits in the
// pod M1 runs had a region whose first and last lines still matched exactly.
// A candidate must sit within a quarter of old's line count (opencode's band,
// edit.ts:303) and score on a character Levenshtein similarity over the
// trimmed interior. The threshold is 0.85, not opencode's shipped 0.65
// (edit.ts:220) — a deliberate deviation, not a transcription slip: opencode
// pairs 0.65 with best-candidate scoring and an LSP pass that surfaces a
// broken write in the same tool result, and invokr finds a bad write at the
// next build or not at all. In the pod M1 runs 21 of the 24 rescuable edits
// scored above 0.85, so the looser value buys coverage too small to measure
// at n=6 while widening the window for a wrong-region write, which is the
// asymmetric harm and the one n=6 cannot detect. blockAnchorMaxGrowth is ours: a
// candidate that matches at both anchors but carries several times old's text
// is a different region that happens to start and end alike.
const (
	blockAnchorMinLines      = 3
	blockAnchorSizeTolerance = 0.25
	blockAnchorMinSimilarity = 0.85
	blockAnchorMaxGrowth     = 2
	blockAnchorMaxCells      = 4 << 20
)

// locateBlockAnchor matches a multi-line old by its first line, trimmed,
// accepting the candidate block whose remaining lines score closest. The last
// line is not an anchor: replaying the #577 M1 failures, a first-line-only
// anchor rescued 9 of 23 at 0.85 against 7 for first+last. It is the
// ladder's last resort, so it returns every candidate and leaves the caller to
// insist on exactly one: an edit written to the wrong region is worse than the
// "old not found" the model can retry from. opencode takes its best-scoring
// candidate instead; requiring uniqueness costs coverage and buys the
// guarantee that a near-tie is never resolved silently.
func locateBlockAnchor(content, old string) [][2]int {
	want := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	if len(want) < blockAnchorMinLines {
		return nil
	}
	lines, offsets := splitWithOffsets(content)
	first := strings.TrimSpace(want[0])
	delta := max(1, int(float64(len(want))*blockAnchorSizeTolerance))
	sizes := []int{len(want)}
	for d := 1; d <= delta; d++ {
		if len(want)-d >= blockAnchorMinLines {
			sizes = append(sizes, len(want)-d)
		}
		sizes = append(sizes, len(want)+d)
	}
	var spans [][2]int
	for i := range lines {
		if strings.TrimSpace(lines[i]) != first {
			continue
		}
		if span, ok := bestBlockAt(lines, offsets, i, want, sizes, len(old)); ok {
			spans = append(spans, span)
		}
	}
	return spans
}

// bestBlockAt scores the blocks starting at line i, one per candidate size, and
// returns the best above the similarity floor. There is no last-line anchor,
// so the end is wherever the rest of old scores best; sizes arrive nearest
// old's own line count first, so a tie keeps the block old describes instead
// of swallowing a trailing blank line.
func bestBlockAt(lines []string, offsets []int, i int, want []string, sizes []int, oldLen int) ([2]int, bool) {
	best, bestScore := [2]int{}, 0.0
	for _, size := range sizes {
		end := i + size - 1
		if end >= len(lines) {
			continue
		}
		span := [2]int{offsets[i], offsets[end] + len(lines[end])}
		if span[1]-span[0] > blockAnchorMaxGrowth*oldLen {
			continue
		}
		if score := lineSimilarity(want[1:], lines[i+1:end+1]); score >= blockAnchorMinSimilarity && score > bestScore {
			best, bestScore = span, score
		}
	}
	return best, bestScore > 0
}

// lineSimilarity scores two blocks by Levenshtein distance over their trimmed
// UTF-8 bytes, as 1 - distance/byte length of the longer. Iterate every byte:
// ranging over strings skips continuation-byte cells and can give different
// Unicode interiors a perfect score. Keep the existing byte-based work bound
// and ASCII-calibrated threshold, rather than changing the matching policy.
func lineSimilarity(want, got []string) float64 {
	a, b := trimJoin(want), trimJoin(got)
	if a == b {
		return 1
	}
	if a == "" || b == "" || len(a)*len(b) > blockAnchorMaxCells {
		return 0 // a block that big is not what this rung is for, and the score is quadratic
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := range len(a) {
		cur := make([]int, len(b)+1)
		cur[0] = i + 1
		for j := range len(b) {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			cur[j+1] = min(prev[j]+cost, min(cur[j]+1, prev[j+1]+1))
		}
		prev = cur
	}
	return 1 - float64(prev[len(b)])/float64(max(len(a), len(b)))
}

func trimJoin(lines []string) string {
	return strings.Join(core.MapSlice(lines, strings.TrimSpace), "\n")
}

// replacementFor shapes the replacement text for the rung that matched, and
// pulls the spans back off a CRLF terminator where needed. Line-based rungs
// match whole lines without their terminator, so the replacement must shed its
// own trailing newline too. Either way it carries the file's own line ending
// rather than whatever the model sent: the model that quotes "old" faithfully
// out of read_file is exactly the one that then hand-writes "new" with bare
// newlines.
func replacementFor(content, newText string, spans [][2]int, lineRung bool, eol string) (string, [][2]int) {
	if !lineRung {
		return toEOL(newText, eol), spans
	}
	if eol == "\r\n" {
		spans = keepTrailingCR(content, spans)
	}
	return toEOL(strings.TrimSuffix(toEOL(newText, "\n"), "\n"), eol), spans
}

// errOldNotFound is a sentinel so the caller can build the "closest match"
// hint against the file on disk rather than the partly-edited buffer: a
// failed batch is rolled back, so a hint quoting the buffer would tell the
// model to copy text that is not in the file.
var errOldNotFound = errors.New("old not found in file")

// keepTrailingCR pulls a span back off the carriage return that terminates its
// last line, so a CRLF file keeps its own \r\n around the spliced replacement.
func keepTrailingCR(content string, spans [][2]int) [][2]int {
	for i, span := range spans {
		if span[1] > span[0] && content[span[1]-1] == '\r' {
			spans[i][1] = span[1] - 1
		}
	}
	return spans
}

// locate returns the byte spans of old in content. A nil norm means exact
// substring search; otherwise lines are compared after norm.
func locate(content, old string, norm func(string) string) [][2]int {
	if norm == nil {
		return locateExact(content, old)
	}
	return locateLines(content, old, norm)
}

func locateExact(content, old string) [][2]int {
	var spans [][2]int
	for from := 0; from <= len(content); {
		i := strings.Index(content[from:], old)
		if i < 0 {
			break
		}
		start := from + i
		spans = append(spans, [2]int{start, start + len(old)})
		from = start + max(len(old), 1)
	}
	return spans
}

func locateLines(content, old string, norm func(string) string) [][2]int {
	lines, offsets := splitWithOffsets(content)
	want := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	var spans [][2]int
	for i := 0; i+len(want) <= len(lines); i++ {
		if !slices.EqualFunc(lines[i:i+len(want)], want, core.EqualBy(norm)) {
			continue
		}
		last := i + len(want) - 1
		spans = append(spans, [2]int{offsets[i], offsets[last] + len(lines[last])})
		i = last
	}
	return spans
}

func splitWithOffsets(content string) ([]string, []int) {
	lines := strings.Split(content, "\n")
	offsets := make([]int, len(lines))
	at := 0
	for i, line := range lines {
		offsets[i] = at
		at += len(line) + 1
	}
	return lines, offsets
}

func trimRightLine(s string) string { return strings.TrimRight(s, " \t\r") }

// Bound the context around a suggested match and the whole-file fallback.
const (
	resupplyContext        = 2
	resupplyWholeFileLines = 200
	resupplyWholeFileBytes = 8000
)

// collapseSpacesLine trims a line and squeezes its internal blank runs to one
// space, so text the model retyped with its own spacing ("func f(a, b  int)",
// a tab where the file has spaces) still finds the line it means. "old not
// found in file" was 21 of the round's 24 tool failures (r10 H3), most of them
// on import blocks and comment headers reconstructed from memory.
func collapseSpacesLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// notFoundError re-supplies the file's actual text at the point of refusal,
// so the retry can quote the tool's own output instead of the model's memory.
// Candidate ranking is diagnostic only; it never authorizes a replacement.
func notFoundError(content, old string) error {
	want := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	lines, _ := splitWithOffsets(content)
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // the terminator's phantom trailing line
	}
	bestIdx, bestScore := -1, 0
	for i := range lines {
		if score := windowScore(lines, want, i); score > bestScore {
			bestIdx, bestScore = i, score
		}
	}
	if bestIdx < 0 {
		return noResemblanceError(content, lines, want[0])
	}
	from, to := max(0, bestIdx-resupplyContext), min(bestIdx+len(want)+resupplyContext, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "old not found in file; suggested closest match to old starts at line %d.\n", bestIdx+1)
	writeFirstDifference(&b, lines, want, bestIdx)
	fmt.Fprintf(&b, "Copy the raw text below verbatim into oldText — do not retype it from memory. File's current text, lines %d-%d; closest match in the file:\n", from+1, to)
	for i := from; i < to; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}
	return fmt.Errorf("%s", b.String())
}

// writeFirstDifference names the first line of old that the file disagrees
// with, which is usually enough on its own for a near-miss retry.
func writeFirstDifference(b *strings.Builder, lines, want []string, bestIdx int) {
	for j, line := range want {
		if bestIdx+j < len(lines) && strings.TrimSpace(lines[bestIdx+j]) == strings.TrimSpace(line) {
			continue
		}
		got := ""
		if bestIdx+j < len(lines) {
			got = lines[bestIdx+j]
		}
		fmt.Fprintf(b, "first difference at old line %d: you gave %q, the file has %q\n", j+1, line, got)
		return
	}
}

// noResemblanceError handles the miss with no close match at all — the model
// invented the anchor wholesale, so there is no window to point at. A file
// small enough to cost less than the re-read it would otherwise provoke is
// returned whole; a larger one is not, because flooding the context is the
// very pressure that caused the fabrication. The model re-reads that one, as
// it does today.
func noResemblanceError(content string, lines []string, first string) error {
	if len(lines) <= resupplyWholeFileLines && len(content) <= resupplyWholeFileBytes {
		return fmt.Errorf("old not found in file; nothing resembles %q. Copy the raw text below verbatim into oldText — do not retype it from memory. The file's full current contents (%d lines):\n%s", first, len(lines), strings.TrimSuffix(content, "\n"))
	}
	return fmt.Errorf("old not found in file; nothing resembles %q. The file is %d lines, too large to quote here: read it and copy the exact text into oldText — do not retype it from memory", first, len(lines))
}

// Exact lines dominate; bounded whole-line similarity breaks ties without
// favoring an unrelated line that happens to share a longer prefix. Similarity
// must exceed 50% to suggest a candidate without any exact matching lines.
func windowScore(lines, want []string, start int) int {
	score := 0
	for j, line := range want {
		if start+j >= len(lines) {
			break
		}
		got, exp := strings.TrimSpace(lines[start+j]), strings.TrimSpace(line)
		switch {
		case got == exp && exp != "":
			score += 1000
		case j == 0 && exp != "":
			score += max(0, int(100*lineSimilarity([]string{exp}, []string{got}))-50)
		}
	}
	return score
}

func editApprovalRequest(plan preparedEdit) approval.Request {
	req := approval.Request{Tool: "edit", Reason: "workspace write", Tier: "write_workspace", Summary: fmt.Sprintf("%s %s", plan.action("edit"), plan.displayPath), Details: fmt.Sprintf("Replacements: %d\n\n%s", plan.replaced, describeOps(plan.args)), Subject: plan.displayPath, SubjectIsPathList: true}
	if plan.operation != "delete" {
		req.Diff = &approval.FileDiff{Path: plan.displayPath, Before: plan.original, After: plan.updated}
	}
	return req
}

// describeOps renders the requested change for the approval prompt, whichever
// shape the call used.
func describeOps(args editArgs) string {
	if args.Delete {
		return "Delete the file."
	}
	if args.Content != nil {
		return fmt.Sprintf("Write full file content:\n%s", *args.Content)
	}
	var b strings.Builder
	for i, op := range args.Edits {
		fmt.Fprintf(&b, "Edit %d old:\n%s\n\nEdit %d new:\n%s\n\n", i+1, op.Old, i+1, op.New)
	}
	return strings.TrimSuffix(b.String(), "\n\n")
}

func runPreparedEdit(ctx context.Context, dispatcher events.Dispatcher, plan preparedEdit) (string, error) {
	if plan.operation == "delete" {
		return runDelete(ctx, dispatcher, plan)
	}
	if !plan.args.DryRun {
		if err := os.MkdirAll(filepath.Dir(plan.absPath), 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", plan.displayPath, err)
		}
		if err := fsutil.WriteFileAtomic(plan.absPath, []byte(plan.updated), plan.mode); err != nil {
			dispatchEditAudit(ctx, dispatcher, plan, "failed", err)
			return "", fmt.Errorf("write %s: %w", plan.displayPath, err)
		}
	}
	return finishEdit(ctx, dispatcher, plan, "applied", editResponse{Path: plan.displayPath, Action: plan.action("update"), Replaced: plan.replaced, DryRun: plan.args.DryRun})
}

// action names what the plan does to the file; fallback is the plain-edit
// word, which the approval prompt and the result spell differently.
func (p preparedEdit) action(fallback string) string {
	return cmp.Or(p.operation, fallback)
}

func dispatchEditAudit(ctx context.Context, dispatcher events.Dispatcher, plan preparedEdit, outcome string, err error) {
	core.DispatchAudit(ctx, dispatcher, &events.AuditPayload{Action: "edit", Tool: "edit", Paths: []string{plan.displayPath}, DryRun: plan.args.DryRun, Outcome: outcome}, err)
}

var knownArgKeys = []string{"path", "content", "edits", "dry_run", "delete"}

func validateArgKeys(call *core.ToolCall) error {
	if call == nil || len(call.Arguments) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &raw); err != nil {
		return nil // let the typed decode report the syntax error
	}
	for key := range raw {
		if !slices.Contains(knownArgKeys, key) {
			return fmt.Errorf("edit: unknown argument %q; accepted: %s", key, strings.Join(knownArgKeys, ", "))
		}
	}
	return nil
}

var linePrefix = regexp.MustCompile(`^line \d+: `)

// stripLinePrefixes removes a "line N: " prefix from every line of old when
// all of them carry one — the model copied a previous error hint back.
func stripLinePrefixes(old string) string {
	lines := strings.Split(old, "\n")
	if slices.ContainsFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != "" && !linePrefix.MatchString(line)
	}) {
		return old
	}
	return strings.Join(core.MapSlice(lines, func(line string) string {
		return linePrefix.ReplaceAllString(line, "")
	}), "\n")
}
