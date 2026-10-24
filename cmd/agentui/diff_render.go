package agentui

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/antonikliment/tuikit"
	"github.com/invokr-ai/invokr/surface/ui"
)

// Edit diffs render through tuikit's DiffView (chroma highlighting, intraline
// word emphasis, side-by-side when the pane is wide enough). DiffView takes
// before/after *contents*, and the transcript only carries the apply_patch
// patch text — so the fragments are reconstructed per file from the patch's
// context/-/+ lines. An edit's whole file is rebuilt where possible (see
// editedFile), so its lines are numbered by their place in the file; an
// apply_patch fragment still counts from 1.

// patchSection is one file's worth of an apply_patch patch.
type patchSection struct {
	path          string
	before, after string
	deleted       bool
}

// maxDiffViewBytes is the largest patch fed through DiffView; anything bigger
// falls back to the plain colorized block rather than re-diffing megabytes
// every frame.
const maxDiffViewBytes = 128 * 1024

// hunkGapMark stands in for the lines a patch skipped between hunks. It is
// inserted into both fragments so DiffView shows it as one context row.
const hunkGapMark = "⋯"

// parsePatchSections reconstructs per-file before/after fragments from the
// apply_patch format. It returns nil when the text does not look like one.
func parsePatchSections(patch string) []patchSection {
	var sections []patchSection
	current := -1
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		header, isHeader := strings.CutPrefix(line, "*** ")
		if !isHeader && !strings.HasPrefix(line, "***") {
			if current >= 0 {
				appendPatchLine(&sections[current], line, &inHunk)
			}
			continue
		}
		current, inHunk = -1, false
		// Add starts in a hunk (every line is new); Update waits for @@; Delete has no body.
		if action, path, ok := strings.Cut(header, " File: "); ok && slices.Contains([]string{"Add", "Update", "Delete"}, action) {
			sections = append(sections, patchSection{path: strings.TrimSpace(path), deleted: action == "Delete"})
			if action != "Delete" {
				current, inHunk = len(sections)-1, action == "Add"
			}
		}
	}
	return sections
}

func appendPatchLine(s *patchSection, line string, inHunk *bool) {
	if strings.HasPrefix(line, "@@") {
		if *inHunk {
			s.before += hunkGapMark + "\n"
			s.after += hunkGapMark + "\n"
		}
		*inHunk = true
		return
	}
	switch {
	case strings.HasPrefix(line, "+"):
		s.after += line[1:] + "\n"
	case strings.HasPrefix(line, "-"):
		s.before += line[1:] + "\n"
	default:
		s.before += strings.TrimPrefix(line, " ") + "\n"
		s.after += strings.TrimPrefix(line, " ") + "\n"
	}
}

// diffViewMemo keeps one DiffView per file section so its per-width render
// cache survives across frames (all setters no-op on unchanged values).
var diffViewMemo = map[string]*tuikit.DiffView{}

func diffViewFor(s patchSection) *tuikit.DiffView {
	key := s.path + "\x00" + s.before + "\x00" + s.after
	if dv, ok := diffViewMemo[key]; ok {
		return dv
	}
	if len(diffViewMemo) > 32 {
		clear(diffViewMemo)
	}
	dv := tuikit.NewDiffView(ui.Invokr.Palette).Before(s.path, s.before).After(s.path, s.after).
		Highlighter(tuikit.ChromaHighlighter(chromaTheme())).Tint(ui.Invokr.DiffAddBg, ui.Invokr.DiffDelBg)
	diffViewMemo[key] = dv
	return dv
}

// resetDiffViews drops the memo so a palette or syntax-theme change repaints.
func resetDiffViews() { clear(diffViewMemo) }

// renderGitDiff renders unified git diff output through DiffView, falling back
// to the plain colorized block when it does not parse or is too large.
func renderGitDiff(diff string, width, maxLines int) string {
	files := tuikit.ParseUnifiedDiff(diff)
	sections := make([]patchSection, len(files))
	for i, file := range files {
		sections[i] = patchSection{path: file.Path, before: file.Before, after: file.After, deleted: file.Deleted}
	}
	return renderDiffSections(sections, diff, width, maxLines)
}

// renderPatchDiff renders an applied patch under its tool line. maxLines caps
// the whole block (0 = uncapped, the expanded form); a multi-file patch
// spends the budget top to bottom and folds the rest behind an indicator.
func renderPatchDiff(patch string, width, maxLines int) string {
	return renderDiffSections(parsePatchSections(patch), patch, width, maxLines)
}

// renderEditDiff renders an edit from its whole file when editedFile could
// rebuild it, and from the patch fragments otherwise.
func renderEditDiff(file patchSection, patch string, width, maxLines int) string {
	if file.path == "" {
		return renderPatchDiff(patch, width, maxLines)
	}
	return renderDiffSections([]patchSection{file}, patch, width, maxLines)
}

// editedFile rebuilds an applied edit's file before and after: after is the
// file on disk, before is it with each replacement undone, last first. It
// gives up (empty section) when the file cannot be read or a replacement's new
// text is not in it, e.g. the file changed again since.
func editedFile(name, argsJSON string) patchSection {
	var fields struct {
		Path  string       `json:"path"`
		Edits []editCallOp `json:"edits"`
	}
	if name != "edit" || json.Unmarshal([]byte(argsJSON), &fields) != nil || fields.Path == "" || len(fields.Edits) == 0 {
		return patchSection{}
	}
	data, err := os.ReadFile(fields.Path)
	if err != nil {
		return patchSection{}
	}
	after := string(data)
	before := after
	for _, op := range slices.Backward(fields.Edits) {
		i := strings.LastIndex(before, op.New)
		if op.New == "" || i < 0 {
			return patchSection{}
		}
		before = before[:i] + op.Old + before[i+len(op.New):]
	}
	return patchSection{path: fields.Path, before: before, after: after}
}

func renderDiffSections(sections []patchSection, raw string, width, maxLines int) string {
	if len(sections) == 0 || len(raw) > maxDiffViewBytes {
		return renderDiffBlock(raw, width, maxLines)
	}
	var out []string
	budget := maxLines
	for i, s := range sections {
		if maxLines > 0 && budget <= 0 {
			out = append(out, foldIndicator(len(sections)-i, width)+" "+ui.Invokr.Muted.Render("(files)"))
			break
		}
		block := renderPatchSection(s, width, budget)
		out = append(out, block)
		budget -= strings.Count(block, "\n") + 1
	}
	return strings.Join(out, "\n")
}

// renderPatchSection is one file's diff: a "path +N −M" header over the
// tinted unified body.
func renderPatchSection(s patchSection, width, budget int) string {
	header := "    " + ui.Invokr.Text.Bold(true).Render(s.path)
	if s.deleted {
		return header + " " + ui.Invokr.Danger.Render("(deleted)")
	}
	adds, dels := lineDelta(s.before, s.after)
	header += " " + ui.Invokr.Success.Render(fmt.Sprintf("+%d", adds)) + " " + ui.Invokr.Danger.Render(fmt.Sprintf("−%d", dels))
	dv := diffViewFor(s)
	dv.MaxLines(max(0, budget-1))
	body := dv.Render(max(20, width-4))
	return header + "\n" + indentBlock(body, "    ")
}

// lineDelta counts the lines added and removed between two texts.
func lineDelta(before, after string) (adds, dels int) {
	dmp := diffmatchpatch.New()
	a, b, index := dmp.DiffLinesToChars(before, after)
	for _, d := range dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), index) {
		n := strings.Count(d.Text, "\n")
		if !strings.HasSuffix(d.Text, "\n") && d.Text != "" {
			n++
		}
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			adds += n
		case diffmatchpatch.DiffDelete:
			dels += n
		}
	}
	return adds, dels
}

// renderDiffBlock is the pre-DiffView plain fallback: colorized +/- lines,
// capped at maxLines (0 = uncapped). It also serves text that does not parse
// as an apply_patch patch.
func renderDiffBlock(diff string, width, maxLines int) string {
	return strings.Join(cappedLines(diff, width, maxLines, renderApprovalDetailLine), "\n")
}
