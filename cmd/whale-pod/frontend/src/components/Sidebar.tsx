import { useState, useEffect, useMemo, useRef } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import type { SummonedItem, MasterTask } from '../types';

function SearchBox({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div style={{ padding: '8px 10px', position: 'relative' }}>
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="#888" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"
        style={{ position: 'absolute', left: 20, top: '50%', transform: 'translateY(-50%)', pointerEvents: 'none' }}>
        <circle cx="11" cy="11" r="8" /><line x1="21" y1="21" x2="16.65" y2="16.65" />
      </svg>
      <input
        type="text"
        value={value}
        onChange={e => onChange(e.target.value)}
        placeholder="搜索专家或对话…"
        style={{
          width: '100%', boxSizing: 'border-box', padding: '6px 10px 6px 30px',
          background: 'rgba(255,255,255,0.06)', border: '1px solid rgba(255,255,255,0.08)',
          borderRadius: 8, color: '#ccc', fontSize: 12, outline: 'none',
        }}
      />
    </div>
  );
}

function AgentAvatar({ item }: { item: SummonedItem | { type: 'whale'; name: string; label: string } }) {
  if (item.type === 'whale') {
    return <img src="/whale.png" width="28" height="28" alt="" style={{ borderRadius: 6, flexShrink: 0 }} />;
  }
  const isTeam = item.type === 'team';
  return (
    <div style={{
      width: 28, height: 28, borderRadius: '50%', flexShrink: 0,
      background: isTeam ? 'linear-gradient(135deg, #9C27B0, #2196F3)' : 'linear-gradient(135deg, #4CAF50, #00BCD4)',
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      color: '#fff', fontSize: 12, fontWeight: 700,
    }}>{item.label[0]}</div>
  );
}

function AgentRow({ item, active, onClick, onContextMenu, lastPreview }: {
  item: SummonedItem | { type: 'whale'; name: string; label: string };
  active: boolean;
  onClick: () => void;
  onContextMenu?: (e: React.MouseEvent) => void;
  lastPreview?: string;
}) {
  const [hover, setHover] = useState(false);
  const desc = item.type !== 'whale' && 'description' in item ? (item as SummonedItem).description : '';
  const preview = lastPreview || desc;
  return (
    <div
      onClick={onClick}
      onContextMenu={onContextMenu}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex', alignItems: 'center', gap: 10, padding: '10px 14px',
        cursor: 'pointer', transition: 'background 0.12s',
        background: active ? 'rgba(76,175,80,0.15)' : hover ? 'rgba(255,255,255,0.04)' : 'transparent',
        borderLeft: active ? '3px solid #4CAF50' : '3px solid transparent',
      }}
    >
      <AgentAvatar item={item} />
      <div style={{ flex: 1, overflow: 'hidden' }}>
        <div style={{ fontSize: 13, color: active ? '#eee' : '#ccc', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
          {item.type === 'whale' ? '' : item.type === 'team' ? '👥 ' : ''}{item.label}
        </div>
        {preview && (
          <div style={{ fontSize: 11, color: '#666', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', marginTop: 4 }}>
            {preview.slice(0, 36)}
          </div>
        )}
      </div>
    </div>
  );
}

const whaleItem = { type: 'whale' as const, name: '', label: 'Whale' };

export default function Sidebar() {
  const toggleSidebar = useStore(s => s.toggleSidebar);
  const collapsed = useStore(s => s.sidebarCollapsed);
  const summonedItems = useStore(s => s.summonedItems);
  const selAgentId = useStore(s => s.selAgentId);
  const selectAgent = useStore(s => s.selectAgent);
  const dismissItem = useStore(s => s.dismissItem);
  const masterTasks = useStore(s => s.masterTasks);
  const selectMasterTask = useStore(s => s.selectMasterTask);
  const addTab = useStore(s => s.addTab);
  const [search, setSearch] = useState('');
  const [lastMsgs, setLastMsgs] = useState<Record<string, string>>({});

  const allAgents: (SummonedItem | typeof whaleItem)[] = useMemo(() => {
    const seen = new Set<string>();
    const result: (SummonedItem | typeof whaleItem)[] = [];
    for (const item of summonedItems) {
      const key = item.type + ':' + item.name;
      if (!seen.has(key)) { seen.add(key); result.push(item); }
    }
    for (const t of masterTasks) {
      const a = t.agent || '';
      if (!a) continue;
      const [type, ...rest] = a.split(':');
      const name = rest.join(':');
      const key = type + ':' + name;
      if (!seen.has(key)) {
        seen.add(key);
        result.push({
          type: type as 'expert' | 'team',
          name,
          label: name,
          description: '',
        });
      }
    }
    if (!seen.has('whale:')) result.push(whaleItem);
    return result;
  }, [summonedItems, masterTasks]);

  // 异步加载各 agent 最后一个对话的最后一条消息（防抖 + 取消）
  const lastMsgAbortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    // Cancel any in-flight requests.
    lastMsgAbortRef.current?.abort();
    const controller = new AbortController();
    lastMsgAbortRef.current = controller;

    const timer = setTimeout(async () => {
      const result: Record<string, string> = {};
      for (const item of allAgents) {
        if (controller.signal.aborted) break;
        const agentKey = item.type === 'whale' ? '' : item.type + ':' + item.name;
        const tasks = masterTasks.filter(t => (t.agent || '') === agentKey);
        const lastTask = tasks.length > 0 ? tasks[tasks.length - 1] : null;
        if (!lastTask) continue;
        try {
          const msgs = await api.getChatMessages(lastTask.id);
          if (!controller.signal.aborted && msgs && msgs.length > 0) {
            result[agentKey] = msgs[msgs.length - 1].content;
          }
        } catch { /* ignore */ }
      }
      if (!controller.signal.aborted) setLastMsgs(result);
    }, 300); // 300ms debounce

    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [masterTasks, summonedItems, allAgents]);

  const q = search.trim().toLowerCase();
  const filtered = q
    ? allAgents.filter(a => a.label.toLowerCase().includes(q) || (a.type !== 'whale' && (a as SummonedItem).description?.toLowerCase().includes(q)))
    : allAgents;

  const matchedTasks: MasterTask[] = q
    ? masterTasks.filter(t => t.goal?.toLowerCase().includes(q)).slice(0, 10)
    : [];

  const handleTaskClick = (task: MasterTask) => {
    // 确保 agent 上下文正确
    const agentKey = task.agent || '';
    if (useStore.getState().selAgentId !== agentKey) {
      useStore.setState({ selAgentId: agentKey });
    }
    useStore.getState().addTab(task.id);
    selectMasterTask(task.id);
  };

  const handleSummon = () => {
    useStore.setState({ activeFunction: 'expert' });
  };

  const handleToggleSettings = () => {
    // fire a custom event that App.tsx listens to
    window.dispatchEvent(new CustomEvent('toggle-settings'));
  };

  return (
    <div
      id="sidebar"
      style={{
        width: collapsed ? 0 : undefined,
        minWidth: collapsed ? 0 : undefined,
        borderRightWidth: collapsed ? 0 : undefined,
        overflow: 'hidden',
      }}
    >
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
        <img src="/whale.png" width="36" height="36" alt="Whale" />
        <span className="titlebar-label">Whale Pod</span>
        <span
          onClick={e => { e.stopPropagation(); toggleSidebar(); }}
          title="隐藏侧栏"
          style={{ marginLeft: 'auto', cursor: 'pointer', opacity: 0.4, display: 'flex', alignItems: 'center', padding: 4 }}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <rect x="3" y="3" width="18" height="18" rx="2" ry="2" />
            <line x1="9" y1="3" x2="9" y2="21" />
            <line x1="15" y1="12" x2="9" y2="12" />
          </svg>
        </span>
      </div>

      <SearchBox value={search} onChange={setSearch} />

      <div style={{ flex: 1, overflowY: 'auto', overflowX: 'hidden' }}>
        {filtered.map(item => {
          const agentKey = item.type + ':' + item.name;
          const agentKeyNorm = item.type === 'whale' ? '' : agentKey;
          const lastPreview = lastMsgs[agentKeyNorm] || undefined;
          return (
            <AgentRow
              key={agentKey}
              item={item}
              active={selAgentId === agentKeyNorm}
              onClick={() => selectAgent(agentKeyNorm, item.name, item.type as 'expert' | 'team' | 'whale')}
              onContextMenu={item.type !== 'whale' ? (e) => {
                e.preventDefault();
                if (confirm(`移除「${item.label}」？`)) {
                  dismissItem(item.name, item.type);
                }
              } : undefined}
              lastPreview={lastPreview}
            />
          );
        })}
        {filtered.length === 0 && matchedTasks.length === 0 && (
          <div style={{ padding: '20px 14px', color: '#666', fontSize: 12, textAlign: 'center' }}>
            未找到匹配的专家
          </div>
        )}
        {matchedTasks.length > 0 && (
          <>
            <div style={{ padding: '6px 14px 2px', fontSize: 11, color: '#555', fontWeight: 600 }}>匹配的对话</div>
            {matchedTasks.map(t => (
              <div
                key={t.id}
                onClick={() => handleTaskClick(t)}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '6px 14px',
                  cursor: 'pointer', transition: 'background 0.12s',
                }}
                onMouseEnter={e => e.currentTarget.style.background = 'rgba(255,255,255,0.04)'}
                onMouseLeave={e => e.currentTarget.style.background = 'transparent'}
              >
                <span style={{ fontSize: 11, color: t.status === 'running' ? '#4CAF50' : '#555', flexShrink: 0 }}>
                  {t.status === 'running' ? '●' : '○'}
                </span>
                <span style={{
                  fontSize: 12, color: '#aaa', flex: 1,
                  whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
                }}>
                  {t.goal || t.id}
                </span>
              </div>
            ))}
          </>
        )}
      </div>

      <div style={{
        borderTop: '1px solid rgba(255,255,255,0.06)', padding: '8px 10px',
        flexShrink: 0, display: 'flex', gap: 6,
      }}>
        <button
          onClick={handleSummon}
          style={{
            flex: 1, padding: '8px 0', borderRadius: 8, border: '1px solid rgba(255,255,255,0.15)',
            background: 'transparent', color: '#888', fontSize: 12, cursor: 'pointer',
            display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 6,
            transition: 'background 0.15s, color 0.15s',
          }}
          onMouseEnter={e => { e.currentTarget.style.background = 'rgba(255,255,255,0.04)'; e.currentTarget.style.color = '#aaa'; }}
          onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = '#888'; }}
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <circle cx="12" cy="12" r="10" /><line x1="12" y1="8" x2="12" y2="16" /><line x1="8" y1="12" x2="16" y2="12" />
          </svg>
          召唤专家
        </button>
        <button
          onClick={handleToggleSettings}
          title="设置 (Ctrl+,)"
          style={{
            width: 36, height: 36, borderRadius: 8, border: '1px solid rgba(255,255,255,0.12)',
            background: 'transparent', color: '#888', fontSize: 16, cursor: 'pointer',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            flexShrink: 0, transition: 'all 0.15s',
          }}
          onMouseEnter={e => { e.currentTarget.style.background = 'rgba(255,255,255,0.08)'; e.currentTarget.style.color = '#ccc'; }}
          onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = '#888'; }}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <circle cx="12" cy="12" r="3"/>
            <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
          </svg>
        </button>
      </div>
    </div>
  );
}
