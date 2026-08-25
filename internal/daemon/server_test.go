package daemon

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	s := New(ServerOptions{})
	if s.Addr() != "127.0.0.1:8765" {
		t.Fatalf("addr = %q, want 127.0.0.1:8765", s.Addr())
	}
	if s.hub.bufferLen != 64 {
		t.Fatalf("hub bufferLen = %d, want 64", s.hub.bufferLen)
	}
	if s.svc != nil || s.eng != nil {
		t.Fatalf("got backend svc=%v eng=%v, want both nil", s.svc, s.eng)
	}
	_ = s.Shutdown(context.Background())
}

func TestNewWithAuthWrapsHandler(t *testing.T) {
	s := New(ServerOptions{Auth: AuthMiddleware("token"), Addr: "127.0.0.1:9000"})
	if s.Addr() != "127.0.0.1:9000" {
		t.Fatalf("addr = %q, want 127.0.0.1:9000", s.Addr())
	}
	if s.httpsrv.Handler == nil {
		t.Fatal("handler is nil")
	}
	_ = s.Shutdown(context.Background())
}

func TestHealthzBackendFlags(t *testing.T) {
	s := New(ServerOptions{})
	hz := s.Healthz()
	if hz["ok"] != true {
		t.Fatalf("ok = %v, want true", hz["ok"])
	}
	if hz["service_backend"] != false || hz["team_engine_backend"] != false {
		t.Fatalf("backend flags = service:%v team:%v, want both false",
			hz["service_backend"], hz["team_engine_backend"])
	}
	if hz["subscribers"] != 0 {
		t.Fatalf("subscribers = %v, want 0", hz["subscribers"])
	}
}

func TestHealthzEndpoint(t *testing.T) {
	s := New(ServerOptions{})
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), `"ok":true`) {
		t.Fatalf("body = %q, want ok=true", string(body[:n]))
	}
}

// TestRoutesBackendDisabled 验证未托管后端时，对应端点返回 503。
func TestRoutesBackendDisabled(t *testing.T) {
	s := New(ServerOptions{})
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/intent"},
		{http.MethodGet, "/api/team/masters"},
		{http.MethodGet, "/api/team/masters/m1/dag"},
		{http.MethodGet, "/api/team/tasks/t1"},
		{http.MethodPost, "/api/team/prompt"},
		{http.MethodPost, "/api/team/spawn"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d, want %d", tc.method, tc.path, resp.StatusCode, http.StatusServiceUnavailable)
		}
		resp.Body.Close()
	}
}

// TestEventsSSE 验证 SSE 端点：首帧连接帧 + 广播后的事件帧。
func TestEventsSSE(t *testing.T) {
	s := New(ServerOptions{})
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	rd := bufio.NewReader(resp.Body)

	// 首帧：连接注释。
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if !strings.Contains(line, ": connected") {
		t.Fatalf("first frame = %q, want connection frame", line)
	}

	// 连接帧后的空行分隔符。
	line, err = rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read frame separator: %v", err)
	}
	if line != "\n" {
		t.Fatalf("frame separator = %q, want newline", line)
	}

	// 广播一个事件。
	s.hub.Broadcast(SSEEvent{Name: "agent", Data: []byte(`{"channel":"agent"}`)})

	line, err = rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read event frame: %v", err)
	}
	if !strings.Contains(line, "event: agent") {
		t.Fatalf("event frame = %q, want 'event: agent'", line)
	}

	line, err = rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read data frame: %v", err)
	}
	if !strings.Contains(line, "data: ") {
		t.Fatalf("data frame = %q, want data prefix", line)
	}
}

// TestRunAddrInUse 验证监听地址冲突时 Run 立即返回错误而非阻塞。
func TestRunAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	s := New(ServerOptions{Auth: nil})
	err = s.Run(context.Background(), addr)
	if err == nil {
		t.Fatal("expected error for in-use address")
	}
	_ = s.Shutdown(context.Background())
}
