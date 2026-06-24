import { useState } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import CreateTaskView from './CreateTaskView';
import ExpertPanel from './ExpertPanel';
import TaskObserverView from './AgentChatView';
import DirectChatView from './DirectChatView';
import TaskControlPanel from './TaskControlPanel';
import ChatHistoryPanel from './ChatHistoryPanel';
import ChatTabBar from './ChatTabBar';

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
      style={{ cursor: 'pointer', opacity: 0.4, display: 'flex', alignItems: 'center', padding: '0 8px', flexShrink: 0 }}
    >
      {sidebarToggleIcon}
    </span>
  );
}

function HistoryButton({ onClick }: { onClick: () => void }) {
  const [hover, setHover] = useState(false);
  return (
    <div
      onClick={onClick}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      title="对话历史"
      style={{
        display: 'flex', alignItems: 'center', gap: 5,
        padding: '6px 12px', cursor: 'pointer',
        borderRadius: 8,
        background: hover ? 'rgba(76,175,80,0.15)' : 'transparent',
        border: hover ? '1px solid rgba(76,175,80,0.3)' : '1px solid transparent',
        transition: 'all 0.15s',
      }}
    >
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none"         stroke={hover ? '#4CAF50' : '#666'} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <circle cx="12" cy="12" r="10" /><polyline points="12 6 12 12 16 14" />
      </svg>
      <span style={{ fontSize: 12,         color: hover ? '#4CAF50' : '#666', whiteSpace: 'nowrap' }}>历史</span>
    </div>
  );
}

function NewChatButton({ onClick }: { onClick: () => void }) {
  const [hover, setHover] = useState(false);
  return (
    <div
      onClick={onClick}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      title="新会话"
      style={{
        display: 'flex', alignItems: 'center', gap: 5,
        padding: '6px 12px', cursor: 'pointer',
        borderRadius: 8,
        background: hover ? 'rgba(76,175,80,0.15)' : 'transparent',
        border: hover ? '1px solid rgba(76,175,80,0.3)' : '1px solid transparent',
        transition: 'all 0.15s',
      }}
    >
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none"         stroke={hover ? '#4CAF50' : '#666'} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
      </svg>
      <span style={{ fontSize: 12,         color: hover ? '#4CAF50' : '#666', whiteSpace: 'nowrap' }}>新会话</span>
    </div>
  );
}

function FloatingButtons({ onHistory, onNewChat }: { onHistory: () => void; onNewChat: () => void }) {
  return (
    <div style={{
      position: 'absolute', top: 8, left: 10, zIndex: 40,
      display: 'flex', alignItems: 'center', gap: 8,
    }}>
      <HistoryButton onClick={onHistory} />
      <NewChatButton onClick={onNewChat} />
    </div>
  );
}

export default function RightPanel() {
  const { activeFunction, selMasterTaskId, selSubtaskId, masterTasks, sidebarCollapsed, selAgentId } =
    useStore();
  const [historyOpen, setHistoryOpen] = useState(false);

  const mt = masterTasks.find(t => t.id === selMasterTaskId);

  const handleNewChat = () => {
    useStore.setState({ selMasterTaskId: null, directChatTaskId: null, activeFunction: 'chat', directMessages: [] });
  };

  // Direct chat
  if (activeFunction === 'chat') {
    return (
      <div id="right-panel" style={{ position: 'relative' }}>
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
          {sidebarCollapsed && <SidebarToggle />}
          <div style={{ display: 'flex', alignItems: 'stretch', gap: 0, flex: 1, minWidth: 0, height: '100%' }}>
            <ChatTabBar />
          </div>
        </div>
        <div style={{ position: 'relative', flex: 1, overflow: 'hidden' }}>
          <FloatingButtons onHistory={() => setHistoryOpen(!historyOpen)} onNewChat={handleNewChat} />
          <ChatHistoryPanel open={historyOpen} onClose={() => setHistoryOpen(false)} />
          <DirectChatView key={selMasterTaskId || selAgentId || 'new'} />
        </div>
      </div>
    );
  }

  // Expert panel
  if (activeFunction === 'expert') {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
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
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
          {sidebarCollapsed && <SidebarToggle />}
        </div>
        <CreateTaskView />
      </div>
    );
  }

  // Direct chat task (no subtasks)
  if (mt && mt.task_count === 0) {
    return (
      <div id="right-panel" style={{ position: 'relative' }}>
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
          {sidebarCollapsed && <SidebarToggle />}
          <div style={{ display: 'flex', alignItems: 'stretch', gap: 0, flex: 1, minWidth: 0, height: '100%' }}>
            <ChatTabBar />
          </div>
        </div>
        <div style={{ position: 'relative', flex: 1, overflow: 'hidden' }}>
          <FloatingButtons onHistory={() => setHistoryOpen(!historyOpen)} onNewChat={handleNewChat} />
          <ChatHistoryPanel open={historyOpen} onClose={() => setHistoryOpen(false)} />
          <DirectChatView key={mt.id} />
        </div>
      </div>
    );
  }

  // Team task with subtasks
  if (mt && mt.task_count > 0) {
    return (
      <div id="right-panel" style={{ position: 'relative' }}>
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
          {sidebarCollapsed && <SidebarToggle />}
          <div style={{ display: 'flex', alignItems: 'stretch', gap: 0, flex: 1, minWidth: 0, height: '100%' }}>
            <ChatTabBar />
          </div>
        </div>
        <div style={{ position: 'relative', flex: 1, overflow: 'hidden' }}>
          <FloatingButtons onHistory={() => setHistoryOpen(!historyOpen)} onNewChat={handleNewChat} />
          <ChatHistoryPanel open={historyOpen} onClose={() => setHistoryOpen(false)} />
          <TaskControlPanel />
        </div>
      </div>
    );
  }

  // Default empty
  return (
    <div id="right-panel">
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
        {sidebarCollapsed && <SidebarToggle />}
      </div>
      <div className="empty-state">
        <img src="/whale.png" width="48" height="48" alt="Whale" />
        <div>选择对话或点击"新对话"开始</div>
      </div>
    </div>
  );
}
