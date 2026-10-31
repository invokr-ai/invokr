package cmd

import (
	"fmt"
	"slices"

	"github.com/invokr-agent/invokr/capability/approval"
	"github.com/invokr-agent/invokr/config"
	"github.com/spf13/cobra"
)

func startupMode(cmd *cobra.Command) (*approval.Policy, error) {
	f := cmd.Flags()
	mode, _ := f.GetString("mode")
	yolo, _ := f.GetBool("yolo")
	publish, _ := f.GetBool("allow-publishing")
	requested := slices.ContainsFunc([]string{"mode", "yolo", "allow-publishing"}, f.Changed)
	if cmd.Name() == "acp" {
		// Chats start in manual; each gets its own copy, carrying the standing rules.
		return approval.NewPolicy(approval.Manual, false), nil
	}
	if !isAgentCommand(cmd) && cmd.Name() != "exec" {
		return nil, nil // other commands do not register the session flags
	}
	if !slices.Contains([]string{"manual", "auto", "plan"}, mode) {
		return nil, fmt.Errorf("--mode must be manual, auto, or plan; use --yolo for unrestricted execution")
	}
	if requested && (f.Changed("allow") || f.Changed("allow-all")) {
		return nil, fmt.Errorf("session mode flags cannot be combined with legacy --allow/--allow-all")
	}
	if yolo {
		if f.Changed("mode") || f.Changed("sandbox") {
			return nil, fmt.Errorf("--yolo conflicts with --mode and --sandbox")
		}
		mode = string(approval.YOLO)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: "+approval.YOLOWarning)
	}
	return approval.NewPolicy(approval.Mode(mode), publish), nil
}

// ConfigureStartupModes runs before Docker relaunch. Only flags before -- count;
// a prompt containing the text --yolo must never disable the sandbox.
func ConfigureStartupModes(cfg *config.AppConfig, args []string) error {
	root, _ := NewRootCmd(*cfg)
	command, remaining, err := root.Find(args)
	if err != nil {
		return err
	}
	command.InitDefaultHelpFlag()
	command.InitDefaultVersionFlag()
	if err := command.ParseFlags(remaining); err != nil {
		return err
	}
	f := command.Flags()
	yolo, _ := f.GetBool("yolo")
	if yolo && (f.Changed("sandbox") || f.Changed("mode")) {
		return fmt.Errorf("--yolo conflicts with --mode and --sandbox")
	}
	if yolo {
		cfg.Sandbox.Enabled = false
	}
	return nil
}
