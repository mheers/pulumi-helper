package cmd

import (
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/mheers/pulumi-helper/helpers"
	"github.com/mheers/pulumi-helper/stack"
	"github.com/spf13/cobra"
)

var (
	stackListCmd = &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "l", "ps"},
		Short:   `lists all stacks in the current workspace`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Set the log level
			helpers.SetLogLevel(LogLevelFlag)

			dieIfNotPulumiProject()

			stacks, err := stack.List()
			if err != nil {
				return err
			}

			return renderStacks(stacks)
		},
	}
)

func renderStacks(stacks []stack.Stack) error {
	if OutputFormatFlag == "table" {
		renderStackListTable(stacks)
	}
	if OutputFormatFlag == "json" {
		err := helpers.PrintJSON(stacks)
		if err != nil {
			return err
		}
	}
	if OutputFormatFlag == "yaml" {
		err := helpers.PrintYAML(stacks)
		if err != nil {
			return err
		}
	}
	if OutputFormatFlag == "csv" {
		err := helpers.PrintCSV(stacks)
		if err != nil {
			return err
		}
	}
	return nil
}

func renderStackListTable(stacks []stack.Stack) {
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendHeader(table.Row{"Name"})
	for _, stack := range stacks {
		t.AppendRow(
			table.Row{
				stack.Name,
			},
		)

		t.AppendSeparator()
	}
	t.Render()
}
