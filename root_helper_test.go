package main

import (
	"os"
	"testing"
)

func TestRushHelperProcess(t *testing.T) {
	if os.Getenv("RUSH_TEST_HELPER_PROCESS") != "1" {
		return
	}

	for i, arg := range os.Args {
		if arg == "--" {
			RootCmd.SetArgs(os.Args[i+1:])
			Execute()
			os.Exit(0)
		}
	}
	os.Exit(2)
}
