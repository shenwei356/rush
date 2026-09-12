//go:build windows

package main

import (
	"context"
	"io"
	"os"
)

func openInputFile(path string) (*os.File, error)               { return os.Open(path) }
func newInputReader(file *os.File, _ context.Context) io.Reader { return file }

func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt} }
func terminationStatus(os.Signal) int { return 130 }
