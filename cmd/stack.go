package cmd

import (
	"github.com/mheers/pulumi-helper/helpers"
	"github.com/mheers/pulumi-helper/stack"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	stackCmd = &cobra.Command{
		Use:     "stacks",
		Aliases: []string{"stack", "st", "s"},
		Short:   `manages stacks`,
		RunE: func(cmd *cobra.Command, args []string) error {
			helpers.PrintInfo()
			cmd.Help()
			return nil
		},
	}
)

func init() {
	stackCmd.AddCommand(stackNameCmd)
	stackCmd.AddCommand(stackListCmd)
	stackCmd.AddCommand(stackSetCmd)
}

func dieIfNotPulumiProject() {
	if !stack.IsPulumiProject() {
		logrus.Fatal("Not a Pulumi project (no Pulumi.yaml file found)")
	}
}
