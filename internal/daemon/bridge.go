package daemon

import (
	"context"

	"github.com/usewhale/whale/internal/team_engine"
)

// bridge 将两个可选后端的事件流统一转为 daemon.Event，并送入 EventHub：
//   - svc.Events()（protocol.Event）→ ChannelAgent
//   - eng.OnEvent（team_engine.TaskEvent）→ ChannelTeam，并在 batch 状态转换时
//     额外广播 ChannelDAG 快照，供前端增量着色 DAG 节点。
//
// bridge 常驻跨请求推送（与 CLI 的当次命令内订阅不同）。后端为 nil 时对应桥接不启动。
// startBridge 启动 agent/team 后端 → EventHub 的常驻转发。幂等：重复调用不启动重复桥接。
func (s *Server) startBridge(ctx context.Context) {
	s.bridgeOnce.Do(func() {
		if s.svc != nil {
			go s.bridgeAgent(ctx)
		}
		if s.eng != nil {
			go s.bridgeTeam(ctx)
		}
	})
}

// bridgeAgent 转发桌面端主会话的 protocol.Event。
func (s *Server) bridgeAgent(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-s.svc.Events():
			if !ok {
				return
			}
			s.broadcastEvent(NewAgentEvent(ev))
		}
	}
}

// bridgeTeam 转发 team engine 的 TaskEvent，并在 batch 状态转换时广播 DAG 快照。
func (s *Server) bridgeTeam(ctx context.Context) {
	cancel := s.eng.OnEvent(func(ev team_engine.TaskEvent) {
		// 始终广播 team 通道事件（含 task_id/old_state/new_state/progress）。
		s.broadcastEvent(NewTeamEvent(ev))
		// 状态转换时广播 DAG 快照，供前端增量着色节点与边。
		if isDAGRelevant(ev) {
			masterID := s.masterTaskIDFor(ev.TaskID)
			if masterID != "" {
				s.broadcastMasterUpdate(masterID)
			}
		}
	})
	defer cancel()

	<-ctx.Done()
}

// isDAGRelevant 判断某个 TaskEvent 是否会引起 DAG 节点（batch）状态变化，
// 从而需要额外广播 dag 事件。仅状态跃迁与任务完成/失败时值得刷新快照。
func isDAGRelevant(ev team_engine.TaskEvent) bool {
	switch ev.Type {
	// EventTaskDone 同时覆盖 done 与 failed（team engine 无独立的 failed 事件）。
	case team_engine.EventStateChanged, team_engine.EventTaskDone:
		return true
	default:
		return false
	}
}

// masterTaskIDFor 由任务 ID 反查其所属 master task，用于定位 DAG 快照。
func (s *Server) masterTaskIDFor(taskID string) string {
	if taskID == "" || s.eng == nil {
		return ""
	}
	task, err := s.eng.Store.GetTask(taskID)
	if err != nil || task == nil {
		return ""
	}
	return task.MasterTaskID
}
