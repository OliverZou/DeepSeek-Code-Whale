package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/usewhale/whale/internal/app"
	"github.com/usewhale/whale/internal/app/service"
	"github.com/usewhale/whale/internal/daemon"
	"github.com/usewhale/whale/internal/team_engine"
)

// daemonFlags 聚合 `whale daemon` 的传输层 / 后端开关配置。
type daemonFlags struct {
	addr       string
	token      string
	whiteboard string
	configPath string
	workdir    string
	noSession  bool
	noTeam     bool
}

func newDaemonCmd(opts *cliOptions) *cobra.Command {
	var flags daemonFlags

	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the local Whale daemon (HTTP/SSE) bridging the desktop session and Team Engine.",
		Long: `Whale daemon exposes a loopback-only HTTP/SSE transport for the local desktop
app. It bridges two optional backends:

  - the desktop main-session (service.Service) via /api/intent + /events (agent),
  - the Team Engine (team_engine.TeamEngine) via the /api/team/... endpoints.

Events from both backends are funneled into a single SSE stream (/events),
so the desktop can render the main agent session and the team DAG in one view.

Both backends are optional: use --no-session / --no-team to host only one.
The server binds to 127.0.0.1 only and is not exposed to the network.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectWorktreeFlag(cmd); err != nil {
				return err
			}
			if err := prepareCLIConfig(cmd, opts); err != nil {
				return err
			}
			return runDaemon(cmd.Context(), opts.cfg, flags)
		},
	}

	f := daemonCmd.Flags()
	f.StringVar(&flags.addr, "addr", "127.0.0.1:8765", "Listen address (loopback only)")
	f.StringVar(&flags.token, "token", "", "Bearer token for Auth middleware (empty = allow all)")
	f.StringVar(&flags.whiteboard, "whiteboard", ".whale/team_tasks", "Whiteboard directory for Team Engine state")
	f.StringVar(&flags.configPath, "config", "", "Path to team_engine.yaml config")
	f.StringVar(&flags.workdir, "workdir", ".", "Working directory for agent execution")
	f.BoolVar(&flags.noSession, "no-session", false, "Do not host the desktop main-session (service) backend")
	f.BoolVar(&flags.noTeam, "no-team", false, "Do not host the Team Engine backend")

	return daemonCmd
}

// runDaemon 构建两个后端并启动 HTTP/SSE 服务。
func runDaemon(ctx context.Context, cfg app.Config, opts daemonFlags) error {
	// Context 绑定 Ctrl+C（SIGINT），触发优雅关闭。
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	var svc *service.Service
	if !opts.noSession {
		s, err := service.New(ctx, cfg, app.StartOptions{NewSession: true})
		if err != nil {
			return fmt.Errorf("init session backend: %w", err)
		}
		defer s.Close()
		svc = s
	}

	var eng *team_engine.TeamEngine
	if !opts.noTeam {
		e, err := newTeamEngine(".whale/team_engine.db", opts.whiteboard, opts.configPath, opts.workdir)
		if err != nil {
			return fmt.Errorf("init team engine: %w", err)
		}
		defer e.Close()
		eng = e
	}

	srv := daemon.New(daemon.ServerOptions{
		Service: svc,
		Engine:  eng,
		Addr:    opts.addr,
		Auth:    daemon.AuthMiddleware(opts.token),
	})

	fmt.Fprintf(os.Stderr, "whale daemon listening on %s (session=%t team=%t)\n",
		opts.addr, svc != nil, eng != nil)
	return srv.Run(ctx, opts.addr)
}
