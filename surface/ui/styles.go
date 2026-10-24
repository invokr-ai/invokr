package ui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/antonikliment/tuikit"
)

// Theme is the app's rendering palette: a tuikit.Theme every tuikit component
// draws from, plus the lipgloss styles the hand-rolled renderers use. The two
// are always derived from the same colors — Palette is the source, the styles
// are views onto it.
type Theme struct {
	Palette tuikit.Theme

	Frame   lipgloss.Style // panel borders, dividers, the prompt marker
	Accent  lipgloss.Style // brand, user turns, selection
	Danger  lipgloss.Style // failures, denials
	Muted   lipgloss.Style // metadata, hints
	Text    lipgloss.Style // assistant body copy
	Success lipgloss.Style // completed tools
	Info    lipgloss.Style // commands, running state
	Warn    lipgloss.Style // section headers, approvals
	// Thinking is the model's reasoning and the narration it writes before a
	// tool call: dimmer than an answer and set behind a gutter, so it never
	// reads as a reply.
	Thinking lipgloss.Style
	// Notice is invokr talking, not the model: mode changes, approval
	// decisions, command feedback.
	Notice lipgloss.Style

	// SelectedBg fills the cursor row in the pickers and completion popup —
	// the one place the design paints a background, because a foreground
	// change alone does not read as "this row is the selection".
	SelectedBg color.Color
	// ChipBg fills inline code chips (a shell command, a filename).
	ChipBg color.Color

	Gradient []color.Color
}

// Invokr is the live theme. It is rebuilt once when the terminal reports its
// background color (see cmd/agentui: tea.RequestBackgroundColor), then read
// from the single-threaded Bubble Tea loop.
//
// ponytail: a reassigned global rather than a value threaded through every
// render function — 60-odd call sites, one writer, one goroutine.
var Invokr = NewTheme(true)

// NewTheme builds the palette for a dark or light terminal background. The dark
// values are the design's; the light ones are their higher-contrast
// counterparts for the same hues.
func NewTheme(isDark bool) Theme {
	d := lipgloss.LightDark(isDark)
	palette := tuikit.Theme{
		Brand:       d(lipgloss.Color("#2f5fd0"), lipgloss.Color("#4D7AE3")),
		Blue:        d(lipgloss.Color("#6d3fd4"), lipgloss.Color("#8B5EE5")),
		Yellow:      d(lipgloss.Color("#9a6b00"), lipgloss.Color("#e8c15a")),
		Green:       d(lipgloss.Color("#2f7d4f"), lipgloss.Color("#6ec28a")),
		Red:         d(lipgloss.Color("#c0392b"), lipgloss.Color("#e06a6a")),
		Cyan:        d(lipgloss.Color("#0f7285"), lipgloss.Color("#6ec8d8")),
		Muted:       d(lipgloss.Color("#6b7280"), lipgloss.Color("#5d6577")),
		TabActiveFg: d(lipgloss.Color("#ffffff"), lipgloss.Color("#0b0d13")),
		FocusBorder: d(lipgloss.Color("#c7ccd9"), lipgloss.Color("#232838")),
	}

	return Theme{
		Palette: palette,
		Frame:   fg(palette.FocusBorder),
		Accent:  fg(palette.Brand).Bold(true),
		Danger:  fg(palette.Red).Bold(true),
		Muted:   fg(palette.Muted),
		Text:    fg(d(lipgloss.Color("#1f2430"), lipgloss.Color("#c9cfdd"))),
		Success: fg(palette.Green),
		Info:    fg(palette.Blue),
		Warn:    fg(palette.Yellow),

		Thinking: fg(palette.Muted).Italic(true),
		Notice:   fg(palette.Cyan),

		SelectedBg: d(lipgloss.Color("#ece6fb"), lipgloss.Color("#1c1830")),
		ChipBg:     d(lipgloss.Color("#eceef4"), lipgloss.Color("#161b27")),

		Gradient: []color.Color{
			d(lipgloss.Color("#6d3fd4"), lipgloss.Color("#b98cff")),
			d(lipgloss.Color("#5a4bd2"), lipgloss.Color("#a396ff")),
			d(lipgloss.Color("#4655d1"), lipgloss.Color("#8d9cff")),
			d(lipgloss.Color("#2f5fd0"), lipgloss.Color("#7aa2ff")),
		},
	}
}

func fg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// Hex renders a palette color as "#rrggbb", for libraries that take colors as
// strings rather than as color.Color — glamour's ansi.StyleConfig is the one
// that matters here.
func Hex(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// Pill renders a short label on a filled background in the given style — the
// env and model badges on the banner, an approval's tier tag.
func (t Theme) Pill(style lipgloss.Style, s string) string {
	return style.Background(t.ChipBg).Render(" " + s + " ")
}

// Chip renders inline code — a command, a path — on a filled background.
func (t Theme) Chip(s string) string { return t.Pill(t.Text, s) }

func (t Theme) GradientText(s string) string {
	runes := []rune(s)
	if len(runes) == 0 || len(t.Gradient) == 0 {
		return t.Accent.Render(s)
	}

	var b strings.Builder
	for i, r := range runes {
		color := t.Gradient[(i*len(t.Gradient))/len(runes)]
		b.WriteString(lipgloss.NewStyle().Foreground(color).Bold(true).Render(string(r)))
	}
	return b.String()
}
