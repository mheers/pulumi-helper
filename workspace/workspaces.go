package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

func List() ([]Workspace, error) {
	workspaces, err := GetWorkspaces()
	if err != nil {
		return nil, err
	}

	var result []Workspace
	for _, workspace := range workspaces {
		result = append(result, workspace)
	}

	return result, nil
}

func GetWorkspaces() (map[string]Workspace, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	pulumiDir := path.Join(homeDir, ".pulumi")
	workspaceDir := path.Join(pulumiDir, "workspaces")

	workspaceFiles, err := findWorkspaceFiles(workspaceDir)
	if err != nil {
		return nil, err
	}

	workspaces, err := getWorkspacesMap(workspaceFiles)
	if err != nil {
		return nil, err
	}

	return workspaces, nil
}

type WorkspaceFile struct {
	Name    string
	Path    string
	ModTime time.Time
}

type Workspace struct {
	File  WorkspaceFile
	Name  string
	Hash  string
	Stack string
}

func (w *Workspace) SetStack(name string) error {
	value := map[string]string{
		"stack": name,
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	err = os.WriteFile(w.File.Path, data, 0644)
	if err != nil {
		return err
	}

	w.Stack = name
	return nil
}

func (w *Workspace) initStack() error {
	data, err := os.ReadFile(w.File.Path)
	if err != nil {
		return err
	}

	// json:
	// {
	// 	"stack": "yaml"
	// }
	result := make(map[string]string)
	err = json.Unmarshal(data, &result)
	if err != nil {
		return err
	}

	stack, ok := result["stack"]
	if !ok {
		return fmt.Errorf("stack not found")
	}

	w.Stack = stack
	return nil
}

func getWorkspacesMap(files []WorkspaceFile) (map[string]Workspace, error) {
	workspaces, err := getWorkspaces(files)
	if err != nil {
		return nil, err
	}

	workspacesMap := make(map[string]Workspace)
	for _, workspace := range workspaces {
		if _, ok := workspacesMap[workspace.Name]; ok {
			if !workspacesMap[workspace.Name].File.ModTime.Before(workspace.File.ModTime) {
				continue
			}
		}
		workspacesMap[workspace.Name] = workspace
	}
	return workspacesMap, nil
}

func getWorkspaces(files []WorkspaceFile) ([]Workspace, error) {
	var workspaces []Workspace
	for _, file := range files {
		workspaceName, hash, err := getWorkspaceNameAndHashFromFile(file.Name)
		if err != nil {
			return nil, err
		}
		ws := Workspace{
			file,
			workspaceName,
			hash,
			"",
		}
		err = ws.initStack()
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}
	return workspaces, nil
}

func findWorkspaceFiles(dir string) ([]WorkspaceFile, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var fileNames []WorkspaceFile
	for _, file := range files {
		workspaceName := file.Name()
		path := dir + "/" + file.Name()
		info, err := file.Info()
		if err != nil {
			return nil, err
		}
		fileNames = append(fileNames,
			WorkspaceFile{
				workspaceName,
				path,
				info.ModTime(),
			},
		)
	}
	return fileNames, nil
}

func getWorkspaceNameAndHashFromFile(file string) (string, string, error) {
	// e.g. pulumi-demo-049bc369530d2f05a8ba2cdbbb49164cfd3ba066-workspace.json leads to pulumi-demo and 049bc369530d2f05a8ba2cdbbb49164cfd3ba066
	// e.g. pulumi-demo-workspace.json leads to pulumi-demo and ""

	// remove the -workspace.json
	workspaceHashName := file[:len(file)-len("-workspace.json")]

	// remove the hash
	hash := strings.Split(workspaceHashName, "-")[len(strings.Split(workspaceHashName, "-"))-1]

	// remove the hash from the name
	workspaceName := strings.Replace(workspaceHashName, "-"+hash, "", 1)

	return workspaceName, hash, nil
}
