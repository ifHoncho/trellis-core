//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// echo turns the terminal's echo on or off (stty); false when it cannot.
func echo(on bool) bool {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	c := exec.Command("stty", arg)
	c.Stdin = os.Stdin
	return c.Run() == nil
}
