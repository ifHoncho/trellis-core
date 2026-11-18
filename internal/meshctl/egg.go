package meshctl

import (
	"bufio"
	"embed"
	"errors"
	"fmt"
	"io"
)

// Not in the usage, on purpose. "meshctl batman" and "meshctl trellis" run
// the Brainfuck programs in egg/ on the small interpreter below: the mesh
// routes with batman-adv, after all.

//go:embed egg/*.bf
var eggs embed.FS

func cmdEgg(name string, stdout io.Writer) error {
	prog, err := eggs.ReadFile("egg/" + name + ".bf")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(stdout)
	if err := runBF(prog, nil, w, bfMaxSteps); err != nil {
		return err
	}
	return w.Flush()
}

const (
	bfTape     = 30000
	bfMaxSteps = 10_000_000
)

var errBFSteps = errors.New("brainfuck: the program ran too long")

// runBF runs a Brainfuck program: a tape of bytes that wrap, input from in
// (none: 0), output to out. Anything but the eight commands is a comment.
func runBF(src []byte, in io.Reader, out io.Writer, maxSteps int) error {
	var code []byte
	for _, c := range src {
		switch c {
		case '+', '-', '<', '>', '[', ']', '.', ',':
			code = append(code, c)
		}
	}
	jump := make([]int, len(code))
	var open []int
	for i, c := range code {
		switch c {
		case '[':
			open = append(open, i)
		case ']':
			if len(open) == 0 {
				return fmt.Errorf("brainfuck: unmatched ] at command %d", i)
			}
			j := open[len(open)-1]
			open = open[:len(open)-1]
			jump[i], jump[j] = j, i
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("brainfuck: unmatched [ at command %d", open[len(open)-1])
	}
	var tape [bfTape]byte
	var one [1]byte
	ptr := 0
	for pc, steps := 0, 0; pc < len(code); pc++ {
		if steps++; steps > maxSteps {
			return errBFSteps
		}
		switch code[pc] {
		case '+':
			tape[ptr]++
		case '-':
			tape[ptr]--
		case '>':
			if ptr++; ptr == bfTape {
				return errors.New("brainfuck: off the end of the tape")
			}
		case '<':
			if ptr--; ptr < 0 {
				return errors.New("brainfuck: off the start of the tape")
			}
		case '.':
			if _, err := out.Write(tape[ptr : ptr+1]); err != nil {
				return err
			}
		case ',':
			tape[ptr] = 0
			if in != nil {
				if n, _ := in.Read(one[:]); n == 1 {
					tape[ptr] = one[0]
				}
			}
		case '[':
			if tape[ptr] == 0 {
				pc = jump[pc]
			}
		case ']':
			if tape[ptr] != 0 {
				pc = jump[pc]
			}
		}
	}
	return nil
}
