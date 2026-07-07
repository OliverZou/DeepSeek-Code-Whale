package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/team_engine"
	"github.com/usewhale/whale/internal/server"
)

const (
	defaultDaemonPort = 18900
	daemonPIDFile     = "daemon.pid"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the Whale background daemon",
	}
	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	return cmd
}

func newDaemonStartCmd() *cobra.Command {
	var port int
	var workdir string
	var stdio bool

	c := &cobra.Command{
		Use:   "start",
		Short: "Start the Whale daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStart(cmd, port, workdir, stdio)
		},
	}
	c.Flags().IntVar(&port, "port", defaultDaemonPort, "WebSocket listen port")
	c.Flags().StringVar(&workdir, "workdir", "", "Working directory (default: data dir)")
	c.Flags().BoolVar(&stdio, "stdio", false, "Run in stdio mode (read stdin, write stdout JSONL)")
	return c
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the Whale daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStop()
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStatus()
		},
	}
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

func runDaemonStart(cmd *cobra.Command, port int, workdir string, stdio bool) error {
	dataDir := store.DefaultDataDir()
	if workdir == "" {
		workdir = dataDir
	}

	if err := os.MkdirAll(workdir, 0755); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	// Team engine.
	whiteboardDir := filepath.Join(workdir, "team_tasks")
	os.MkdirAll(whiteboardDir, 0755)
	spawner := team_engine.NewShellSubagentSpawner()
	eng, err := team_engine.New("", whiteboardDir, "", spawner)
	if err != nil {
		return fmt.Errorf("init team engine: %w", err)
	}

	srv, err := server.NewDaemon(eng, server.DaemonConfig{
		Port:    port,
		DataDir: dataDir,
		WorkDir: workdir,
	})
	if err != nil {
		eng.Close()
		return fmt.Errorf("init daemon: %w", err)
	}

	if stdio {
		return srv.RunStdio()
	}

	// PID file check.
	pidPath := filepath.Join(dataDir, daemonPIDFile)
	if pid, err := readPIDFile(pidPath); err == nil && isProcessAlive(pid) {
		fmt.Printf("daemon already running (pid %d)\n", pid)
		return nil
	}
	os.Remove(pidPath)

	// Write PID file.
	pid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		srv.Close()
		eng.Close()
		return fmt.Errorf("write pid file: %w", err)
	}

	// Start HTTP server in background.
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon server error: %v\n", err)
		}
	}()

	fmt.Printf("whale daemon started on :%d (pid %d)\n", port, pid)

	// Wait for signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Fprintln(os.Stderr, "\nshutting down...")
	srv.Close()
	eng.Close()
	os.Remove(pidPath)
	return nil
}

// ---------------------------------------------------------------------------
// stop
// ---------------------------------------------------------------------------

func runDaemonStop() error {
	dataDir := store.DefaultDataDir()
	pidPath := filepath.Join(dataDir, daemonPIDFile)

	pid, err := readPIDFile(pidPath)
	if err != nil {
		fmt.Println("daemon not running")
		return nil
	}
	if !isProcessAlive(pid) {
		os.Remove(pidPath)
		fmt.Println("daemon not running (stale pid file removed)")
		return nil
	}

	if err := terminateProcess(pid); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}

	// Wait up to 5 seconds.
	for i := 0; i < 50; i++ {
		if !isProcessAlive(pid) {
			os.Remove(pidPath)
			fmt.Println("daemon stopped")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill.
	if err := killProcess(pid); err != nil {
		return fmt.Errorf("force kill daemon: %w", err)
	}
	os.Remove(pidPath)
	fmt.Println("daemon force-stopped")
	return nil
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func runDaemonStatus() error {
	dataDir := store.DefaultDataDir()
	pidPath := filepath.Join(dataDir, daemonPIDFile)

	pid, err := readPIDFile(pidPath)
	if err != nil {
		fmt.Println("not running")
		return nil
	}
	if !isProcessAlive(pid) {
		os.Remove(pidPath)
		fmt.Println("not running (stale pid file removed)")
		return nil
	}
	fmt.Printf("running (pid %d)\n", pid)
	return nil
}
