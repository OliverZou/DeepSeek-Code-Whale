import { useState, useEffect, useMemo, useRef } from 'react';
import { useStore } from '../store';
import type { Subtask, DialogueEntry } from '../types';
import DagView from './DagView';

const stateIcon = (state: string) => {
  switch (state) {
    case 'done': return '✅';
    case 'producing': case 'assigned': return '🔄';
    case 'verifying': return '🔍';
    case 'pending_confirmation': return '⚠️';
    case 'suspended': return '⏸';
    case 'failed': return '❌';
    default: return '⏳';
  }
};

const stateColor = (state: string) => {
  switch (state) {
    case 'done': return '#4CAF50';
    case 'producing': case 'assigned': return '#d29922';
    case 'verifying': return '#2196F3';
    case 'pending_confirmation': return '#FF9800';
    case 'suspended': return '#888';
    case 'failed': return '#f44336';
    default: return '#666';
  }
};

function TaskList({
  subtasks,
  selSubtaskId,
  onSelect,
}: {
  subtasks: Subtask[];
  selSubtaskId: string | null;
  onSelect: (id: string) => void;
}) {
  const sts = subtasks.filter(s => s.id !== '__leader__');
  if (sts.length === 0) {
    return <div style={{ color: '#666', fontSize: 12, padding: '12px 14px' }}>暂无任务</div>;
  }

  return (
    <div style={{ maxHeight: 200, overflow: 'auto', borderBottom: '1px solid var(--border)' }}>
      {sts.map(t => (
        <div
          key={t.id}
          onClick={() => onSelect(t.id)}
          style={{
            display: 'flex', alignItems: 'center', gap: 8, padding: '8px 14px',
            cursor: 'pointer', fontSize: 13, borderLeft: '3px solid transparent',
            background: selSubtaskId === t.id ? 'var(--active)' : 'transparent',
            borderLeftColor: selSubtaskId === t.id ? 'var(--accent)' : 'transparent',
            transition: 'background .15s',
          }}
          onMouseEnter={e => { if (selSubtaskId !== t.id) e.currentTarget.style.background = 'rgba(255,255,255,0.03)'; }}
          onMouseLeave={e => { if (selSubtaskId !== t.id) e.currentTarget.style.background = 'transparent'; }}
        >
          <span style={{ color: stateColor(t.state), fontSize: 14, flexShrink: 0 }}>{stateIcon(t.state)}</span>
          <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: '#ccc' }}>
            {t.title}
          </span>
          <span style={{ color: '#666', fontSize: 11, flexShrink: 0 }}>{t.role}</span>
        </div>
      ))}
    </div>
  );
}

function DialogueWithRoleFilter() {
  const { selSubtaskId, dialogue, leaderPlan } = useStore();
  const [roleFilter, setRoleFilter] = useState<string>('');
  const ref = useRef<HTMLDivElement>(null);

  const entries = selSubtaskId === '__leader__' ? (leaderPlan || []) : (dialogue || []);

  // Extract unique roles
  const roles = useMemo(() => {
    const set = new Set<string>();
    for (const e of entries) set.add(e.role);
    return [...set];
  }, [entries]);

  const filtered = roleFilter ? entries.filter(e => e.role === roleFilter) : entries;

  useEffect(() => {
    if (ref.current) {
      const atBottom = ref.current.scrollTop + ref.current.clientHeight >= ref.current.scrollHeight - 4;
      if (atBottom || filtered.length <= 3) ref.current.scrollTop = ref.current.scrollHeight;
    }
  }, [filtered]);

  if (entries.length === 0) {
    return (
      <div style={{ color: '#666', textAlign: 'center', padding: 40 }}>暂无对话数据</div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      {/* Role switcher */}
      <div style={{
        display: 'flex', gap: 4, padding: '8px 12px', borderBottom: '1px solid var(--border)',
        flexShrink: 0, flexWrap: 'wrap',
      }}>
        <button
          onClick={() => setRoleFilter('')}
          style={{
            padding: '2px 10px', borderRadius: 12, border: '1px solid var(--border)',
            background: roleFilter === '' ? 'var(--accent)' : 'transparent',
            color: roleFilter === '' ? '#fff' : '#888', fontSize: 11, cursor: 'pointer',
          }}
        >全部</button>
        {roles.slice(0, 6).map(r => (
          <button
            key={r}
            onClick={() => setRoleFilter(r === roleFilter ? '' : r)}
            style={{
              padding: '2px 10px', borderRadius: 12, border: '1px solid var(--border)',
              background: roleFilter === r ? 'var(--accent)' : 'transparent',
              color: roleFilter === r ? '#fff' : '#888', fontSize: 11, cursor: 'pointer',
              maxWidth: 100, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            }}
          >{r}</button>
        ))}
      </div>

      {/* Dialogue entries */}
      <div ref={ref} style={{ flex: 1, overflow: 'auto', padding: '8px 12px' }}>
        {filtered.map((e, i) => {
          const cls = e.role.startsWith('worker') || e.role.includes('round') ? 'role-worker'
            : e.role.startsWith('verifier') || e.role.startsWith('审查') ? 'role-verifier'
            : e.role.startsWith('📋') ? 'role-planner'
            : 'role-input';
          return (
            <div key={i} className={`msg ${cls}`}>
              <div className="role-label">{e.role}</div>
              <div className="msg-content">{e.content}</div>
            </div>
          );
        })}
        {filtered.length === 0 && (
          <div style={{ color: '#666', textAlign: 'center', padding: 20 }}>该角色暂无对话</div>
        )}
      </div>
    </div>
  );
}

function SubtaskOutput({ subtask }: { subtask: Subtask }) {
  if (!subtask.output) {
    return <div style={{ color: '#666', textAlign: 'center', padding: 40 }}>暂无产出</div>;
  }
  return (
    <div style={{ padding: 12, overflow: 'auto', height: '100%' }}>
      <pre style={{
        color: '#ccc', fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word',
        background: 'rgba(0,0,0,0.2)', padding: 12, borderRadius: 8, margin: 0,
      }}>
        {subtask.output}
      </pre>
    </div>
  );
}

export default function TaskPanel() {
  const { selMasterTaskId, selSubtaskId, subtasks, selectSubtask } = useStore();
  const [detailTab, setDetailTab] = useState<'dialogue' | 'dag' | 'output'>('dialogue');

  const sts = subtasks || [];
  const selectedTask = sts.find(s => s.id === selSubtaskId);

  // Auto-select first subtask if none selected
  useEffect(() => {
    const nonLeader = sts.filter(s => s.id !== '__leader__');
    if (nonLeader.length > 0 && !selSubtaskId) {
      selectSubtask(nonLeader[0].id);
    }
  }, []);

  if (!selMasterTaskId) {
    return <div className="empty-state">未选择对话</div>;
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      {/* Task list */}
      <TaskList
        subtasks={sts}
        selSubtaskId={selSubtaskId}
        onSelect={id => selectSubtask(id)}
      />

      {/* Detail area */}
      {selectedTask ? (
        <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
          {/* Detail header */}
          <div style={{
            padding: '8px 12px', borderBottom: '1px solid var(--border)',
            display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0,
          }}>
            <span style={{ color: stateColor(selectedTask.state), fontSize: 14 }}>
              {stateIcon(selectedTask.state)}
            </span>
            <span style={{ color: '#ccc', fontWeight: 600, fontSize: 13, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {selectedTask.title}
            </span>
            <span style={{ color: '#666', fontSize: 11 }}>{selectedTask.role}</span>
          </div>

          {/* Detail tabs */}
          <div style={{ display: 'flex', borderBottom: '1px solid var(--border)', flexShrink: 0, padding: '0 8px' }}>
            {([
              { id: 'dialogue', label: '对话' },
              { id: 'dag', label: '流程图' },
              { id: 'output', label: '产出' },
            ] as const).map(tab => (
              <div
                key={tab.id}
                className={`tab-btn ${detailTab === tab.id ? 'active' : ''}`}
                onClick={() => setDetailTab(tab.id)}
              >
                {tab.label}
              </div>
            ))}
          </div>

          {/* Tab content */}
          <div style={{ flex: 1, overflow: 'hidden' }}>
            {detailTab === 'dialogue' && <DialogueWithRoleFilter />}
            {detailTab === 'dag' && <DagView />}
            {detailTab === 'output' && <SubtaskOutput subtask={selectedTask} />}
          </div>
        </div>
      ) : sts.length === 0 ? (
        <div style={{ color: '#666', textAlign: 'center', padding: 40, flex: 1 }}>
          暂无任务数据
        </div>
      ) : (
        <div style={{ color: '#666', textAlign: 'center', padding: 40, flex: 1 }}>
          点击上方任务查看详情
        </div>
      )}
    </div>
  );
}
