package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/invokr-agent/acp"
	"github.com/spf13/cobra"

	"github.com/invokr-agent/invokr/cmd/agentui"
	"github.com/invokr-agent/invokr/platform/turn"
	"github.com/invokr-agent/invokr/surface/acpagent"
	toolcore "github.com/invokr-agent/invokr/tools/core"
)

func NewACPCmd(services ServicesProvider) *cobra.Command {
	return &cobra.Command{
		Use:          "acp",
		Short:        "Run the agent as an ACP (Agent Client Protocol) agent over stdio",
		GroupID:      groupServe,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := services()
			if err != nil {
				return err
			}
			if s.App.Log.Output == "stdout" {
				return fmt.Errorf("acp: stdout carries the protocol; set --log-output to stderr or a file")
			}
			protocol := cmd.OutOrStdout()
			os.Stdout = os.Stderr
			// Skills installed later need an invokr acp restart: the registry
			// is read by concurrent sessions, so it is filled once, here.
			registerCustomCommands(cmd.Context(), s.Commands, s.Logger)
			return acpagent.Serve(cmd.Context(), cmd.InOrStdin(), protocol, s.NewChatSession, acpCommands{s.Commands})
		},
	}
}

// acpCommands offers the TUI's headless slash commands to ACP chats.
type acpCommands struct{ backend agentui.CommandBackend }

func (c acpCommands) Available() []acp.AvailableCommand {
	return toolcore.MapSlice(agentui.HeadlessCommands(), func(cmd agentui.HeadlessCommand) acp.AvailableCommand {
		out := acp.AvailableCommand{Name: cmd.Name, Description: cmd.Summary}
		if _, args, ok := strings.Cut(cmd.Usage, " "); ok {
			out.Input = &acp.AvailableCommandInput{Hint: args}
		}
		return out
	})
}

func (c acpCommands) Run(ctx context.Context, sess turn.Session, text string) (string, string, bool) {
	return agentui.RunHeadless(ctx, c.backend, sess, text)
}
