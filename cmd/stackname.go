package cmd

import (
	"fmt"

	"github.com/mheers/pulumi-helper/helpers"
	"github.com/mheers/pulumi-helper/stack"
	"github.com/spf13/cobra"
)

var (
	ignoreError  bool
	stackNameCmd = &cobra.Command{
		Use:     "name",
		Aliases: []string{"n"},
		Short:   `returns the current stack name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Set the log level
			helpers.SetLogLevel(LogLevelFlag)

			if !ignoreError {
				dieIfNotPulumiProject()
			}

			stackName, err := stack.StackName()
			fmt.Printf("%s", stackName)

			if err != nil && !ignoreError {
				return err
			}

			return nil
		},
	}
)

func init() {
	stackNameCmd.Flags().BoolVarP(&ignoreError, "ignore-error", "i", false, "ignore error if not in a Pulumi project")
}
