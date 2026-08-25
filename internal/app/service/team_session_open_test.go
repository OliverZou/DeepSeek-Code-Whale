package service

import (
	"context"
	"testing"
	"time"
)

// TestDispatchTeamSessionOpen_DoesNotAbort guards the case-split fix:
// /team session <confirm> must emit EventTeamSessionOpen with the member
// transcript text — it must NOT execute the abort branch (previously the two
// intents shared one case, so opening a session stopped the current run).
func TestDispatchTeamSessionOpen_DoesNotAbort(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{ctx: context.Background(), app: app, events: make(chan Event, 16)}

	s.Dispatch(Intent{Kind: IntentTeamSessionOpen, Input: "00000000-0000-0000-0000-000000000000"})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.events:
			// 打开会话只允许出现 EventTeamSessionOpen（错误文本也只是读取失败，
			// 不是停止 run 的 EventInfo/EventError）。
			if ev.Kind != EventTeamSessionOpen {
				t.Fatalf("IntentTeamSessionOpen produced %v (must not reach abort branch)", ev.Kind)
			}
			return
		case <-deadline:
			t.Fatal("timeout: IntentTeamSessionOpen dispatched no event")
		}
	}
}

// TestDispatchTeamAbort_StillStops guards that abort is untouched by the split:
// /team abort <id> still goes through StopSelectedRun (EventInfo result).
func TestDispatchTeamAbort_StillStops(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{ctx: context.Background(), app: app, events: make(chan Event, 16)}

	s.Dispatch(Intent{Kind: IntentTeamAbort, Input: "00000000-0000-0000-0000-000000000000"})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.events:
			if ev.Kind == EventTeamSessionOpen {
				t.Fatalf("TeamAbort must not emit EventTeamSessionOpen")
			}
			return
		case <-deadline:
			t.Fatal("timeout: IntentTeamAbort dispatched no event")
		}
	}
}
