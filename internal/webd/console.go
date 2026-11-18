package webd

import (
	"net"
	"net/http"
	"time"

	"meridian/trellis/internal/voice/wire"
)

// The browser console (architecture §13, Voice): a signed-in browser's
// WebSocket, passed to trellis-voiced's console socket message by
// message. webd reads nothing in them; it only keeps the two ends paired
// and the connection alive.

const defaultVoiceConsole = "/run/trellis-voice/console.sock"

// A console is pinged every consolePing and cut when nothing at all (not
// even a pong) has come from it for consoleIdle. A console that only
// listens sends nothing else. Variables for the tests.
var (
	consolePing = 20 * time.Second
	consoleIdle = 60 * time.Second
)

func (s *Server) voiceConsoleSocket() string {
	if s.opts.VoiceConsole != "" {
		return s.opts.VoiceConsole
	}
	return defaultVoiceConsole
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	voice, err := net.DialTimeout("unix", s.voiceConsoleSocket(), 2*time.Second)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "voice is not running on this node")
		return
	}
	ws, err := wsUpgrade(w, r)
	if err != nil {
		voice.Close()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ws.idle = consoleIdle
	s.log.Info("voice console connected", "from", remoteIP(r))
	done := make(chan struct{}, 2)
	// Browser → voiced.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			op, m, err := ws.ReadMessage()
			if err != nil {
				return
			}
			kind := wire.Binary
			if op == opText {
				kind = wire.Text
			}
			_ = voice.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if wire.Write(voice, kind, m) != nil {
				return
			}
		}
	}()
	// voiced → browser, with a ping every 20 s.
	go func() {
		defer func() { done <- struct{}{} }()
		msgs := make(chan [2][]byte, 32)
		quit := make(chan struct{}) // this loop has left: the reader must not wait on msgs
		defer close(quit)
		go func() {
			defer close(msgs)
			for {
				kind, m, err := wire.Read(voice)
				if err != nil {
					return
				}
				select {
				case msgs <- [2][]byte{{kind}, m}:
				case <-quit:
					return
				}
			}
		}()
		ping := time.NewTicker(consolePing)
		defer ping.Stop()
		for {
			select {
			case m, ok := <-msgs:
				if !ok {
					return
				}
				op := byte(opBinary)
				if m[0][0] == wire.Text {
					op = opText
				}
				if ws.write(op, m[1]) != nil {
					return
				}
			case <-ping.C:
				// A session that ends (sign-out, a new password) ends its
				// console, as it ends its event stream.
				if !s.authenticated(r) || ws.write(opPing, nil) != nil {
					return
				}
			}
		}
	}()
	<-done
	voice.Close()
	ws.Close()
	<-done
	s.log.Info("voice console gone", "from", remoteIP(r))
}
