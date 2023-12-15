package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"golang.org/x/exp/maps"
)

func List() ([]State, error) {
	states, err := GetStates()
	if err != nil {
		return nil, err
	}

	var result []State
	for _, state := range states {
		result = append(result, state)
	}

	return result, nil
}

func stateDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	pulumiDir := path.Join(homeDir, ".pulumi")
	stateDir := path.Join(pulumiDir, "stacks")
	return stateDir, nil
}

func GetState(name string) (*State, error) {
	states, err := GetStates()
	if err != nil {
		return nil, err
	}

	state, ok := states[name]
	if !ok {
		return nil, fmt.Errorf("state %s not found", name)
	}

	return &state, nil
}

func GetStates() (map[string]State, error) {
	stateDir, err := stateDir()
	if err != nil {
		return nil, err
	}

	stateFiles, err := findStateFiles(stateDir)
	if err != nil {
		return nil, err
	}

	states, err := getStatesMap(stateFiles)
	if err != nil {
		return nil, err
	}

	return states, nil
}

type State struct {
	Name     string
	FileName string
	Path     string
	ModTime  time.Time
}

func (s *State) Outputs() (map[string]gjson.Result, error) {

	jsonB, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}

	jsonS := string(jsonB)

	resources := gjson.Get(jsonS, "checkpoint.latest.resources").Array()

	stackResource := resources[0].Map()
	if stackResource["type"].String() != "pulumi:pulumi:Stack" {
		return nil, fmt.Errorf("stack resource not found")
	}
	r := stackResource["outputs"].Map()
	return r, nil
}

func (s *State) OutputKeys() ([]string, error) {
	outputs, err := s.Outputs()
	if err != nil {
		return nil, err
	}
	keys := maps.Keys[map[string]gjson.Result](outputs)
	return keys, nil
}

func (s *State) GetOutput(name string, result interface{}) error {

	outputs, err := s.Outputs()
	if err != nil {
		return err
	}

	rawJSON := outputs[name].Raw

	err = json.Unmarshal([]byte(rawJSON), &result)
	if err != nil {
		return err
	}

	return nil
}

func getStatesMap(stateFiles []State) (map[string]State, error) {
	states := make(map[string]State)
	for _, stateFile := range stateFiles {
		states[stateFile.Name] = stateFile
	}
	return states, nil
}

func findStateFiles(dir string) ([]State, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var fileNames []State
	for _, file := range files {
		stateName := file.Name()
		path := dir + "/" + file.Name()
		info, err := file.Info()
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(stateName, ".json") {
			continue
		}

		stateName = strings.TrimSuffix(stateName, ".json")

		fileNames = append(fileNames,
			State{
				Name:     stateName,
				FileName: file.Name(),
				Path:     path,
				ModTime:  info.ModTime(),
			},
		)
	}
	return fileNames, nil
}
