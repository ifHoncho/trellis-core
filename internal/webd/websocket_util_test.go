package webd

import (
	"bufio"
	"bytes"
	"net"
	"time"
)

func newReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

// nopConn takes writes (pongs, closes) and drops them.
type nopConn struct{ net.Conn }

func (nopConn) Write(b []byte) (int, error)      { return len(b), nil }
func (nopConn) Close() error                     { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }
