package main

import (
	_ "embed"
	"fmt"
	"k8slse/cmd"
	"os"
)

var version string

func main() {
	cmd.AppVersion = version
	if err := cmd.Execute(); err != nil {
		fmt.Print(err.Error())
		os.Exit(1)
	}
}
