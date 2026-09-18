package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// ErrRemote means the proxy reported an error during the session. It has
// already been printed.
var ErrRemote = errors.New("the exec proxy reported an error")

// Run connects to a container's exec endpoint and wires it to this terminal
// until either end hangs up.
func Run(ctx context.Context, endpoint, token string, verbose bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 30 * time.Second
	conn, response, err := dialer.DialContext(ctx, endpoint, http.Header{
		"Authorization": {"Bearer " + token},
	})
	if err != nil {
		return dialError(err, response)
	}
	defer conn.Close()

	// Nothing else unblocks ReadMessage on a signal.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	s := &session{conn: conn}

	stdin := int(os.Stdin.Fd())
	interactive := term.IsTerminal(stdin)
	if interactive {
		state, err := term.MakeRaw(stdin)
		if err != nil {
			return fmt.Errorf("putting the terminal into raw mode: %w", err)
		}
		defer term.Restore(stdin, state)
		restore := prepareConsole()
		defer restore()
		s.raw = true
	}

	// The container's terminal is not started until it has a size, so one is
	// sent even when there is no terminal here to measure.
	stdout := int(os.Stdout.Fd())
	width, height, err := term.GetSize(stdout)
	if err != nil {
		width, height = 80, 24
	}
	s.send(resizeFrame(width, height))
	if interactive {
		go watchResize(ctx, stdout, func(width, height int) { s.send(resizeFrame(width, height)) })
	}

	go s.pumpStdin(interactive)
	go s.keepAlive(ctx)

	return s.read(verbose)
}

type session struct {
	conn *websocket.Conn
	raw  bool

	// gorilla allows one writer at a time; stdin, resizes and pings are three.
	mu sync.Mutex
}

// send writes a text frame, as the az CLI does; the proxy's handling of
// binary frames is unknown.
func (s *session) send(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteMessage(websocket.TextMessage, frame)
}

func (s *session) pumpStdin(interactive bool) {
	buffer := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buffer)
		if n > 0 {
			if s.send(stdinFrame(buffer[:n])) != nil {
				return
			}
		}
		if err != nil {
			// The protocol has no end-of-input message. Piped input ends the
			// way a person would end it: Ctrl-D, which the container's
			// terminal turns into EOF for whatever is reading.
			if !interactive {
				s.send(stdinFrame([]byte{0x04}))
			}
			return
		}
	}
}

// keepAlive stops an idle session being dropped by something in between.
func (s *session) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *session) read(verbose bool) error {
	var remoteError bool
	for {
		_, message, err := s.conn.ReadMessage()
		if err != nil {
			if remoteError {
				return ErrRemote
			}
			if closedNormally(err) {
				return nil
			}
			return fmt.Errorf("reading from the container: %w", err)
		}

		kind, payload, err := decode(message)
		if err != nil {
			return err
		}
		switch kind {
		case Stdout:
			os.Stdout.Write(payload)
		case Stderr:
			os.Stderr.Write(payload)
		case Info:
			// "Connecting to the container...", "received success status
			// from cluster": chatter, beside the path already printed.
			if verbose {
				s.notice("", payload)
			}
		case Error:
			remoteError = true
			s.notice("error: ", payload)
		}
	}
}

// notice prints the proxy's own messages on a line of their own. In raw mode
// a bare \n moves down without returning, so it has to be spelt \r\n.
func (s *session) notice(prefix string, payload []byte) {
	text := append([]byte(prefix), bytes.TrimRight(payload, "\r\n")...)
	text = append(text, '\n')
	if s.raw {
		text = bytes.ReplaceAll(text, []byte("\n"), []byte("\r\n"))
	}
	os.Stderr.Write(text)
}

func closedNormally(err error) bool {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
		return true
	}
	// Closed by our own signal handler, or the proxy dropped the TCP
	// connection without a close frame when the shell exited.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
}

func dialError(err error, response *http.Response) error {
	if response == nil {
		return fmt.Errorf("connecting to the container: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	detail := bytes.TrimSpace(body)
	if len(detail) == 0 {
		return fmt.Errorf("connecting to the container: %s", response.Status)
	}
	return fmt.Errorf("connecting to the container: %s: %s", response.Status, detail)
}
