package cmd

import (
	"github.com/mheers/pulumi-helper/helpers"
	"github.com/mheers/pulumi-helper/stack"
	"github.com/spf13/cobra"
)

var (
	stackSetCmd = &cobra.Command{
		Use:     "set [name]",
		Short:   `sets the current stack`,
		Aliases: []string{"s", "select"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Set the log level
			helpers.SetLogLevel(LogLevelFlag)

			dieIfNotPulumiProject()

			if len(args) != 1 {
				return cmd.Help()
			}
			name := args[0]

			return stack.SetStack(name)
		},
	}
)
