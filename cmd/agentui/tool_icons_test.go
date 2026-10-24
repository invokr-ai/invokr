package agentui

import (
	"strings"
	"testing"
)

// A tool with no primaryArgKeys entry falls back to firstStringField. That
// fallback must not depend on Go's randomised map iteration order, or the
// preview chip flickers between fields on every re-render.
func TestPrimaryArgReadPaths(t *testing.T) {
	if got := primaryArg("read", `{"paths":["src/main.go","README.md"],"offset":4}`); got != "src/main.go, README.md" {
		t.Fatalf("read preview = %q", got)
	}
}

func TestPrimaryArgFallbackIsStable(t *testing.T) {
	const args = `{"action":"kill","session":"m-a82a75","target":"pane-0"}`

	first := primaryArg("process", args)
	if first == "" {
		t.Fatalf("primaryArg returned empty preview for %s", args)
	}
	for i := 0; i < 200; i++ {
		if got := primaryArg("process", args); got != first {
			t.Fatalf("primaryArg unstable: got %q on iteration %d, want %q", got, i, first)
		}
	}
}

// create_tool carries several long string fields; the chip must name the tool
// being created rather than whichever field the fallback happens to reach.
func TestPrimaryArgCreateToolPrefersName(t *testing.T) {
	const args = `{"name":"current_time","description":"Return the current wall-clock time.","example":"current_time()","python_source":"def run():\n    pass\n"}`

	for i := 0; i < 200; i++ {
		if got := primaryArg("create_tool", args); got != "current_time" {
			t.Fatalf("primaryArg = %q on iteration %d, want %q", got, i, "current_time")
		}
	}
}

// TestTruncatePreviewKeepsCommandHead: a long shell command keeps its start;
// only a bare path keeps its tail.
func TestTruncatePreviewKeepsCommandHead(t *testing.T) {
	cmd := `go test -count=1 ./... 2>&1 | tail -12; echo "exit: ${PIPESTATUS[0]}"`
	if got := truncatePreview(cmd, 30); !strings.HasPrefix(got, "go test -count=1") || !strings.HasSuffix(got, "...") {
		t.Fatalf("command preview = %q, want its head kept", got)
	}
	if got := truncatePreview("/home/user/project/internal/pkg/file.go", 20); !strings.HasSuffix(got, "file.go") {
		t.Fatalf("path preview = %q, want its tail kept", got)
	}
}
