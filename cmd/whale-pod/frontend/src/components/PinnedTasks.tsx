import { useState } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import CollapsibleSection from './CollapsibleSection';
import { useMenu } from './ContextMenu';

function PinnedItem({ t, isActive, onSelect, onUnpin }: {
  t: { id: string; goal: string; status: string; task_count: number; done_count: number };
  isActive: boolean; onSelect: () => void; onUnpin: () => void;
}) {
  const [hover, setHover] = useState(false);
  const menu = useMenu();
  const done = t.status === 'done';
  const running = t.status === 'running';

  return (
    <div
      className={`tree-item ${isActive ? 'active' : ''}`}
      onClick={onSelect}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
    >
      <span style={{
        width: 6, height: 6, borderRadius: '50%', flexShrink: 0,
        background: done ? '#4CAF50' : running ? '#d29922' : '#666',
        boxShadow: done ? '0 0 4px #4CAF50' : running ? '0 0 6px #d29922' : 'none',
        animation: running ? 'pulse 1.2s ease-in-out infinite' : 'none',
      }} />
      <span className="tree-label">{t.goal.length > 24 ? t.goal.slice(0, 22) + '…' : t.goal}</span>
      {t.task_count > 0 && <span className="tree-meta">{t.done_count}/{t.task_count}</span>}
      <div style={{ display: 'flex', alignItems: 'center', gap: 2, opacity: hover ? 1 : 0, transition: 'opacity 0.15s', marginLeft: 'auto' }}>
        <span onClick={e => { e.stopPropagation(); onUnpin(); }}
          style={{ cursor: 'pointer', opacity: 1, flexShrink: 0, display: 'flex' }}
          title="取消置顶"
        >
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="#d29922" strokeWidth="2" strokeLinecap="round">
            <line x1="12" y1="17" x2="12" y2="22" /><path d="M5 17h14v-1.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1v4.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24Z" /><line x1="2" y1="2" x2="22" y2="22" />
          </svg>
        </span>
        <span
          ref={menu.ref}
          onClick={e => { e.stopPropagation(); e.preventDefault(); menu.showWith([
            { label: '取消置顶', onClick: onUnpin },
            { label: '删除', onClick: () => { if (confirm('此操作不可恢复，将同时删除关联的任务数据。确定删除？')) { api.deleteSession(t.id); useStore.getState().loadMasterTasks(); } } },
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

export default function PinnedTasks() {
  const { pinnedTaskIds, masterTasks, selMasterTaskId, selectMasterTask } = useStore();
  const pinned = masterTasks.filter(t => pinnedTaskIds.includes(t.id));

  const unpin = (id: string) => {
    const current = useStore.getState().pinnedTaskIds;
    useStore.setState({ pinnedTaskIds: current.filter(i => i !== id) });
  };

  const icon = (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0 }}>
      <line x1="12" y1="17" x2="12" y2="22" />
      <path d="M5 17h14v-1.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1v4.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24Z" />
    </svg>
  );

  return (
    <CollapsibleSection title="置顶任务" icon={icon} defaultOpen>
      {pinned.map(t => (
        <PinnedItem
          key={t.id}
          t={t}
          isActive={selMasterTaskId === t.id}
          onSelect={() => selectMasterTask(t.id)}
          onUnpin={() => unpin(t.id)}
        />
      ))}
      {pinned.length === 0 && (
        <div className="empty-hint" style={{ padding: '4px 14px' }}>暂无置顶任务</div>
      )}
    </CollapsibleSection>
  );
}
