package cmd

import (
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/mheers/pulumi-helper/helpers"
	"github.com/mheers/pulumi-helper/workspace"
	"github.com/spf13/cobra"
)

var (
	workspacesListCmd = &cobra.Command{
		Use:     "list",
		Short:   "lists all workspaces",
		Aliases: []string{"ls", "ps"},
		Long:    ``,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Set the log level
			helpers.SetLogLevel(LogLevelFlag)

			spaces, err := workspace.List()
			if err != nil {
				return err
			}

			return renderWorkspaces(spaces)
		},
	}
)

func renderWorkspaces(spaces []workspace.Workspace) error {
	if OutputFormatFlag == "table" {
		renderWorkspaceListTable(spaces)
	}
	if OutputFormatFlag == "json" {
		err := helpers.PrintJSON(spaces)
		if err != nil {
			return err
		}
	}
	if OutputFormatFlag == "yaml" {
		err := helpers.PrintYAML(spaces)
		if err != nil {
			return err
		}
	}
	if OutputFormatFlag == "csv" {
		err := helpers.PrintCSV(spaces)
		if err != nil {
			return err
		}
	}
	return nil
}

func renderWorkspaceListTable(spaces []workspace.Workspace) {
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendHeader(table.Row{"Name", "Current Stack", "Modified"})
	for _, space := range spaces {
		t.AppendRow(
			table.Row{
				space.Name,
				space.Stack,
				space.File.ModTime,
			},
		)

		t.AppendSeparator()
	}
	t.Render()
}
