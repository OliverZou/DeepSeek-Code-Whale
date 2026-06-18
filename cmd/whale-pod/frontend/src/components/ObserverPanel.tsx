import { useStore } from '../store';
import type { Subtask } from '../types';

export default function ObserverPanel() {
  const { subtasks, runSubtask, cancelSubtask } = useStore();
  const sts = subtasks || [];

  if (sts.length === 0) {
    return <div className="empty-state">暂无子任务</div>;
  }

  const renderTree = (tasks: Subtask[], depth: number) =>
    tasks.map(st => {
      const indent = depth * 16;
      const isRunning = ['running', 'producing', 'verifying', 'assigned'].includes(st.state);
      const isDone = st.state === 'done';
      const stateCls = isDone ? 'done' : isRunning ? 'running' : 'pending';

      return (
        <div key={st.id}>
          <div className="subtask-row" style={{ paddingLeft: indent + 8 }}>
            <span className={`dot ${stateCls}`} />
            <span className="subtask-title">{st.title}</span>
            <span className="subtask-role">{st.role}</span>
            <span className="subtask-pct">{st.progress}%</span>
            {st.id !== '__leader__' && !st.children?.length && (
              <span className="subtask-actions">
                {isRunning
                  ? <button className="btn-sm btn-stop" onClick={() => cancelSubtask(st.id)}>⏹</button>
                  : isDone
                  ? <button className="btn-sm btn-rerun" onClick={() => runSubtask(st.id)}>⟳</button>
                  : <button className="btn-sm btn-run" onClick={() => runSubtask(st.id)}>▶</button>
                }
              </span>
            )}
          </div>
          {st.children && renderTree(st.children, depth + 1)}
        </div>
      );
    });

  return <div className="observer-panel">{renderTree(sts, 0)}</div>;
}
