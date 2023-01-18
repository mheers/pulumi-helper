package main

import (
	"github.com/mheers/pulumi-helper/cmd"
	"github.com/sirupsen/logrus"
)

func main() {
	// execute the command
	err := cmd.Execute()
	if err != nil {
		logrus.Fatalf("Execute failed: %+v", err)
	}
}
