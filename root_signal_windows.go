//go:build windows

package main

import (
	"context"
	"io"
	"os"
)

func openInputFile(path string) (*os.File, error) { return os.Open(path) }
func newInterruptibleOutput(file *os.File) (*os.File, func() error, error) {
	return file, func() error { return nil }, nil
}
func newInputReader(file *os.File, _ context.Context) (io.Reader, func() error, error) {
	return file, func() error { return nil }, nil
}

func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt} }
func terminationStatus(os.Signal) int { return 130 }
