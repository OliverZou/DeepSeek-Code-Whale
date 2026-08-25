package daemon

import (
	"time"

	"github.com/usewhale/whale/internal/runtime/protocol"
	"github.com/usewhale/whale/internal/team_engine"
)

// Channel 标识 SSE 事件来源通道。前端据此区分渲染器。
type Channel string

const (
	// ChannelAgent — 桌面主会话事件（复用 protocol.Event 原字段）。
	ChannelAgent Channel = "agent"
	// ChannelTeam — team engine 事件（TaskEvent 全字段）。
	ChannelTeam Channel = "team"
	// ChannelDAG — team DAG 进度变更（batch 状态/cycle 变化）。
	ChannelDAG Channel = "dag"
)

// Event 是 daemon 向桌面端 SSE 推送的统一事件结构，桥接两类后端来源：
//   - agent 侧：svc.Events() 的 protocol.Event（Channel=ChannelAgent）
//   - team 侧：eng.OnEvent 的 team_engine.TaskEvent（Channel=ChannelTeam）
//   - DAG 进度变更（Channel=ChannelDAG），携带 batch 状态/cycle 快照供前端着色 DAG 节点。
//
// 每个 Event 只填充与 Channel 对应的载荷字段，其余保持零值。
type Event struct {
	Channel      Channel                `json:"channel"`
	Agent        *protocol.Event        `json:"agent,omitempty"`
	Team         *team_engine.TaskEvent `json:"team,omitempty"`
	DAG          *DAGEvent              `json:"dag,omitempty"`
	SessionID    string                 `json:"session_id,omitempty"`
	MasterTaskID string                 `json:"master_task_id,omitempty"`
	Time         time.Time              `json:"time"`
}

// NewAgentEvent 从 protocol.Event 构造一个 agent 通道事件。
func NewAgentEvent(ev protocol.Event) Event {
	return Event{Channel: ChannelAgent, Agent: &ev, Time: time.Now()}
}

// NewTeamEvent 从 team_engine.TaskEvent 构造一个 team 通道事件。
func NewTeamEvent(ev team_engine.TaskEvent) Event {
	return Event{Channel: ChannelTeam, Team: &ev, Time: time.Now()}
}

// NewDAGEvent 构造一个 DAG 进度变更事件。
func NewDAGEvent(dag DAGEvent, masterTaskID string) Event {
	return Event{
		Channel:      ChannelDAG,
		DAG:          &dag,
		MasterTaskID: masterTaskID,
		Time:         time.Now(),
	}
}

// DAGEvent 描述某个 master task 的 DAG 增量变更或快照。
// 前端据此增量着色 DAG 节点与依赖边，无需重建整图。
type DAGEvent struct {
	MasterTaskID string          `json:"master_task_id,omitempty"`
	Batches      []DAGBatchEvent `json:"batches,omitempty"`
}

// DAGBatchEvent 是单个 batch（DAG 节点）的运行时状态快照。
type DAGBatchEvent struct {
	BatchID    string         `json:"batch_id"`
	Label      string         `json:"label,omitempty"`
	Status     string         `json:"status"`
	CycleCount int            `json:"cycle_count,omitempty"`
	DependsOn  []string       `json:"depends_on,omitempty"`
	Tasks      []DAGTaskEvent `json:"tasks,omitempty"`
}

// DAGTaskEvent 是 batch 内单个任务的状态快照。
type DAGTaskEvent struct {
	TaskID   string `json:"task_id"`
	Title    string `json:"title,omitempty"`
	State    string `json:"state"`
	Progress int    `json:"progress,omitempty"`
}
