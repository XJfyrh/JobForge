//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "executor process acceptance requires the documented Linux container")
	os.Exit(2)
}
