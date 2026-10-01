//go:build !windows

package main

import (
	"fmt"
	"os"
)

func showError(title, text string) { fmt.Fprintf(os.Stderr, "%s: %s\n", title, text) }
