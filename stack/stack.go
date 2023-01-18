package stack

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/mheers/pulumi-helper/workspace"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

var BaseDir = "."

type PulumiYaml struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Runtime     string `yaml:"runtime"`
}

type PulumiStackYaml struct {
	Encryptionsalt string            `yaml:"encryptionsalt"`
	Config         map[string]string `yaml:"config"`
}

type Stack struct {
	Name          string
	File          string
	Project       *PulumiYaml
	Configuration *PulumiStackYaml
}

func StackName() (string, error) {
	project, err := ProjectName()
	if err != nil {
		return "", err
	}

	spaces, err := workspace.GetWorkspaces()
	if err != nil {
		return "", err
	}

	space, ok := spaces[project]
	if !ok {
		return "", errors.New("no workspace found")
	}

	return space.Stack, nil
}

func SetStack(newStack string) error {
	// check if stack exists
	stacks, err := FindStacks(BaseDir)
	if err != nil {
		return err
	}

	found := false
	for _, stack := range stacks {
		if stack == newStack {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("stack %s not found", newStack)
	}

	project, err := ProjectName()
	if err != nil {
		return err
	}

	spaces, err := workspace.GetWorkspaces()
	if err != nil {
		return err
	}

	space, ok := spaces[project]
	if !ok {
		logrus.Fatal("no workspace found")
	}

	return space.SetStack(newStack)
}

func List() ([]Stack, error) {
	stacks, err := FindStacks(BaseDir)
	if err != nil {
		return nil, err
	}

	var result []Stack
	for _, stack := range stacks {
		stack, err := ReadStack(stack)
		if err != nil {
			return nil, err
		}
		result = append(result, *stack)
	}

	return result, nil
}

func FindStacks(dir string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var stacks []string
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "Pulumi.") && strings.HasSuffix(file.Name(), ".yaml") && file.Name() != "Pulumi.yaml" {
			stack := file.Name()

			// remove prefix
			stack = strings.Replace(stack, "Pulumi.", "", 1)

			// remove suffix by removing the last 5 characters
			stack = stack[:len(stack)-5]

			stacks = append(stacks, stack)
		}
	}
	return stacks, nil
}

func ReadStack(name string) (*Stack, error) {
	project, err := Project()
	if err != nil {
		return nil, err
	}
	configuration, err := ReadStackYaml(name)
	if err != nil {
		return nil, err
	}
	stack := &Stack{
		Name:          name,
		File:          fmt.Sprintf("Pulumi.%s.yaml", name),
		Project:       project,
		Configuration: configuration,
	}

	return stack, nil
}

func ReadStackYaml(name string) (*PulumiStackYaml, error) {
	// open file
	data, err := os.ReadFile(path.Join(BaseDir, fmt.Sprintf("Pulumi.%s.yaml", name)))
	if err != nil {
		return nil, err
	}

	p := &PulumiStackYaml{}
	err = yaml.Unmarshal(data, p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func WriteStackYaml(name string, stack *PulumiStackYaml) error {
	var b bytes.Buffer
	yamlEncoder := yaml.NewEncoder(&b)
	yamlEncoder.SetIndent(2)
	err := yamlEncoder.Encode(&stack)
	if err != nil {
		return err
	}

	err = os.WriteFile(path.Join(BaseDir, fmt.Sprintf("Pulumi.%s.yaml", name)), b.Bytes(), 0644)
	if err != nil {
		return err
	}
	return nil
}

func IsPulumiProject() bool {
	file := path.Join(BaseDir, "Pulumi.yaml")
	_, err := os.Stat(file)
	return err == nil
}

func Project() (*PulumiYaml, error) {
	if !IsPulumiProject() {
		return nil, errors.New("not a pulumi project")
	}

	// open file
	data, err := os.ReadFile(path.Join(BaseDir, "Pulumi.yaml"))
	if err != nil {
		return nil, err
	}

	// parse yaml
	p := &PulumiYaml{}
	err = yaml.Unmarshal(data, p)
	if err != nil {
		return nil, err
	}

	return p, err
}

func ProjectName() (string, error) {
	p, err := Project()
	if err != nil {
		return "", err
	}

	return p.Name, nil
}
