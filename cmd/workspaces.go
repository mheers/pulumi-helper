package cmd

import (
	"github.com/mheers/pulumi-helper/helpers"
	"github.com/spf13/cobra"
)

var (
	workspacesCmd = &cobra.Command{
		Use:     "workspaces",
		Aliases: []string{"workspace", "ws", "w"},
		Short:   `manages workspaces`,
		RunE: func(cmd *cobra.Command, args []string) error {
			helpers.PrintInfo()
			cmd.Help()
			return nil
		},
	}
)

func init() {
	workspacesCmd.AddCommand(workspacesListCmd)
}
