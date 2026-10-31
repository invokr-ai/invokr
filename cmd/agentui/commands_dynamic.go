package agentui

import (
	"context"
	"slices"
	"strings"

	"github.com/invokr-agent/invokr/platform/turn"

	tea "charm.land/bubbletea/v2"
)

// DynamicCommand mirrors runtime.CustomCommand without importing runtime:
// a slash command declared by a tool (kind "tool") or a skill (kind "skill").
type DynamicCommand struct {
	Name, Usage, Summary, Kind string
}

// RefreshDynamicCommands, when set (by the CLI wiring), re-derives the dynamic
// command batch from the backend. The completion menu calls it as it opens so
// skills installed mid-session appear without a restart.
var RefreshDynamicCommands func()

// dynamicNames tracks which registry entries came from RegisterDynamicCommands
// so a re-registration (via RefreshDynamicCommands) replaces them instead of
// stacking duplicates. Registration runs on the single Bubble Tea loop (or
// before it starts), so no locking is needed.
var dynamicNames = map[string]bool{}

// RegisterDynamicCommands appends tool- and skill-declared commands to the
// slash-command registry. Built-in names always win: a dynamic command whose
// name collides with a built-in (or an earlier dynamic entry in this batch)
// is skipped — the registry is the source of truth the user sees in /help.
// Skipped names are returned so the wiring can log them; completion and /help
// pick the new entries up automatically because both render from the registry.
func RegisterDynamicCommands(commands []DynamicCommand) (skipped []string) {
	commandRegistry = slices.DeleteFunc(commandRegistry, func(spec commandSpec) bool { return dynamicNames[spec.name] })
	dynamicNames = map[string]bool{}
	for _, command := range commands {
		if command.Name == "" || findCommand(command.Name) != nil {
			skipped = append(skipped, command.Name)
			continue
		}
		commandRegistry = append(commandRegistry, dynamicSpec(command))
		dynamicNames[command.Name] = true
	}
	return skipped
}

func dynamicSpec(command DynamicCommand) commandSpec {
	spec := commandSpec{
		name:         command.Name,
		usage:        command.Usage,
		summary:      command.Summary,
		handler:      runBackendCommand,
		plain:        runPlainBackend,
		needsBackend: true,
	}
	if command.Kind == "skill" {
		spec.handler = runSkillCommand
		spec.plain = runPlainSkill
		spec.skill = true
	}
	return spec
}

// skillPromptMsg carries a loaded skill, ready to submit as a turn.
type skillPromptMsg struct {
	prompt string
	err    error
}

// runSkillCommand loads the skill's instructions through the backend and
// submits them as a user turn (plus any trailing words as the task), so the
// model actually follows the skill instead of the text landing as UI output.
func runSkillCommand(m *model, text string, args []string) tea.Cmd {
	if m.sending {
		m.commandMessage("wait for the current turn to finish")
		return nil
	}
	name, _, _ := parseCommand(text)
	return func() tea.Msg {
		instructions, err := m.commands.Execute(m.ctx, name, nil)
		return skillPromptMsg{prompt: skillPrompt(instructions, args), err: err}
	}
}

// runPlainSkill prints the skill's instructions. Plain mode is line-oriented
// with no way to substitute the typed command for a different prompt, so the
// turn-submitting behavior stays interactive-only for now.
func runPlainSkill(ctx context.Context, backend CommandBackend, session turn.Session, name string, _ []string) (string, bool) {
	out, _ := runPlainBackend(ctx, backend, session, name, nil)
	return out + "\n\n(interactive mode submits this skill as a turn)", true
}

func skillPrompt(instructions string, args []string) string {
	prompt := "Follow this skill:\n\n" + strings.TrimSpace(instructions)
	if task := strings.TrimSpace(strings.Join(args, " ")); task != "" {
		prompt += "\n\nTask: " + task
	}
	return prompt
}

// updateSkillMsg fans the skill-related messages back out; folded into one
// Update case to stay inside the gocyclo budget.
func (m *model) updateSkillMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case skillPromptMsg:
		return m.updateSkillPrompt(msg)
	case hubSkillsMsg:
		return m.updateHubSkills(msg)
	}
	return m, nil
}

func (m *model) updateSkillPrompt(msg skillPromptMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.commandMessage("error: " + msg.err.Error())
		return m, nil
	}
	// submit refuses while an approval is pending, the log panel is open, or a
	// turn is in flight; bail with feedback instead of silently overwriting
	// whatever the user has typed with the skill prompt.
	if (m.approvalQueue != nil && m.approvalQueue.Len() > 0) || m.logPanel.mode == modeLogs || m.sending {
		m.commandMessage("skill not started — finish the pending approval, log view, or turn first")
		return m, nil
	}
	draft := m.input.Value()
	m.input.SetValue(msg.prompt)
	next, cmd := m.submit()
	if cmd == nil {
		next.input.SetValue(draft)
	}
	return next, cmd
}
