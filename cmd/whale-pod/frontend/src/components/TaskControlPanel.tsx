import { useState, useRef, useEffect } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import type { TeamChatMessage, TaskConfirmation, Subtask } from '../types';
import DagView from './DagView';

function ConfirmationCard({ conf }: { conf: TaskConfirmation }) {
  const [feedback, setFeedback] = useState('');
  const [busy, setBusy] = useState(false);
  const handle = async (approved: boolean) => {
    setBusy(true);
    await useStore.getState().confirmTask(conf.task_id, approved, feedback);
    setBusy(false);
  };
  return (
    <div style={{
      background: 'rgba(255,152,0,0.08)', border: '1px solid rgba(255,152,0,0.3)',
      borderRadius: 10, padding: 14, marginBottom: 10,
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 8 }}>
        <span style={{ fontSize: 14 }}>⚠️</span>
        <span style={{ color: '#FF9800', fontWeight: 600, fontSize: 13 }}>{conf.role} · 请求确认</span>
      </div>
      <div style={{ color: '#ccc', fontSize: 13, lineHeight: 1.6, marginBottom: 10, whiteSpace: 'pre-wrap' }}>{conf.content}</div>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <button
          onClick={() => handle(true)} disabled={busy}
          style={{
            padding: '5px 16px', borderRadius: 6, border: 'none', cursor: 'pointer',
            background: '#4CAF50', color: '#fff', fontSize: 12, fontWeight: 600,
            opacity: busy ? 0.5 : 1,
          }}
        >✅ 确认继续</button>
        <button
          onClick={() => handle(false)} disabled={busy}
          style={{
            padding: '5px 16px', borderRadius: 6, border: '1px solid #666', cursor: 'pointer',
            background: 'transparent', color: '#aaa', fontSize: 12,
            opacity: busy ? 0.5 : 1,
          }}
        >❌ 拒绝</button>
        <input
          value={feedback} onChange={e => setFeedback(e.target.value)}
          placeholder="反馈（可选）"
          style={{
            flex: 1, background: '#1a1a1a', border: '1px solid #333', borderRadius: 4,
            color: '#ccc', fontSize: 12, padding: '4px 8px', outline: 'none',
          }}
        />
      </div>
    </div>
  );
}

function ChatBubble({ msg, roles }: { msg: TeamChatMessage; roles: string[] }) {
  const isHuman = msg.from === 'human';
  return (
    <div style={{
      display: 'flex', flexDirection: isHuman ? 'row-reverse' : 'row',
      marginBottom: 10, padding: '0 12px',
    }}>
      <div style={{ maxWidth: '75%' }}>
        <div style={{
          fontSize: 11, color: '#888', marginBottom: 3,
          textAlign: isHuman ? 'right' : 'left',
        }}>
          {isHuman ? '👤 你' : `🤖 ${msg.from}`}
          {msg.to && msg.to !== 'leader' && !isHuman ? '' : ''}
          {isHuman && msg.to && msg.to !== 'leader' ? ` → @${msg.to}` : ''}
        </div>
        <div style={{
          background: isHuman ? 'rgba(76,175,80,0.12)' : '#1e1e1e',
          border: isHuman ? '1px solid rgba(76,175,80,0.2)' : '1px solid #2a2a2a',
          borderRadius: isHuman ? '12px 4px 12px 12px' : '4px 12px 12px 12px',
          padding: '8px 12px', color: '#e0e0e0', fontSize: 13, lineHeight: 1.6,
          whiteSpace: 'pre-wrap',
        }}>
          {msg.content}
        </div>
      </div>
    </div>
  );
}

function ChatInput({ masterTaskId, roles }: { masterTaskId: string; roles: string[] }) {
  const [msg, setMsg] = useState('');
  const [targetRole, setTargetRole] = useState('');
  const [showRoles, setShowRoles] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const handleSend = async () => {
    if (!msg.trim()) return;
    await useStore.getState().sendTeamChat(msg.trim(), targetRole || undefined);
    setMsg('');
  };

  return (
    <div style={{ display: 'flex', gap: 6, padding: '8px 12px', borderTop: '1px solid #222' }}>
      <div style={{ position: 'relative' }}>
        <button
          onClick={() => setShowRoles(!showRoles)}
          style={{
            background: 'transparent', border: '1px solid #333', borderRadius: 6,
            color: targetRole ? '#4CAF50' : '#666', fontSize: 12, padding: '4px 10px',
            cursor: 'pointer', whiteSpace: 'nowrap',
          }}
        >{targetRole ? `@${targetRole}` : '@'}</button>
        {showRoles && (
          <div style={{
            position: 'absolute', bottom: '100%', left: 0, marginBottom: 4,
            background: '#1e1e1e', border: '1px solid #333', borderRadius: 8,
            maxHeight: 200, overflow: 'auto', minWidth: 140, zIndex: 10,
          }}>
            <div
              onClick={() => { setTargetRole(''); setShowRoles(false); }}
              style={{ padding: '6px 12px', cursor: 'pointer', color: targetRole === '' ? '#4CAF50' : '#aaa', fontSize: 12 }}
            >Leader（默认）</div>
            {roles.map(r => (
              <div
                key={r} onClick={() => { setTargetRole(r); setShowRoles(false); }}
                style={{ padding: '6px 12px', cursor: 'pointer', color: targetRole === r ? '#4CAF50' : '#aaa', fontSize: 12 }}
              >{r}</div>
            ))}
          </div>
        )}
      </div>
      <input
        ref={inputRef}
        value={msg}
        onChange={e => setMsg(e.target.value)}
        onKeyDown={e => e.key === 'Enter' && handleSend()}
        placeholder={targetRole ? `@${targetRole} 输入消息…` : '输入消息…'}
        style={{
          flex: 1, background: '#1a1a1a', border: '1px solid #333', borderRadius: 8,
          color: '#e0e0e0', fontSize: 13, padding: '6px 12px', outline: 'none',
        }}
      />
      <button
        onClick={handleSend} disabled={!msg.trim()}
        style={{
          background: msg.trim() ? '#4CAF50' : '#333', border: 'none', borderRadius: 8,
          color: '#fff', fontSize: 12, padding: '6px 14px', cursor: msg.trim() ? 'pointer' : 'default',
          opacity: msg.trim() ? 1 : 0.4,
        }}
      >发送</button>
    </div>
  );
}

function TaskProgressPanel() {
  const { subtasks, confirmations, selMasterTaskId } = useStore();
  const sts = subtasks || [];

  const batches = new Map<string, Subtask[]>();
  const leaderTasks: Subtask[] = [];
  for (const st of sts) {
    if (st.id === '__leader__') { leaderTasks.push(st); continue; }
    const bid = st.batch_id || 'default';
    if (!batches.has(bid)) batches.set(bid, []);
    batches.get(bid)!.push(st);
  }

  const stateIcon = (state: string) => {
    switch (state) {
      case 'done': return <span style={{ color: '#4CAF50' }}>✅</span>;
      case 'producing': case 'assigned': return <span style={{ color: '#d29922' }}>🔄</span>;
      case 'verifying': return <span style={{ color: '#2196F3' }}>🔍</span>;
      case 'pending_confirmation': return <span style={{ color: '#FF9800' }}>⚠️</span>;
      case 'suspended': return <span style={{ color: '#888' }}>⏸</span>;
      case 'failed': return <span style={{ color: '#f44336' }}>❌</span>;
      default: return <span style={{ color: '#666' }}>⏳</span>;
    }
  };

  const batchLabel = (bid: string, idx: number) => {
    if (bid === 'default') return `阶段${idx + 1}`;
    const match = bid.match(/batch[_-]?(\d+)/);
    return match ? `阶段${parseInt(match[1])}` : `阶段${idx + 1}`;
  };

  return (
    <div style={{ height: '100%', overflow: 'auto', padding: '8px 0' }}>
      {leaderTasks.map(st => (
        <div key={st.id} style={{ padding: '4px 12px', marginBottom: 4 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            {stateIcon(st.state)}
            <span style={{ color: '#ccc', fontSize: 12, fontWeight: 600 }}>📋 任务规划</span>
            <span style={{ color: '#4CAF50', fontSize: 11, marginLeft: 'auto' }}>✅</span>
          </div>
        </div>
      ))}
      {[...batches.entries()].map(([bid, tasks], idx) => {
        const done = tasks.filter(t => t.state === 'done' || t.state === 'failed').length;
        return (
          <div key={bid} style={{ marginBottom: 4 }}>
            <div style={{
              padding: '4px 12px', display: 'flex', alignItems: 'center', gap: 6,
              color: '#999', fontSize: 11, borderTop: idx === 0 ? 'none' : '1px solid #222',
              marginTop: idx === 0 ? 0 : 4, paddingTop: idx === 0 ? 4 : 8,
            }}>
              <span>{batchLabel(bid, idx)}</span>
              <span style={{ marginLeft: 'auto' }}>{done}/{tasks.length}</span>
            </div>
            {tasks.map(t => (
              <div
                key={t.id}
                style={{
                  padding: '3px 12px 3px 24px', display: 'flex', alignItems: 'center', gap: 6,
                  cursor: 'pointer', fontSize: 12,
                }}
                onClick={() => useStore.getState().selectSubtask(t.id)}
                onMouseEnter={e => (e.currentTarget.style.background = 'rgba(255,255,255,0.03)')}
                onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
              >
                {stateIcon(t.state)}
                <span style={{ color: '#ccc', flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{t.title}</span>
                <span style={{ color: '#666', fontSize: 10 }}>{t.role}</span>
              </div>
            ))}
          </div>
        );
      })}
    </div>
  );
}

export default function TaskControlPanel() {
  const { selMasterTaskId, teamChatMessages, confirmations, subtasks } = useStore();
  const chatRef = useRef<HTMLDivElement>(null);
  const [rightView, setRightView] = useState<'list' | 'dag'>('dag');
  const messages = teamChatMessages || [];
  const confs = confirmations || [];

  const roles = [...new Set((subtasks || []).filter(s => s.id !== '__leader__').map(s => s.role))];

  useEffect(() => {
    if (selMasterTaskId) {
      useStore.getState().loadTeamChat();
      useStore.getState().loadConfirmations();
    }
  }, [selMasterTaskId]);

  useEffect(() => {
    if (chatRef.current) {
      chatRef.current.scrollTop = chatRef.current.scrollHeight;
    }
  }, [messages, confs]);

  if (!selMasterTaskId) {
    return <div className="empty-state">选择对话或点击"新对话"开始</div>;
  }

  return (
    <div style={{ display: 'flex', height: '100%' }}>
      {/* 左侧：对话面板 (70%) */}
      <div style={{ flex: 7, display: 'flex', flexDirection: 'column', borderRight: '1px solid #222' }}>
        <div ref={chatRef} style={{ flex: 1, overflow: 'auto', padding: '12px 0' }}>
          {messages.length === 0 && confs.length === 0 && (
            <div style={{ color: '#666', textAlign: 'center', padding: 40 }}>暂无对话，在下方输入消息开始</div>
          )}
          {messages.map((m, i) => <ChatBubble key={i} msg={m} roles={roles} />)}
          {confs.map(c => <ConfirmationCard key={c.task_id} conf={c} />)}
        </div>
        <ChatInput masterTaskId={selMasterTaskId} roles={roles} />
      </div>

      {/* 右侧：任务进度面板 (30%) */}
      <div style={{ flex: 3, display: 'flex', flexDirection: 'column', background: '#141414' }}>
        <div style={{
          display: 'flex', alignItems: 'center', gap: 0,
          borderBottom: '1px solid #222', flexShrink: 0,
        }}>
          <span
            onClick={() => setRightView('dag')}
            style={{
              padding: '6px 12px', cursor: 'pointer', fontSize: 11,
              color: rightView === 'dag' ? '#4CAF50' : '#666',
              borderBottom: rightView === 'dag' ? '2px solid #4CAF50' : '2px solid transparent',
              transition: 'all 0.12s',
            }}
          >DAG</span>
          <span
            onClick={() => setRightView('list')}
            style={{
              padding: '6px 12px', cursor: 'pointer', fontSize: 11,
              color: rightView === 'list' ? '#4CAF50' : '#666',
              borderBottom: rightView === 'list' ? '2px solid #4CAF50' : '2px solid transparent',
              transition: 'all 0.12s',
            }}
          >列表</span>
        </div>
        <div style={{ flex: 1, overflow: 'hidden' }}>
          {rightView === 'dag' ? <DagView /> : <TaskProgressPanel />}
        </div>
      </div>
    </div>
  );
}