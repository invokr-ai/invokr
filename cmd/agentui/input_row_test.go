package agentui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/bubbles/v2/textarea"
)

// TestCursorDisplayRowCountsWrappedAndEarlierLines: the caret row is the
// wrapped rows above the cursor in its own line plus every row of the lines
// before it.
func TestCursorDisplayRowCountsWrappedAndEarlierLines(t *testing.T) {
	ta := textarea.New()
	ta.SetWidth(20)
	ta.SetHeight(20)
	ta.SetValue("short\n" + strings.Repeat("word ", 12))
	want := 1 + ta.LineInfo().RowOffset
	if want < 2 {
		t.Fatalf("setup: second line should wrap, RowOffset = %d", ta.LineInfo().RowOffset)
	}
	if got := cursorDisplayRow(ta); got != want {
		t.Fatalf("cursorDisplayRow = %d, want %d", got, want)
	}
	ta.SetValue("one line")
	if got := cursorDisplayRow(ta); got != 0 {
		t.Fatalf("single-row draft: cursorDisplayRow = %d, want 0", got)
	}
}

// TestInputAreaRedrawsAfterTypingAndThemeChange: the memoised box follows the
// draft and a theme change.
func TestInputAreaRedrawsAfterTypingAndThemeChange(t *testing.T) {
	m := newModel(context.Background(), &fakeSession{}, nil, nil, nil, false, nil, nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	before := m.inputArea()
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if after := m.inputArea(); after == before || !strings.Contains(after, "x") {
		t.Fatalf("box did not follow the draft: %q", after)
	}
	m.inputMemo.view = "stale"
	m.applyTheme()
	if m.inputArea() == "stale" {
		t.Fatal("box was not redrawn after a theme change")
	}
}
