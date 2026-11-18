// Package wire frames the browser console's messages on the unix socket
// between webd and trellis-voiced: each WebSocket message, text (JSON) or
// binary (audio), as a kind byte, a 4-byte big-endian length and the
// payload. webd passes messages through without reading them.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Kinds, as WebSocket's opcodes.
const (
	Text   byte = 1
	Binary byte = 2
)

// MaxMessage bounds a message: the largest is a status in JSON.
const MaxMessage = 64 << 10

// Write writes one message.
func Write(w io.Writer, kind byte, b []byte) error {
	if len(b) > MaxMessage {
		return fmt.Errorf("wire: a %d-byte message", len(b))
	}
	var h [5]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

// Read reads one message.
func Read(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[0] != Text && h[0] != Binary {
		return 0, nil, errors.New("wire: unknown message kind")
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxMessage {
		return 0, nil, fmt.Errorf("wire: a %d-byte message", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return h[0], b, err
}
