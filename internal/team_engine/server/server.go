// Package server provides an HTTP API server with SSE push for
// real-time task status monitoring.  The web dashboard frontend lives
// in cmd/dashboard (Wails app) and consumes these API endpoints.
//
//	Browser ←── SSE (/api/events) ──→ Go HTTP Server ←── TeamEngine (FileStore + Whiteboard)
//	        ←── GET /api/tasks      ← JSON task list
//	        ←── GET /api/stats      ← JSON aggregate stats
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/team_engine"
)

// DashboardServer serves the web dashboard over HTTP with SSE-based live
// updates.
type DashboardServer struct {
	engine     *team_engine.TeamEngine
	dbPath        string
	whiteboardDir string
	addr       string
	httpServer *http.Server

	done       chan struct{}
	wg         sync.WaitGroup

	mu         sync.Mutex
	sseClients map[chan string]struct{}
}

// DashboardTask is the enriched task representation sent to the frontend.
type DashboardTask struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	State         string   `json:"state"`
	Role          string   `json:"role"`
	Profile       string   `json:"profile"`
	ElapsedSec    int      `json:"elapsed_sec"`
	RetryCount    int      `json:"retry_count"`
	MaxRetries    int      `json:"max_retries"`
	OutputPreview string   `json:"output_preview"`
	ProgressPct   int      `json:"progress_pct"`
	VerifierFocus string   `json:"verifier_focus"`
	ParentIDs     []string `json:"parent_ids"`
	ChildCount    int      `json:"child_count"`
}

// SSEPayload is the full payload pushed to SSE clients every tick.
type SSEPayload struct {
	Timestamp string               `json:"timestamp"`
	Stats     *team_engine.Stats   `json:"stats"`
	Tasks     []DashboardTask      `json:"tasks"`
}

// NewDashboardServer creates a DashboardServer bound to addr, using the
// given engine instance.
func NewDashboardServer(eng *team_engine.TeamEngine, addr string) *DashboardServer {
	return &DashboardServer{
		engine:     eng,
		addr:       addr,
		done:       make(chan struct{}),
		sseClients: make(map[chan string]struct{}),
	}
}

// StartBackgroundDashboard opens an independent read-only view of the
// team_engine data at dbPath/whiteboardDir and serves the dashboard on
// addr.  It returns immediately; the server runs in a background goroutine.
// The engine is created lazily — no files are written until the first
// team tool (team_plan, team_create, etc.) is used.  Until then the
// dashboard shows an empty view that auto-populates when tasks appear.
//
// This is the "always-on" mode — call it once at Whale startup and
// the dashboard stays available for the whole session.
func StartBackgroundDashboard(dbPath, whiteboardDir, addr string) *DashboardServer {
	// Always start with nil engine — the engine (and its db/ directories)
	// is created lazily when the first team tool runs.  The broadcast
	// loop will pick it up automatically.
	srv := &DashboardServer{
		engine:        nil,
		dbPath:        dbPath,
		whiteboardDir: whiteboardDir,
		addr:          addr,
		done:          make(chan struct{}),
		sseClients:    make(map[chan string]struct{}),
	}

	go func() {
		if err := srv.Start(); err != nil {
			log.Printf("Dashboard: %v", err)
		}
	}()

	return srv
}

// ReloadEngine replaces the server's engine with a new one opened from the
// same paths.  Call this after the database has been created so the
// dashboard can pick up new tasks immediately.
func (s *DashboardServer) ReloadEngine(dbPath, whiteboardDir string) {
	eng, err := team_engine.New(dbPath, whiteboardDir, "", nil)
	if err != nil {
		log.Printf("Dashboard: reload engine: %v", err)
		return
	}
	s.mu.Lock()
	old := s.engine
	s.engine = eng
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// reloadIfNeeded tries to open the engine if the database file now exists.
// It is a no-op when the engine is already loaded or the db is still missing.
func (s *DashboardServer) reloadIfNeeded(dbPath, whiteboardDir string) {
	// Quick check without holding the lock: does the db file exist?
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return
	}
	s.ReloadEngine(dbPath, whiteboardDir)
}

// Start registers routes and begins serving HTTP.  This call blocks until the
// server is stopped.
func (s *DashboardServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/stats", s.handleStats)

	// Listen on an OS-assigned port so multiple Whale instances don't collide.
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("dashboard listen: %w", err)
	}
	s.addr = ln.Addr().String() // update to actual port

	s.httpServer = &http.Server{
		Addr:    "",
		Handler: mux,
	}

	// Start the SSE broadcast goroutine.
	s.wg.Add(1)
	go s.broadcastLoop()

	fmt.Printf("Dashboard: http://%s\n", s.addr)
	return s.httpServer.Serve(ln)
}

// Shutdown gracefully stops the broadcast loop and HTTP server.
//
// Order: shut down HTTP server first (drains pending requests), then
// signal the broadcast loop to stop, then wait for the broadcast
// goroutine to finish.
func (s *DashboardServer) Shutdown(ctx context.Context) error {
	// Step 1: Shut down HTTP server first — this unblocks Serve() and
	// allows SSE client connections to drain cleanly.
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}

	// Step 2: Signal the broadcast loop to stop.
	close(s.done)
	// Step 3: Wait for broadcast goroutine to finish.
	s.wg.Wait()
	return nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleIndex serves the embedded dashboard HTML.
func (s *DashboardServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(`{"message":"Team Engine API is running. The dashboard has moved to cmd/dashboard (Wails app).","endpoints":{"/api/events":"SSE stream","/api/tasks":"JSON task list","/api/stats":"JSON aggregate stats"}}`))
}

// handleTasks returns all tasks as a JSON array.
func (s *DashboardServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.collectDashboardTasks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, tasks)
}

// handleStats returns aggregate statistics as JSON.
func (s *DashboardServer) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	eng := s.engine
	s.mu.Unlock()
	if eng == nil {
		s.writeJSON(w, &team_engine.Stats{Total: 0, ByState: map[string]int{}})
		return
	}
	stats, err := eng.GetStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, stats)
}

// handleEvents is the SSE endpoint.  It pushes task state every 2 seconds.
func (s *DashboardServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Create a channel for this client.  Buffered so slow clients don't block
	// the broadcast.
	ch := make(chan string, 8)
	s.mu.Lock()
	s.sseClients[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.sseClients, ch)
		s.mu.Unlock()
		close(ch)
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Broadcast
// ---------------------------------------------------------------------------

// broadcastLoop runs in a background goroutine, collecting task data every
// 2 seconds and pushing to all connected SSE clients.
func (s *DashboardServer) broadcastLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Send an immediate first update.
	s.broadcast()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.broadcast()
		}
	}
}

// broadcast collects current state and fans out to all SSE clients.
func (s *DashboardServer) broadcast() {
	s.mu.Lock()
	eng := s.engine
	dbPath := s.dbPath
	wbDir := s.whiteboardDir
	s.mu.Unlock()
	if eng == nil {
		// Try lazy init — the engine may have been created by a team tool.
		s.reloadIfNeeded(dbPath, wbDir)
		s.mu.Lock()
		eng = s.engine
		s.mu.Unlock()
		if eng == nil {
			return // still no engine, skip broadcast
		}
	}
	tasks, err := s.collectDashboardTasks()
	if err != nil {
		log.Printf("broadcast: collect tasks: %v", err)
		// If the database has been closed (e.g. engine shut down),
		// clear the engine reference so the broadcast loop stops
		// spamming errors every 2 seconds.
		if strings.Contains(err.Error(), "database is closed") {
			s.mu.Lock()
			if s.engine == eng {
				s.engine = nil
			}
			s.mu.Unlock()
		}
		return
	}

	stats, err := eng.GetStats()
	if err != nil {
		log.Printf("broadcast: get stats: %v", err)
		return
	}

	payload := SSEPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Stats:     stats,
		Tasks:     tasks,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("broadcast: marshal: %v", err)
		return
	}

	msg := string(data)

	s.mu.Lock()
	defer s.mu.Unlock()

	for ch := range s.sseClients {
		select {
		case ch <- msg:
		default:
			// Client is too slow — skip this update.
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// collectDashboardTasks retrieves all tasks and enriches them with
// dashboard-specific fields.
func (s *DashboardServer) collectDashboardTasks() ([]DashboardTask, error) {
	s.mu.Lock()
	eng := s.engine
	s.mu.Unlock()
	if eng == nil {
		return nil, nil
	}
	fullTasks, err := eng.ListTasks()
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	// Compute child counts: count how many tasks reference each task as parent.
	childCounts := make(map[string]int)
	for _, t := range fullTasks {
		for _, pid := range t.ParentIDs {
			childCounts[pid]++
		}
	}

	now := time.Now().UTC()
	result := make([]DashboardTask, len(fullTasks))

	for i, t := range fullTasks {
		elapsed := 0
		createdAt, parseErr := time.Parse(time.RFC3339, t.CreatedAt)
		if parseErr == nil {
			elapsed = int(now.Sub(createdAt).Seconds())
		}

		preview, _ := eng.Whiteboard.ReadOutput(t.ID)
		if len(preview) > 200 {
			preview = preview[len(preview)-200:]
		}

		result[i] = DashboardTask{
			ID:            t.ID,
			Title:         t.Title,
			State:         string(t.State),
			Role:          string(t.Role),
			Profile:       string(t.Profile),
			ElapsedSec:    elapsed,
			RetryCount:    t.RetryCount,
			MaxRetries:    t.MaxRetries,
			OutputPreview: preview,
			ProgressPct:   team_engine.GetProgress(t.State),
			VerifierFocus: t.VerifierFocus,
			ParentIDs:     t.ParentIDs,
			ChildCount:    childCounts[t.ID],
		}
	}

	return result, nil
}

// writeJSON writes v as indented JSON to w.
func (s *DashboardServer) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
