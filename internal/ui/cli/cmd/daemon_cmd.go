package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/server"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the Whale background daemon",
	}
	cmd.AddCommand(newDaemonStartCmd())
	return cmd
}

func newDaemonStartCmd() *cobra.Command {
	var workdir string
	var sessionID string

	c := &cobra.Command{
		Use:   "start",
		Short: "Start the Whale daemon (stdio mode)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStart(workdir, sessionID)
		},
	}
	c.Flags().StringVar(&workdir, "workdir", "", "Working directory (default: data dir)")
	c.Flags().StringVar(&sessionID, "session-id", "", "Resume an existing session")
	return c
}

func runDaemonStart(workdir string, sessionID string) error {
	dataDir := store.DefaultDataDir()
	if workdir == "" {
		workdir = dataDir
	}

	if err := os.MkdirAll(workdir, 0755); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	srv, err := server.NewDaemon(nil, server.DaemonConfig{
		DataDir: dataDir,
		WorkDir: workdir,
	})
	if err != nil {
		return fmt.Errorf("init daemon: %w", err)
	}

	return srv.RunStdioWithSession(sessionID)
}
