package agentui

import (
	"context"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/invokr-agent/invokr/platform/turn"
	toolcore "github.com/invokr-agent/invokr/tools/core"
)

// HeadlessCommand is a slash command that runs without the TUI, for clients
// such as ACP chats.
type HeadlessCommand struct{ Name, Usage, Summary string }

// headlessAllowed are the built-ins a chat may run: none acts on the host
// without the model and its approvals. false means the bare listing only,
// so /jobs cannot run a job nor /skills install one. Skills are allowed too:
// they become a prompt. Tool commands are not: they would run outside the
// chat's own approvals.
var headlessAllowed = map[string]bool{
	"help": true, "usage": true, "compact": true, "model": true, "reasoning": true,
	"profile": true, "tools": true, "agents": true, "jobs": false, "skills": false,
}

func headless(spec *commandSpec) bool {
	_, builtin := headlessAllowed[spec.name]
	return spec.plain != nil && (builtin || spec.skill)
}

func HeadlessCommands() []HeadlessCommand {
	var out []HeadlessCommand
	for _, spec := range commandRegistry {
		if headless(&spec) {
			usage := spec.usage
			if !headlessAllowed[spec.name] && !spec.skill {
				usage = "/" + spec.name
			}
			out = append(out, HeadlessCommand{spec.name, usage, spec.summary})
		}
	}
	return out
}

// RunHeadless runs a headless slash command. A skill yields the prompt to
// send as a turn instead of a reply; handled is false for text that is not a
// headless command, which then goes to the model unchanged.
func RunHeadless(ctx context.Context, backend CommandBackend, session turn.Session, text string) (reply, prompt string, handled bool) {
	name, args, slash := parseCommand(text)
	spec := findCommand(name)
	if !slash || spec == nil || !headless(spec) {
		return "", "", false
	}
	if withArgs, builtin := headlessAllowed[name]; builtin && !withArgs && len(args) > 0 {
		return "/" + name + " takes no arguments here; use the terminal UI", "", true
	}
	if name == "help" {
		return headlessHelp(), "", true
	}
	if spec.skill {
		instructions, err := backend.Execute(ctx, name, nil)
		if err != nil {
			return "error: " + err.Error(), "", true
		}
		return "", skillPrompt(instructions, args), true
	}
	out, _ := spec.plain(ctx, backend, session, name, args)
	return ansi.Strip(out), "", true
}

// headlessHelp lists only what a headless client can run, one line each:
// chat apps wrap the TUI's two-column table badly.
func headlessHelp() string {
	lines := toolcore.MapSlice(HeadlessCommands(), func(c HeadlessCommand) string { return c.Usage + " — " + c.Summary })
	return strings.Join(lines, "\n")
}
