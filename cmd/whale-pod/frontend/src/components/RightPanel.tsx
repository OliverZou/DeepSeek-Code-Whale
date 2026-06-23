import { useStore } from '../store';
import { api } from '../wails';
import CreateTaskView from './CreateTaskView';
import ExpertPanel from './ExpertPanel';
import TaskObserverView from './TaskObserverView';
import AgentChatView from './AgentChatView';
import DirectChatView from './DirectChatView';
import TaskControlPanel from './TaskControlPanel';

const sidebarToggleIcon = (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <rect x="3" y="3" width="18" height="18" rx="2" ry="2" />
    <line x1="9" y1="3" x2="9" y2="21" />
    <line x1="15" y1="12" x2="21" y2="12" />
  </svg>
);

function SidebarToggle() {
  const toggleSidebar = useStore(s => s.toggleSidebar);
  return (
    <span
      onClick={e => { e.stopPropagation(); toggleSidebar(); }}
      title="显示侧栏"
      style={{ cursor: 'pointer', opacity: 0.4, display: 'flex', alignItems: 'center', padding: '0 8px' }}
    >
      {sidebarToggleIcon}
    </span>
  );
}

function ChatTitle({ mt }: { mt?: { goal: string; workspace_path: string } | null }) {
  if (!mt) return null;
  const wsName = mt.workspace_path ? mt.workspace_path.split(/[/\\]/).pop() : '';
  return (
    <>
      {wsName && <span style={{ color: '#888', fontWeight: 400 }}>{wsName}</span>}
      {wsName && <span style={{ color: '#444', margin: '0 6px' }}>/</span>}
      <span style={{ color: '#ccc' }}>{mt.goal}</span>
    </>
  );
}

export default function RightPanel() {
  const { activeFunction, selMasterTaskId, selSubtaskId, masterTasks, directChatTaskId, sidebarCollapsed } =
    useStore();

  const mt = masterTasks.find(t => t.id === selMasterTaskId);
  const directMt = directChatTaskId ? masterTasks.find(t => t.id === directChatTaskId) : null;

  // Direct chat
  if (activeFunction === 'chat') {
    const currentMt = selMasterTaskId ? mt : directMt;
    return (
      <div id="right-panel">
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
          {sidebarCollapsed && <SidebarToggle />}
          <ChatTitle mt={currentMt} />
        </div>
        <DirectChatView key={selMasterTaskId || 'new'} />
      </div>
    );
  }

  // Expert panel
  if (activeFunction === 'expert') {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
          {sidebarCollapsed && <SidebarToggle />}
        </div>
        <ExpertPanel />
      </div>
    );
  }

  // Create task
  if (activeFunction === 'create') {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
          {sidebarCollapsed && <SidebarToggle />}
        </div>
        <CreateTaskView />
      </div>
    );
  }

  // Direct chat task (no subtasks) — load chat messages
  if (mt && mt.task_count === 0) {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
          {sidebarCollapsed && <SidebarToggle />}
          <ChatTitle mt={mt} />
        </div>
        <DirectChatView key={mt.id} />
      </div>
    );
  }

  // Team task with subtasks — use TaskControlPanel
  if (mt && mt.task_count > 0) {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
          {sidebarCollapsed && <SidebarToggle />}
          <ChatTitle mt={mt} />
        </div>
        <TaskControlPanel />
      </div>
    );
  }

  // Default empty
  return (
    <div id="right-panel">
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
        {sidebarCollapsed && <SidebarToggle />}
      </div>
      <div className="empty-state">
        <img src="/whale.png" width="48" height="48" alt="Whale" />
        <div>选择对话或点击"新对话"开始</div>
      </div>
    </div>
  );
}
