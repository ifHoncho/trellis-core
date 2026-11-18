package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// echo turns the console's echo on or off; false when it cannot (input
// that is not a console).
func echo(on bool) bool {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if on {
		mode |= windows.ENABLE_ECHO_INPUT
	} else {
		mode &^= windows.ENABLE_ECHO_INPUT
	}
	return windows.SetConsoleMode(h, mode) == nil
}
