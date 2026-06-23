import { useState, useEffect, useRef, type ReactNode } from 'react';
import { useStore, persistPinnedIds } from '../store';
import { api } from '../wails';
import type { MasterTask, SummonedItem } from '../types';
import { useMenu } from './ContextMenu';
import ConfirmDialog from './ConfirmDialog';

function GroupHeader({ label, title, icon, open, onToggle, action }: { label: string; title?: string; icon: ReactNode; open: boolean; onToggle: () => void; action?: ReactNode }) {
  const [hover, setHover] = useState(false);
  return (
    <div className="tree-dir" onClick={onToggle} title={title} onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)} style={{ display: 'flex', alignItems: 'center', gap: 4, cursor: 'pointer' }}>
      <svg width="8" height="5" viewBox="0 0 8 5" style={{
        opacity: 0.5, flexShrink: 0, transition: 'transform 0.15s',
        transform: open ? 'rotate(0deg)' : 'rotate(-90deg)',
      }}>
        <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
      </svg>
      {icon}
      <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{label}</span>
      {action && <span style={{ opacity: hover ? 1 : 0, transition: 'opacity 0.15s' }}>{action}</span>}
    </div>
  );
}

function WorkspaceGroup({ dir, tasks, expanded, toggleGroup, selMasterTaskId, selectMasterTask, pinnedTaskIds, pinTask, onDelete, removeWorkspace, openWorkspace }: {
  dir: string; tasks: MasterTask[]; expanded: Set<string>; toggleGroup: (k: string) => void;
  selMasterTaskId: string | null; selectMasterTask: (id: string) => Promise<void>;
  pinnedTaskIds: string[]; pinTask: (id: string) => void; onDelete: (id: string, goal: string) => void;
  removeWorkspace: (dir: string) => Promise<void>; openWorkspace: (dir: string) => Promise<void>;
}) {
  const menu = useMenu();
  const [hover, setHover] = useState(false);
  const isOpen = expanded.has(dir);
  const label = dir.split(/[/\\]/).pop() || dir;
  return (
    <div>
      <div className="tree-dir" onClick={() => toggleGroup(dir)} title={dir} onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)} style={{ display: 'flex', alignItems: 'center', gap: 4, cursor: 'pointer', position: 'relative', paddingLeft: 22 }}>
        <svg width="8" height="5" viewBox="0 0 8 5" style={{
          opacity: 0.5, flexShrink: 0, transition: 'transform 0.15s',
          transform: isOpen ? 'rotate(0deg)' : 'rotate(-90deg)',
        }}>
          <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
        </svg>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0, opacity: 0.5 }}>
          <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>
        </svg>
        <span>{label}</span>
        <span
          ref={menu.ref}
          onMouseDown={e => { e.stopPropagation(); }}
          onClick={e => { e.stopPropagation(); menu.showWith([
            { label: '新对话', onClick: async () => { await openWorkspace(dir); useStore.setState({ activeFunction: 'create' }); } },
            { label: '移除', onClick: () => removeWorkspace(dir) },
          ]); }}
          onContextMenu={e => { e.preventDefault(); e.stopPropagation(); menu.showWith([
            { label: '新对话', onClick: async () => { await openWorkspace(dir); useStore.setState({ activeFunction: 'create' }); } },
            { label: '移除', onClick: () => removeWorkspace(dir) },
          ]); }}
          style={{ marginLeft: 'auto', opacity: hover ? 1 : 0, transition: 'opacity 0.15s', cursor: 'pointer', display: 'flex', alignItems: 'center', justifyContent: 'center', width: 24, height: 24, padding: 4, boxSizing: 'content-box' }}
        >
          <svg width="14" height="3" viewBox="0 0 14 3" fill="#888">
            <circle cx="1.5" cy="1.5" r="1.5"/><circle cx="7" cy="1.5" r="1.5"/><circle cx="12.5" cy="1.5" r="1.5"/>
          </svg>
          {menu.portal}
        </span>
      </div>
      {isOpen && tasks.length > 0 && tasks.map(t => (
        <TaskItem key={t.id} t={t} selMasterTaskId={selMasterTaskId} selectMasterTask={selectMasterTask} pinnedTaskIds={pinnedTaskIds} pinTask={pinTask} onDelete={onDelete} indent={28} />
      ))}
      {isOpen && tasks.length === 0 && (
        <div className="empty-hint" style={{ padding: '4px 14px 4px 28px' }}>暂无对话</div>
      )}
    </div>
  );
}

function TaskItem({ t, selMasterTaskId, selectMasterTask, pinnedTaskIds, pinTask, onDelete, indent }: {
  t: MasterTask; selMasterTaskId: string | null; selectMasterTask: (id: string) => Promise<void>;
  pinnedTaskIds: string[]; pinTask: (id: string) => void; onDelete: (id: string, goal: string) => void; indent?: number;
}) {
  const [hover, setHover] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [editValue, setEditValue] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const menu = useMenu();
  const isPinned = pinnedTaskIds.includes(t.id);
  const pct = t.task_count > 0 ? Math.round(t.done_count / t.task_count * 100) : 0;
  const done = t.status === 'done';
  const running = t.status === 'running';
  const padLeft = (indent || 0) + 22;

  const startRename = () => {
    setEditValue(t.goal);
    setRenaming(true);
    menu.hide();
    setTimeout(() => inputRef.current?.select(), 0);
  };

  return (
    <div
      className={`tree-item ${selMasterTaskId === t.id ? 'active' : ''}`}
      onClick={() => { if (!renaming) selectMasterTask(t.id); }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
    >
      <span style={{
        width: 6, height: 6, borderRadius: '50%', flexShrink: 0, marginLeft: padLeft - (indent || 0),
        background: done ? '#4CAF50' : running ? '#d29922' : '#666',
        boxShadow: done ? '0 0 4px #4CAF50' : running ? '0 0 6px #d29922' : 'none',
        animation: running ? 'pulse 1.2s ease-in-out infinite' : 'none',
      }} />
      {renaming ? (
        <input
          ref={inputRef}
          value={editValue}
          onChange={e => setEditValue(e.target.value)}
          onBlur={() => {
            const val = editValue.trim();
            if (val && val !== t.goal) {
              api.renameMasterTask(t.id, val);
              useStore.getState().loadMasterTasks();
            }
            setRenaming(false);
          }}
          onKeyDown={e => {
            if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
            if (e.key === 'Escape') setRenaming(false);
          }}
          style={{
            background: '#2a2a2a', border: '1px solid #4CAF50', borderRadius: 4,
            color: '#fff', fontSize: 12, padding: '1px 6px', outline: 'none',
            width: '100%',
          }}
        />
      ) : (
        <span className="tree-label">{t.goal.length > 24 ? t.goal.slice(0, 22) + '…' : t.goal}</span>
      )}
      {t.task_count > 0 && <span className="tree-meta">{t.done_count}/{t.task_count}</span>}
      {pct > 0 && <div className="mini-bar" style={{ width: `${pct}%` }} />}
      <div style={{ display: 'flex', alignItems: 'center', gap: 2, opacity: hover ? 1 : 0, transition: 'opacity 0.15s', marginLeft: 'auto' }}>
        <span onClick={e => { e.stopPropagation(); pinTask(t.id); }} style={{ cursor: 'pointer', opacity: isPinned ? 1 : 0.4, display: 'flex' }} title={isPinned ? '取消置顶' : '置顶'}>
          {isPinned ? (
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <line x1="12" y1="17" x2="12" y2="22" /><path d="M5 17h14v-1.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1v4.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24Z" /><line x1="2" y1="2" x2="22" y2="22" />
            </svg>
          ) : (
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <line x1="12" y1="17" x2="12" y2="22" /><path d="M5 17h14v-1.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1v4.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24Z" />
            </svg>
          )}
        </span>
        <span
          ref={menu.ref}
          onMouseDown={e => { e.stopPropagation(); }}
          onClick={e => { e.stopPropagation(); menu.showWith([
            { label: '重命名', onClick: startRename },
            { label: isPinned ? '取消置顶' : '置顶', onClick: () => pinTask(t.id) },
            { label: '删除', onClick: () => onDelete(t.id, t.goal) },
          ]); }}
          style={{ position: 'relative', display: 'flex', alignItems: 'center', justifyContent: 'center', width: 24, height: 24, cursor: 'pointer' }}
        >
          <svg width="14" height="3" viewBox="0 0 14 3" fill="#888">
            <circle cx="1.5" cy="1.5" r="1.5"/><circle cx="7" cy="1.5" r="1.5"/><circle cx="12.5" cy="1.5" r="1.5"/>
          </svg>
          {menu.portal}
        </span>
      </div>
    </div>
  );
}

const whaleIcon = (
  <img src="/whale.png" width="16" height="16" alt="" style={{ borderRadius: 3, flexShrink: 0 }} />
);

function agentIcon(agent: string, label: string) {
  if (!agent) return whaleIcon;
  const isTeam = agent.startsWith('team:');
  return (
    <div style={{
      width: 18, height: 18, borderRadius: '50%', flexShrink: 0,
      background: isTeam ? 'linear-gradient(135deg, #9C27B0, #2196F3)' : 'linear-gradient(135deg, #4CAF50, #00BCD4)',
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      color: '#fff', fontSize: 9, fontWeight: 700,
    }}>{label[0]}</div>
  );
}

const pinIcon = (
  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="#d29922" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0, opacity: 0.7 }}>
    <line x1="12" y1="17" x2="12" y2="22" /><path d="M5 17h14v-1.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1v4.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24Z" />
  </svg>
);

function AgentGroup({ agent, wsMap, expanded, toggleGroup, selMasterTaskId, selectMasterTask, pinnedTaskIds, pinTask, onDelete, removeWorkspace, openWorkspace, summonedItems, dismissItem }: {
  agent: string;
  wsMap?: Map<string, MasterTask[]>;
  expanded: Set<string>;
  toggleGroup: (k: string) => void;
  selMasterTaskId: string | null;
  selectMasterTask: (id: string) => Promise<void>;
  pinnedTaskIds: string[];
  pinTask: (id: string) => void;
  onDelete: (id: string, goal: string) => void;
  removeWorkspace: (dir: string) => Promise<void>;
  openWorkspace: (dir: string) => Promise<void>;
  summonedItems: SummonedItem[];
  dismissItem: (item: SummonedItem) => void;
}) {
  const menu = useMenu();
  const agentKey = agent || '__whale__';
  const agentOpen = expanded.has(agentKey);
  const label = !agent ? 'Whale' : (summonedItems.find(s => `${s.type}:${s.name}` === agent)?.label || agent);
  const icon = agentIcon(agent, label);

  const wsEntries = wsMap ? [...wsMap.entries()] : [];
  const wsWithTasks = wsEntries.filter(([, tasks]) => tasks.length > 0);
  wsWithTasks.sort(([a], [b]) => {
    if (!a) return 1;
    if (!b) return -1;
    return a.localeCompare(b);
  });
  const hasAnyTasks = wsWithTasks.length > 0;

  const summonedItem = summonedItems.find(s => `${s.type}:${s.name}` === agent);
  const menuItems = summonedItem ? [
    { label: '新对话', onClick: () => {
      useStore.setState({ preselectedExpert: summonedItem.name, activeFunction: 'create' });
    }},
    { label: '移除', onClick: () => dismissItem(summonedItem) },
  ] : !agent ? [
    { label: '新对话', onClick: () => useStore.setState({ activeFunction: 'create' }) },
  ] : [];

  return (
    <div>
      <GroupHeader
        label={label}
        icon={icon}
        open={agentOpen}
        onToggle={() => toggleGroup(agentKey)}
        action={menuItems.length > 0 ? (
          <span
            ref={menu.ref}
            onMouseDown={e => { e.stopPropagation(); }}
            onClick={e => { e.stopPropagation(); menu.showWith(menuItems); }}
            style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', width: 20, height: 20, cursor: 'pointer' }}
          >
            <svg width="12" height="3" viewBox="0 0 12 3" fill="#888">
              <circle cx="1.5" cy="1.5" r="1.5"/><circle cx="6" cy="1.5" r="1.5"/><circle cx="10.5" cy="1.5" r="1.5"/>
            </svg>
            {menu.portal}
          </span>
        ) : undefined}
      />
      {agentOpen && hasAnyTasks && wsWithTasks.map(([dir, tasks]) => (
        <WorkspaceGroup
          key={dir || '__no_ws__'}
          dir={dir || '无工作空间'}
          tasks={tasks}
          expanded={expanded}
          toggleGroup={toggleGroup}
          selMasterTaskId={selMasterTaskId}
          selectMasterTask={selectMasterTask}
          pinnedTaskIds={pinnedTaskIds}
          pinTask={pinTask}
          onDelete={onDelete}
          removeWorkspace={removeWorkspace}
          openWorkspace={openWorkspace}
        />
      ))}
      {agentOpen && !hasAnyTasks && (
        <div className="empty-hint" style={{ padding: '4px 14px 4px 28px' }}>暂无对话</div>
      )}
    </div>
  );
}

export default function TaskTree() {
  const { masterTasks, selMasterTaskId, selectMasterTask, pinnedTaskIds, openWorkspaces, openWorkspace, removeWorkspace, summonedItems } = useStore();
  const allTasks = masterTasks || [];
  const [deleteTarget, setDeleteTarget] = useState<{ id: string; goal: string } | null>(null);

  const pinned = allTasks.filter(t => pinnedTaskIds.includes(t.id));
  const unpinned = allTasks.filter(t => !pinnedTaskIds.includes(t.id));
  const [expanded, setExpanded] = useState<Set<string>>(new Set(['__pinned__', '__whale__']));

  useEffect(() => {
    setExpanded(prev => {
      let next = prev;
      for (const ws of openWorkspaces) {
        if (!next.has(ws)) {
          if (next === prev) next = new Set(prev);
          next.add(ws);
        }
      }
      for (const s of summonedItems) {
        const key = `${s.type}:${s.name}`;
        if (!next.has(key)) {
          if (next === prev) next = new Set(prev);
          next.add(key);
        }
      }
      return next;
    });
  }, [openWorkspaces, summonedItems]);

  const toggleGroup = (key: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      next.has(key) ? next.delete(key) : next.add(key);
      return next;
    });
  };

  const pinTask = (id: string) => {
    const current = useStore.getState().pinnedTaskIds;
    let next: string[];
    if (current.includes(id)) {
      next = current.filter(i => i !== id);
    } else {
      next = [...current, id];
    }
    useStore.setState({ pinnedTaskIds: next });
    persistPinnedIds(next);
  };

  const agentMap = new Map<string, Map<string, MasterTask[]>>();
  const getWsMap = (agent: string) => {
    if (!agentMap.has(agent)) agentMap.set(agent, new Map());
    return agentMap.get(agent)!;
  };

  for (const t of unpinned) {
    const agent = t.agent || '';
    const wsMap = getWsMap(agent);
    const dir = t.workspace_path || '';
    if (!wsMap.has(dir)) wsMap.set(dir, []);
    wsMap.get(dir)!.push(t);
  }

  const agentKeys: string[] = [];
  for (const s of summonedItems) {
    const key = `${s.type}:${s.name}`;
    if (!agentKeys.includes(key)) agentKeys.push(key);
  }
  for (const agent of agentMap.keys()) {
    if (agent && !agentKeys.includes(agent) && !summonedItems.some(s => `${s.type}:${s.name}` === agent)) {
      agentKeys.push(agent);
    }
  }
  if (!agentKeys.includes('')) {
    agentKeys.push('');
  }

  const handleDelete = (id: string, goal: string) => setDeleteTarget({ id, goal });

  const dismissItem = (item: SummonedItem) => {
    const next = useStore.getState().summonedItems.filter(s => !(s.name === item.name && s.type === item.type));
    useStore.setState({ summonedItems: next });
    api.saveSummonedItems(next);
  };

  return (
    <>
      {pinned.length > 0 && (
        <div>
          <GroupHeader label="已置顶" icon={pinIcon} open={expanded.has('__pinned__')} onToggle={() => toggleGroup('__pinned__')} />
          {expanded.has('__pinned__') && pinned.map(t => (
            <TaskItem key={t.id} t={t} selMasterTaskId={selMasterTaskId} selectMasterTask={selectMasterTask} pinnedTaskIds={pinnedTaskIds} pinTask={pinTask} onDelete={handleDelete} />
          ))}
        </div>
      )}

      {agentKeys.map(agent => (
        <AgentGroup
          key={agent || '__whale__'}
          agent={agent}
          wsMap={agentMap.get(agent)}
          expanded={expanded}
          toggleGroup={toggleGroup}
          selMasterTaskId={selMasterTaskId}
          selectMasterTask={selectMasterTask}
          pinnedTaskIds={pinnedTaskIds}
          pinTask={pinTask}
          onDelete={handleDelete}
          removeWorkspace={removeWorkspace}
          openWorkspace={openWorkspace}
          summonedItems={summonedItems}
          dismissItem={dismissItem}
        />
      ))}

      <ConfirmDialog
        open={deleteTarget !== null}
        title="删除对话"
        message={`确定删除「${deleteTarget?.goal || ''}」吗？此操作不可恢复，将同时删除关联的任务数据。`}
        confirmLabel="删除"
        danger
        onConfirm={() => {
          if (deleteTarget) { api.deleteSession(deleteTarget.id); useStore.getState().loadMasterTasks(); }
          setDeleteTarget(null);
        }}
        onCancel={() => setDeleteTarget(null)}
      />
    </>
  );
}
