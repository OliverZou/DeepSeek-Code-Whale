import { useState, useEffect, useMemo, useRef, useLayoutEffect } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import type { SummonedItem, MasterTask } from '../types';
import ConfirmDialog from './ConfirmDialog';
import { useMenu } from './ContextMenu';

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

function useFixedRight(sidebarRef: React.RefObject<HTMLDivElement>, rowRef: React.RefObject<HTMLDivElement>, leftOffset: number, active: boolean): React.CSSProperties {
  const [style, setStyle] = useState<React.CSSProperties>({});
  useLayoutEffect(() => {
    if (!active) return;
    const row = rowRef.current;
    const sb = sidebarRef.current;
    if (!row || !sb) return;
    const rr = row.getBoundingClientRect();
    const sr = sb.getBoundingClientRect();
    setStyle({
      position: 'fixed',
      left: sr.right - leftOffset,
      top: rr.top + rr.height / 2 - 11,
      zIndex: 10,
    });
  }, [active, leftOffset, sidebarRef]);
  return style;
}

function AgentRow({ item, active, onClick, onNewChat, onRemove, lastPreview, sidebarRef }: {
  item: SummonedItem | { type: 'whale'; name: string; label: string };
  active: boolean;
  onClick: () => void;
  onNewChat?: () => void;
  onRemove?: () => void;
  lastPreview?: string;
  sidebarRef: React.RefObject<HTMLDivElement>;
}) {
  const [hover, setHover] = useState(false);
  const rowRef = useRef<HTMLDivElement>(null);
  const showMenu = !!(onNewChat || onRemove);
  const dotStyle = useFixedRight(sidebarRef, rowRef, 30, hover && showMenu);
  const menu = useMenu();
  const desc = item.type !== 'whale' && 'description' in item ? (item as SummonedItem).description : '';
  const preview = lastPreview || desc;
  const menuItems = [
    ...(onNewChat ? [{ label: '新对话', onClick: onNewChat }] : []),
    ...(onRemove ? [{ label: '移除', onClick: onRemove }] : []),
  ];

  return (
    <div
      ref={rowRef}
      onClick={onClick}
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
      {showMenu && hover && (
        <span
          ref={menu.ref}
          onMouseDown={e => { e.stopPropagation(); }}
          onClick={e => { e.stopPropagation(); menu.showWith(menuItems); }}
          style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', width: 22, height: 22, cursor: 'pointer', ...dotStyle }}
        >
          <svg width="14" height="3" viewBox="0 0 14 3" fill="#aaa">
            <circle cx="1.5" cy="1.5" r="1.5"/><circle cx="7" cy="1.5" r="1.5"/><circle cx="12.5" cy="1.5" r="1.5"/>
          </svg>
          {menu.portal}
        </span>
      )}
    </div>
  );
}

const whaleItem = { type: 'whale' as const, name: '', label: 'Whale' };

function AgentSection({
  item, sidebarRef,
  agentKey, agentKeyNorm, lastPreview,
  agentTasks, isExpanded, isActive,
  selectAgent, setRemoveTarget,
  toggleExpand, handleTaskClick,
  selMasterTaskId,
}: {
  item: SummonedItem | typeof whaleItem;
  sidebarRef: React.RefObject<HTMLDivElement>;
  agentKey: string; agentKeyNorm: string; lastPreview?: string;
  agentTasks: MasterTask[]; isExpanded: boolean; isActive: boolean;
  selectAgent: (agentId: string, agentName: string, agentType: 'expert' | 'team' | 'whale') => void;
  setRemoveTarget: (item: SummonedItem | null) => void;
  toggleExpand: () => void;
  handleTaskClick: (task: MasterTask) => void;
  selMasterTaskId: string | null;
}) {
  const rowRef = useRef<HTMLDivElement>(null);
  const hasTasks = agentTasks.length > 0;
  const countStyle = useFixedRight(sidebarRef, rowRef, 58, hasTasks);
  return (
    <div>
      <div ref={rowRef} style={{ display: 'flex', alignItems: 'center' }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <AgentRow
            item={item}
            active={isActive}
            onClick={() => selectAgent(agentKeyNorm, item.name, item.type as 'expert' | 'team' | 'whale')}
            onNewChat={item.type !== 'whale' ? () => useStore.setState({ preselectedExpert: item.name, activeFunction: 'create' }) : undefined}
            onRemove={item.type !== 'whale' ? () => setRemoveTarget(item as SummonedItem) : undefined}
            lastPreview={lastPreview}
            sidebarRef={sidebarRef}
          />
        </div>
        {hasTasks && (
          <span
            onClick={e => { e.stopPropagation(); toggleExpand(); }}
            style={{ display: 'flex', alignItems: 'center', gap: 2, cursor: 'pointer', padding: '4px 8px', color: '#aaa', fontSize: 10, userSelect: 'none', ...countStyle }}
          >
            <span style={{ color: '#aaa', marginRight: 2 }}>{agentTasks.length}</span>
            <svg width="8" height="5" viewBox="0 0 8 5" style={{ transform: isExpanded ? 'rotate(0deg)' : 'rotate(-90deg)', transition: 'transform 0.15s' }}>
              <path d="M0 0l4 5 4-5" fill="none" stroke="#aaa" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
            </svg>
          </span>
        )}
      </div>
      {isExpanded && hasTasks && (
        <div style={{ paddingLeft: 24, borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
          {agentTasks.slice(0, 20).map(t => (
            <div
              key={t.id}
              onClick={() => handleTaskClick(t)}
              style={{
                display: 'flex', alignItems: 'center', gap: 6, padding: '5px 14px 5px 10px',
                cursor: 'pointer', borderRadius: 4, fontSize: 12,
                background: selMasterTaskId === t.id ? 'rgba(76,175,80,0.1)' : 'transparent',
                borderLeft: selMasterTaskId === t.id ? '2px solid #4CAF50' : '2px solid transparent',
                marginBottom: 1,
              }}
              onMouseEnter={e => { if (selMasterTaskId !== t.id) e.currentTarget.style.background = 'rgba(255,255,255,0.03)'; }}
              onMouseLeave={e => { if (selMasterTaskId !== t.id) e.currentTarget.style.background = 'transparent'; }}
            >
              <span style={{ width: 6, height: 6, borderRadius: '50%', flexShrink: 0, background: t.status === 'running' ? '#4CAF50' : t.status === 'done' ? '#888' : '#555' }} />
              <span style={{ flex: 1, color: selMasterTaskId === t.id ? '#ddd' : '#999', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{t.goal || '未命名对话'}</span>
              {t.task_count > 0 && <span style={{ color: '#FF9800', fontSize: 10, flexShrink: 0 }}>📋{t.task_count}</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

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
  const selMasterTaskId = useStore(s => s.selMasterTaskId);
  const [search, setSearch] = useState('');
  const [lastMsgs, setLastMsgs] = useState<Record<string, string>>({});
  const [expandedAgents, setExpandedAgents] = useState<Set<string>>(new Set());
  const [removeTarget, setRemoveTarget] = useState<SummonedItem | null>(null);
  const sidebarRef = useRef<HTMLDivElement>(null);

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

    // Capture current masterTasks so abort race doesn't use stale data.
    const captured = masterTasks;

    const timer = setTimeout(async () => {
      const result: Record<string, string> = {};
      for (const item of allAgents) {
        if (controller.signal.aborted) break;
        const agentKey = item.type === 'whale' ? '' : item.type + ':' + item.name;
        const agentTasks = captured.filter(t => (t.agent || '') === agentKey);
        // GetChatMessages returns tasks sorted by ModTime desc, so [0] is the most recent.
        const recentTask = agentTasks.length > 0 ? agentTasks[0] : null;
        if (!recentTask) continue;
        try {
          const msgs = await api.getChatMessages(recentTask.id);
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
  }, [allAgents]);

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
    const current = useStore.getState().activeFunction;
    useStore.setState({ activeFunction: current === 'settings' ? null : 'settings' });
  };

  return (
    <div
      id="sidebar"
      ref={sidebarRef}
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
          const agentTasks = masterTasks.filter(t => (t.agent || '') === agentKeyNorm);
          const isExpanded = expandedAgents.has(agentKey);
          const isActive = selAgentId === agentKeyNorm;
          const toggleExpand = () => {
            setExpandedAgents(prev => {
              const next = new Set(prev);
              if (next.has(agentKey)) next.delete(agentKey); else next.add(agentKey);
              return next;
            });
          };
          return (
            <AgentSection
              key={agentKey}
              item={item}
              sidebarRef={sidebarRef}
              agentKey={agentKey}
              agentKeyNorm={agentKeyNorm}
              lastPreview={lastPreview}
              agentTasks={agentTasks}
              isExpanded={isExpanded}
              isActive={isActive}
              selectAgent={selectAgent}
              setRemoveTarget={setRemoveTarget}
              toggleExpand={toggleExpand}
              handleTaskClick={handleTaskClick}
              selMasterTaskId={selMasterTaskId}
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

      <ConfirmDialog
        open={removeTarget !== null}
        title="移除专家"
        message={`确定移除「${removeTarget?.label || ''}」吗？移除后仍可重新召唤。`}
        confirmLabel="移除"
        danger
        onConfirm={() => {
          if (removeTarget) { dismissItem(removeTarget.name, removeTarget.type); }
          setRemoveTarget(null);
        }}
        onCancel={() => setRemoveTarget(null)}
      />
    </div>
  );
}
