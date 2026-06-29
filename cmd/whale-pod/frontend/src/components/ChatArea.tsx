import { useState } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import CreateTaskView from './CreateTaskView';
import DirectChatView from './DirectChatView';
import ExpertPanel from './ExpertPanel';
import SettingsPanel from './SettingsPanel';
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
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none"
        stroke={hover ? '#4CAF50' : '#666'} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <circle cx="12" cy="12" r="10" /><polyline points="12 6 12 12 16 14" />
      </svg>
      <span style={{ fontSize: 12, color: hover ? '#4CAF50' : '#666', whiteSpace: 'nowrap' }}>历史</span>
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
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none"
        stroke={hover ? '#4CAF50' : '#666'} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
      </svg>
      <span style={{ fontSize: 12, color: hover ? '#4CAF50' : '#666', whiteSpace: 'nowrap' }}>新会话</span>
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

export default function ChatArea() {
  const { activeFunction, selMasterTaskId, masterTasks, sidebarCollapsed, selAgentId, directMessages } = useStore();
  const [historyOpen, setHistoryOpen] = useState(false);

  const mt = masterTasks.find(t => t.id === selMasterTaskId);
  const hasChatMessages = directMessages.length > 0;

  const handleNewChat = () => {
    useStore.setState({ selMasterTaskId: null, directChatTaskId: null, activeFunction: 'chat', directMessages: [] });
  };

  const renderContent = () => {
    // Create task view
    if (activeFunction === 'create') {
      return <CreateTaskView />;
    }

    // Expert panel
    if (activeFunction === 'expert') {
      return <ExpertPanel />;
    }

    // Settings panel
    if (activeFunction === 'settings') {
      return <SettingsPanel visible={true} onClose={() => useStore.setState({ activeFunction: null })} />;
    }

    // New direct chat (no session selected)
    if (activeFunction === 'chat') {
      return (
        <div style={{ position: 'relative', flex: 1, overflow: 'hidden' }}>
          <FloatingButtons onHistory={() => setHistoryOpen(!historyOpen)} onNewChat={handleNewChat} />
          <ChatHistoryPanel open={historyOpen} onClose={() => setHistoryOpen(false)} />
          <DirectChatView key={selMasterTaskId || selAgentId || 'new'} />
        </div>
      );
    }

    // Session with chat — always show DirectChatView
    if (mt || hasChatMessages) {
      return (
        <div style={{ position: 'relative', flex: 1, overflow: 'hidden' }}>
          <FloatingButtons onHistory={() => setHistoryOpen(!historyOpen)} onNewChat={handleNewChat} />
          <ChatHistoryPanel open={historyOpen} onClose={() => setHistoryOpen(false)} />
          <DirectChatView key={mt?.id || selMasterTaskId || 'session'} />
        </div>
      );
    }

    // Default empty
    return (
      <div className="empty-state">
        <img src="/whale.png" width="48" height="48" alt="Whale" />
        <div>选择对话或点击"新对话"开始</div>
      </div>
    );
  };

  return (
    <div id="chat-area" style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden', background: 'var(--panel-bg)', minWidth: 200 }}>
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
        {sidebarCollapsed && <SidebarToggle />}
        <div style={{ display: 'flex', alignItems: 'stretch', gap: 0, flex: 1, minWidth: 0, height: '100%' }}>
          <ChatTabBar />
        </div>
      </div>
      {renderContent()}
    </div>
  );
}
