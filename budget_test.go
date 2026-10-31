package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	metrics "github.com/antonikliment/go-code-metrics/analysis"
)

// Per-area implementation LOC budgets.
//
// The goclocbudget linter enforces one repository-wide ceiling
// (.golangci.yml). That number cannot say *where* growth happened, so a TUI
// polish PR and a correctness fix compete for the same headroom — see
// docs/private/reviews/2026-08-25-loc-vs-peer-harnesses.md. These per-area budgets are
// the local ceilings underneath it: the repo cap is still the hard aggregate
// limit, and no single area may grow into everyone else's room.
//
// The budgets deliberately sum above the repo cap. Both constraints bind: an
// area can be under its own budget and still be refused by the repo cap.
//
// Raising an entry is a normal reviewable change. Raising several at once, or
// raising the repo cap to fit, is the signal to stop and reconsider the shape
// of the change instead.
//
// Set INVOKR_BUDGET_REPORT=1 to print the current table instead of asserting,
// which is how the numbers below were calibrated.

// area is one budgeted slice of the tree. Prefixes are matched longest-first,
// so "cmd/agentui" wins over "cmd" without depending on declaration order.
type area struct {
	prefix string
	budget int
}

var budgets = []area{
	// Surfaces. The TUI is the thinnest subsystem against comparable
	// harnesses (3.8k here against crush's 27k), so it gets the most room:
	// the cap should not be what stops the TUI catching up.
	{"cmd/agentui", 6268},
	{"cmd", 2500},
	{"internal", 153},

	// Agent loop and tool plumbing — the core of the core.
		{"agent", 3833},
	{"tools/core", 1344},
	{"tools/system", 11320},
	{"tools", 600}, // tools/, tools/integrations

	// Capabilities. Memory is budgeted apart from the rest because it is by
	// far the largest and has its own extraction discussion on file
	// (docs/private/plans/capability-bundles.md).
	{"capability/memory", 4800},
	{"capability", 4700},

	// Platform, config, clients, surfaces, leaf helpers.
	{"platform", 2520},
	{"config", 1389},
	{"clients", 800},
	{"surface", 1600},
	{"support", 500},
	{"constants", 300},
}

// excludedDirs mirrors .golangci.yml's goclocbudget exclude-dirs so the two
// checks measure the same tree. Keep them in step.
var excludedDirs = []string{
	"vendor", ".git", "node_modules", "app/dist",
	"support/testutil", "cmd/e2ereport", "bench", ".claude",
}

func TestPerAreaLOCBudgets(t *testing.T) {
	byArea, unassigned := measure(t)

	if len(unassigned) > 0 {
		sort.Strings(unassigned)
		t.Errorf("no budget covers these paths — add an area to budgets: %s",
			strings.Join(unassigned, ", "))
	}

	if os.Getenv("INVOKR_BUDGET_REPORT") != "" {
		report(t, byArea)
		return
	}

	for _, a := range budgets {
		if got := byArea[a.prefix]; got > a.budget {
			t.Errorf("%s: %d implementation lines exceeds its budget of %d (over by %d)",
				a.prefix, got, a.budget, got-a.budget)
		}
	}
}

// measure aggregates implementation code lines per budgeted area, and reports
// any file that no area claims.
func measure(t *testing.T) (map[string]int, []string) {
	t.Helper()
	tree, err := metrics.Analyze(metrics.Options{
		Root:             ".",
		IncludeTests:     false,
		ExcludeGenerated: true,
		ExcludeDirs:      excludedDirs,
	})
	if err != nil {
		t.Fatalf("analyze repository: %v", err)
	}
	byArea := map[string]int{}
	var unassigned []string
	for _, f := range metrics.Files(tree) {
		rel := filepath.ToSlash(f.Path)
		// Files at the repository root (main.go) belong to no area and are
		// covered by the repo-wide cap alone.
		if !strings.Contains(rel, "/") {
			continue
		}
		if prefix, ok := areaFor(rel); ok {
			byArea[prefix] += f.Code
			continue
		}
		unassigned = append(unassigned, rel)
	}
	return byArea, unassigned
}

// areaFor returns the longest budgeted prefix containing rel.
func areaFor(rel string) (string, bool) {
	best := ""
	for _, a := range budgets {
		if rel == a.prefix || strings.HasPrefix(rel, a.prefix+"/") {
			if len(a.prefix) > len(best) {
				best = a.prefix
			}
		}
	}
	return best, best != ""
}

func report(t *testing.T, byArea map[string]int) {
	t.Helper()
	total, budgeted := 0, 0
	for _, a := range budgets {
		got := byArea[a.prefix]
		total += got
		budgeted += a.budget
		t.Logf("%-20s %6d / %6d  (%+d)", a.prefix, got, a.budget, a.budget-got)
	}
	t.Logf("%-20s %6d / %6d", "TOTAL", total, budgeted)
	fmt.Fprintf(os.Stderr, "budgeted areas total: %d implementation lines\n", total)
}
