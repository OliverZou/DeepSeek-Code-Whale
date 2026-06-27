import { useState, useEffect, useRef } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import type { MasterTask } from '../types';

const PAGE_SIZE = 20;

export default function ChatHistoryPanel({ open, onClose }: { open: boolean; onClose: () => void }) {
  const selAgentId = useStore(s => s.selAgentId);
  const selectMasterTask = useStore(s => s.selectMasterTask);
  const [sessions, setSessions] = useState<MasterTask[]>([]);
  const [offset, setOffset] = useState(0);
  const [hasMore, setHasMore] = useState(true);
  const [loading, setLoading] = useState(false);
  const [visible, setVisible] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);

  const agent = selAgentId === 'whale:' ? '' : selAgentId || '';

  useEffect(() => {
    if (open) {
      setVisible(true);
      loadMore(0);
    } else {
      setVisible(false);
    }
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (panelRef.current && !panelRef.current.contains(e.target as Node)) {
        onClose();
      }
    };
    const timer = setTimeout(() => document.addEventListener('mousedown', handler), 100);
    return () => {
      clearTimeout(timer);
      document.removeEventListener('mousedown', handler);
    };
  }, [open, onClose]);

  const loadMore = async (fromOffset: number) => {
    if (loading) return;
    setLoading(true);
    try {
      const result = await api.listSessionsByAgent(agent, fromOffset, PAGE_SIZE);
      if (fromOffset === 0) {
        setSessions(result || []);
      } else {
        setSessions(prev => [...prev, ...(result || [])]);
      }
      setHasMore((result || []).length >= PAGE_SIZE);
      setOffset(fromOffset + (result || []).length);
    } finally {
      setLoading(false);
    }
  };

  const handleSelect = (session: MasterTask) => {
    // Ensure agent context matches the session's agent
    const agentKey = session.agent || '';
    if (useStore.getState().selAgentId !== agentKey) {
      useStore.setState({ selAgentId: agentKey });
    }
    // Force directChatTaskId so DirectChatView loads this session's messages,
    // not a stale previous session.
    useStore.setState({ directChatTaskId: session.id });
    useStore.getState().addTab(session.id);
    selectMasterTask(session.id);
    onClose();
  };

  const handleDelete = async (e: React.MouseEvent, session: MasterTask) => {
    e.stopPropagation();
    if (!confirm(`删除对话「${session.goal || session.id}」？`)) return;
    await api.deleteSession(session.id);
    setSessions(prev => prev.filter(s => s.id !== session.id));
    useStore.getState().loadMasterTasks();
  };

  const handleDeleteAll = async () => {
    if (!confirm(`确定删除「${selAgentId || 'Whale'}」的全部对话记录？此操作不可恢复。`)) return;
    await api.deleteAllSessions(agent);
    setSessions([]);
    setHasMore(false);
    useStore.getState().loadMasterTasks();
  };

  const handleClearEmpty = async () => {
    await api.clearEmptySessions(agent);
    // Reload list
    const result = await api.listSessionsByAgent(agent, 0, PAGE_SIZE);
    setSessions(result || []);
    setHasMore((result || []).length >= PAGE_SIZE);
    setOffset((result || []).length);
    useStore.getState().loadMasterTasks();
  };

  const formatTime = (iso: string) => {
    if (!iso) return '';
    try {
      const d = new Date(iso);
      const now = new Date();
      const isToday = d.toDateString() === now.toDateString();
      if (isToday) return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
      return d.toLocaleDateString('zh-CN', { month: 'short', day: 'numeric' });
    } catch { return ''; }
  };

  return (
    <div
      ref={panelRef}
      style={{
        position: 'absolute', top: 0, left: 0, bottom: 0, zIndex: 50,
        width: 300,
        transform: visible ? 'translateX(0)' : 'translateX(-100%)',
        transition: 'transform 0.25s cubic-bezier(0.4, 0, 0.2, 1)',
        background: 'rgba(20, 20, 20, 0.92)',
        backdropFilter: 'blur(12px)',
        borderRight: '1px solid rgba(255,255,255,0.08)',
        display: 'flex', flexDirection: 'column',
        overflow: 'hidden',
        boxShadow: visible ? '4px 0 24px rgba(0,0,0,0.4)' : 'none',
      }}
    >
      <div style={{
        padding: '12px 14px', borderBottom: '1px solid rgba(255,255,255,0.06)',
        display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        flexShrink: 0,
      }}>
        <span style={{ fontSize: 13, color: '#aaa', fontWeight: 600 }}>对话历史</span>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          <span
            onClick={handleClearEmpty}
            title="清理空对话"
            style={{ cursor: 'pointer', color: '#666', display: 'flex', alignItems: 'center', padding: 2 }}
            onMouseEnter={e => e.currentTarget.style.color = '#ccc'}
            onMouseLeave={e => e.currentTarget.style.color = '#666'}
          >
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M3 6h18"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/>
              <line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/>
            </svg>
          </span>
          <span
            onClick={handleDeleteAll}
            title="全部删除"
            style={{ cursor: 'pointer', color: '#666', display: 'flex', alignItems: 'center', padding: 2 }}
            onMouseEnter={e => e.currentTarget.style.color = '#f44336'}
            onMouseLeave={e => e.currentTarget.style.color = '#666'}
          >
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
            </svg>
          </span>
          <span
            onClick={onClose}
            style={{ cursor: 'pointer', color: '#666', fontSize: 16, lineHeight: 1 }}
          >✕</span>
        </div>
      </div>

      <div style={{ flex: 1, overflowY: 'auto' }}>
        {sessions.length === 0 && !loading && (
          <div style={{ padding: '24px 14px', color: '#666', fontSize: 12, textAlign: 'center' }}>
            暂无对话记录
          </div>
        )}
        {sessions.map(s => (
          <div
            key={s.id}
            onClick={() => handleSelect(s)}
            title={s.session_path || undefined}
            style={{
              padding: '10px 14px', cursor: 'pointer',
              borderBottom: '1px solid rgba(255,255,255,0.03)',
              transition: 'background 0.12s',
            }}
            onMouseEnter={e => e.currentTarget.style.background = 'rgba(255,255,255,0.04)'}
            onMouseLeave={e => e.currentTarget.style.background = 'transparent'}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <span style={{
                fontSize: 12, color: s.status === 'running' ? '#4CAF50' : '#555',
                flexShrink: 0,
              }}>
                {s.status === 'running' ? '●' : '○'}
              </span>
              <span style={{
                fontSize: 13, color: '#ccc', flex: 1,
                whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
              }}>
                {s.goal || s.id}
              </span>
              <span
                onClick={e => handleDelete(e, s)}
                title="删除对话"
                style={{
                  color: '#555', fontSize: 11, cursor: 'pointer', flexShrink: 0,
                  opacity: 0, transition: 'opacity 0.15s', padding: '2px 4px',
                }}
                onMouseEnter={e => { e.currentTarget.style.opacity = '1'; e.currentTarget.style.color = '#f44336'; }}
                onMouseLeave={e => { e.currentTarget.style.opacity = '0'; e.currentTarget.style.color = '#555'; }}
              >✕</span>
            </div>
            <div style={{ fontSize: 11, color: '#555', marginTop: 2, paddingLeft: 18 }}>
              {formatTime(s.created_at)}
              {s.workspace_label && <span style={{ marginLeft: 8 }}>📁 {s.workspace_label}</span>}
            </div>
          </div>
        ))}
        {loading && (
          <div style={{ padding: '12px 14px', color: '#666', fontSize: 12, textAlign: 'center' }}>
            加载中…
          </div>
        )}
        {hasMore && !loading && (
          <div
            onClick={() => loadMore(offset)}
            style={{
              padding: '10px 14px', color: '#4CAF50', fontSize: 12,
              textAlign: 'center', cursor: 'pointer',
              transition: 'background 0.12s',
            }}
            onMouseEnter={e => e.currentTarget.style.background = 'rgba(76,175,80,0.08)'}
            onMouseLeave={e => e.currentTarget.style.background = 'transparent'}
          >
            加载更多
          </div>
        )}
      </div>
    </div>
  );
}
