package daemon

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/app/service"
	"github.com/usewhale/whale/internal/team_engine"
)

// Server 是 whale daemon 的 HTTP/SSE 传输层，桥接两个可选后端：
//   - service.Service：桌面端主会话（protocol.Event / DispatchProtocol）。
//   - team_engine.TeamEngine：team 任务执行（OnEvent / 6 动词 / Store/Whiteboard 只读）。
//
// 两个后端均为可选：daemon 可仅转发主会话事件，也可仅托管 team 任务，或两者兼有。
// Server 只绑定回环地址（127.0.0.1），面向本地桌面端，不对外网暴露。
type Server struct {
	hub *EventHub

	svc *service.Service        // 可为 nil：不托管桌面主会话
	eng *team_engine.TeamEngine // 可为 nil：不托管 team 任务

	httpsrv *http.Server

	// bridgeOnce 确保后端事件桥接只启动一次（跨多次 Run 常驻转发）。
	bridgeOnce sync.Once
}

// ServerOptions 控制 daemon 构建行为。可选项（svc/eng 任一为 nil 即停用对应后端）。
type ServerOptions struct {
	// Service 桌面端主会话后端。nil 表示不托管。
	Service *service.Service
	// Engine team 引擎后端。nil 表示不托管。
	Engine *team_engine.TeamEngine
	// Addr 监听地址，例如 "127.0.0.1:8765"。默认 "127.0.0.1:8765"。
	Addr string
	// ReadHeaderTimeout 限制请求头读取时间，防止慢速攻击。
	ReadHeaderTimeout time.Duration
	// HubBufferLen 每个 SSE 订阅者的积压缓冲长度。
	HubBufferLen int
	// Auth 认证中间件。nil 表示放行（仅回环仍推荐启用）。
	Auth func(next http.Handler) http.Handler
}

// New 构造一个 daemon Server，尚未监听。
func New(opts ServerOptions) *Server {
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	s := &Server{
		hub: NewEventHub(opts.HubBufferLen),
		svc: opts.Service,
		eng: opts.Engine,
	}
	mux := s.routes()
	var handler http.Handler = mux
	if opts.Auth != nil {
		handler = opts.Auth(mux)
	}
	s.httpsrv = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
	}
	return s
}

// Run 启动并监听 addr（同步阻塞，直到 ctx 取消或服务出错）。
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// 常驻启动后端→EventHub 桥接（幂等，跨请求持续推送）。
	s.startBridge(ctx)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpsrv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		_ = s.Shutdown(context.Background())
		return ctx.Err()
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Shutdown 优雅关闭 HTTP 服务并释放事件订阅。
func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.Close()
	if s.httpsrv != nil {
		if err := s.httpsrv.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Hub 暴露事件总线，供 bridge 将 backend 事件写入。
func (s *Server) Hub() *EventHub { return s.hub }

// Addr 返回实际监听地址。
func (s *Server) Addr() string {
	if s.httpsrv == nil {
		return ""
	}
	return s.httpsrv.Addr
}

// Healthz 是极简健康检查。此处返回非零值便于对外观测子进程/服务状态。
func (s *Server) Healthz() map[string]any {
	return map[string]any{
		"ok":                  true,
		"subscribers":         s.hub.SubscriberCount(),
		"service_backend":     s.svc != nil,
		"team_engine_backend": s.eng != nil,
	}
}
