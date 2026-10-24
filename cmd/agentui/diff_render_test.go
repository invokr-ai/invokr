package agentui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const multiFilePatch = "*** Begin Patch\n" +
	"*** Update File: a.go\n@@\n ctx\n-old\n+new\n" +
	"*** Add File: b.txt\n+hello\n+world\n" +
	"*** Delete File: c.txt\n" +
	"*** End Patch"

func TestParsePatchSections(t *testing.T) {
	sections := parsePatchSections(multiFilePatch)
	if len(sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(sections))
	}
	if sections[0].path != "a.go" || sections[0].before != "ctx\nold\n" || sections[0].after != "ctx\nnew\n" {
		t.Fatalf("update section = %+v", sections[0])
	}
	if sections[1].path != "b.txt" || sections[1].before != "" || sections[1].after != "hello\nworld\n" {
		t.Fatalf("add section = %+v", sections[1])
	}
	if !sections[2].deleted || sections[2].path != "c.txt" {
		t.Fatalf("delete section = %+v", sections[2])
	}
	if parsePatchSections("just some text\nno patch here") != nil {
		t.Fatal("non-patch text produced sections")
	}
}

func TestParsePatchMarksHunkGaps(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n@@\n-x\n+y\n@@\n-p\n+q\n*** End Patch"
	s := parsePatchSections(patch)[0]
	if !strings.Contains(s.before, hunkGapMark) || !strings.Contains(s.after, hunkGapMark) {
		t.Fatalf("hunk gap not marked: %+v", s)
	}
}

func TestRenderPatchDiffGolden(t *testing.T) {
	got := ansi.Strip(renderPatchDiff(multiFilePatch, 80, 0))
	for _, want := range []string{"a.go", "b.txt", "c.txt", "(deleted)", "- old", "+ new", "+ hello"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered diff missing %q:\n%s", want, got)
		}
	}
}

func TestRenderPatchDiffCapsAndFolds(t *testing.T) {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n*** Update File: big.go\n@@\n")
	for range maxDiffLines + 20 {
		b.WriteString("+added line\n")
	}
	b.WriteString("*** Update File: second.go\n@@\n-a\n+b\n*** End Patch")
	got := ansi.Strip(renderPatchDiff(b.String(), 80, maxDiffLines))
	if !strings.Contains(got, "more line") {
		t.Fatalf("capped diff shows no truncation tail:\n%s", got)
	}
	if !strings.Contains(got, "▸ 1 more lines (files)") {
		t.Fatalf("second file not folded behind an indicator:\n%s", got)
	}
	full := ansi.Strip(renderPatchDiff(b.String(), 80, 0))
	if !strings.Contains(full, "second.go") || strings.Contains(full, "(files)") {
		t.Fatalf("uncapped render still folded:\n%s", full)
	}
}

func TestRenderPatchDiffFallsBackOnHugeInput(t *testing.T) {
	huge := "*** Begin Patch\n*** Update File: a.go\n@@\n+" +
		strings.Repeat("x", maxDiffViewBytes) + "\n*** End Patch"
	got := ansi.Strip(renderPatchDiff(huge, 80, maxDiffLines))
	if strings.Contains(got, "--- a.go") {
		t.Fatalf("huge patch went through DiffView")
	}
	if got == "" {
		t.Fatal("huge patch rendered nothing")
	}
}

// TestRenderPatchDiffIsUnified: one column under a "path +N −M" header, with
// no side-by-side split and no ---/+++/@@ lines, even when wide (R3).
func TestRenderPatchDiffIsUnified(t *testing.T) {
	got := ansi.Strip(renderPatchDiff(
		"*** Begin Patch\n*** Update File: a.go\n@@\n ctx\n-old\n+new\n*** End Patch", 130, 0))
	if !strings.HasPrefix(strings.TrimSpace(got), "a.go +1 −1") {
		t.Fatalf("header missing:\n%s", got)
	}
	for _, bad := range []string{"│", "---", "+++", "@@"} {
		if strings.Contains(got, bad) {
			t.Fatalf("unified render has %q:\n%s", bad, got)
		}
	}
}

// TestEditedFileNumbersLinesInTheFile: an edit deep in a file is numbered by
// its place there, not from 1 (R4).
func TestEditedFileNumbersLinesInTheFile(t *testing.T) {
	t.Chdir(t.TempDir())
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines[9] = "changed"
	if err := os.WriteFile("f.txt", []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := editedFile("edit", `{"path":"f.txt","edits":[{"oldText":"line 10","newText":"changed"}]}`)
	if !strings.Contains(file.before, "line 10") || strings.Contains(file.before, "changed") {
		t.Fatalf("before not rebuilt: %q", file.before)
	}
	got := ansi.Strip(renderEditDiff(file, "", 100, 0))
	if !strings.Contains(got, "10 - line 10") || !strings.Contains(got, "10 + changed") {
		t.Fatalf("lines not numbered by file position:\n%s", got)
	}
}
