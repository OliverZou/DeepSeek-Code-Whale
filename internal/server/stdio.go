package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"

	"github.com/google/uuid"
)

type stdioWriter struct {
	encMu sync.Mutex
	enc   *json.Encoder
}

func newStdioWriter(w io.Writer) *stdioWriter {
	enc := json.NewEncoder(w)
	return &stdioWriter{enc: enc}
}

func (s *stdioWriter) SendResponse(resp wsResponse) error {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	return s.enc.Encode(resp)
}

func (s *stdioWriter) Push(p wsPush) error {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	return s.enc.Encode(p)
}

// RunStdio runs the daemon in stdio mode with a new session.
func (d *Daemon) RunStdio() error {
	return d.RunStdioWithSession("")
}

// RunStdioWithSession runs the daemon in stdio mode. If sessionID is non-empty,
// the daemon will resume that existing session; otherwise a new one is created.
func (d *Daemon) RunStdioWithSession(sessionID string) error {
	sw := newStdioWriter(os.Stdout)
	d.stdioWriter = sw

	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	sw.Push(wsPush{
		Type: "ready",
		Payload: map[string]string{
			"session_id": sessionID,
		},
	})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req wsRequest
		if err := json.Unmarshal(line, &req); err != nil {
			sw.SendResponse(wsResponse{
				Type:    "error",
				Payload: map[string]string{"message": "invalid json"},
			})
			continue
		}

		if req.Type == "shutdown" {
			break
		}

		d.handleMessage(sw, req)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("daemon stdio: scanner error: %v", err)
	}

	d.wg.Wait()
	return nil
}