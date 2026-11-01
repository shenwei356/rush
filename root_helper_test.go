package main

import (
	"fmt"
	"net/http"
	"os"
	"testing"
)

func TestRushHelperProcess(t *testing.T) {
	if os.Getenv("RUSH_TEST_HELPER_PROCESS") != "1" {
		return
	}
	if mode := os.Getenv("RUSH_TEST_VERSION_RESPONSE"); mode != "" {
		http.DefaultTransport = versionTestTransport{mode: mode, closedFile: os.Getenv("RUSH_TEST_VERSION_BODY_CLOSED")}
	}
	if path := os.Getenv("RUSH_TEST_CLOSE_ERROR_FILE"); path != "" {
		closeRunFile = func(file *os.File) error {
			err := file.Close()
			if file.Name() == path {
				return fmt.Errorf("injected close failure for %s", path)
			}
			return err
		}
	}

	for i, arg := range os.Args {
		if arg == "--" {
			executeArgs(os.Args[i+1:])
			os.Exit(0)
		}
	}
	os.Exit(2)
}
