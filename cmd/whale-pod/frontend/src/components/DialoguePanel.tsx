import { useRef, useEffect } from 'react';
import { useStore } from '../store';

export default function DialoguePanel() {
  const { selSubtaskId, dialogue, leaderPlan } = useStore();
  const ref = useRef<HTMLDivElement>(null);

  const entries = selSubtaskId === '__leader__' ? (leaderPlan || []) : (dialogue || []);

  useEffect(() => {
    if (ref.current) {
      const atBottom = ref.current.scrollTop + ref.current.clientHeight >= ref.current.scrollHeight - 4;
      if (atBottom || entries.length <= 3) ref.current.scrollTop = ref.current.scrollHeight;
    }
  }, [entries]);

  if (entries.length === 0) {
    return <div className="empty-state"><div className="empty-icon">💬</div>暂无对话数据</div>;
  }

  return (
    <div className="dialogue-panel" ref={ref}>
      {entries.map((e, i) => {
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
    </div>
  );
}
