import { useStore } from '../store';
import { api } from '../wails';
import type { MasterTask } from '../types';

export default function TaskTree() {
  const { masterTasks, workDirs, selMasterTaskId, selectMasterTask, workDir } = useStore();
  const mts = masterTasks || [];

  const grouped = new Map<string, MasterTask[]>();
  for (const t of mts) {
    const dir = t.workspace_path || workDir || '.';
    if (!grouped.has(dir)) grouped.set(dir, []);
    grouped.get(dir)!.push(t);
  }

  return (
    <div className="section">
      <div className="section-title">
        <span>任务列表</span>
        <button className="btn-icon" onClick={() => api.openTerminal()} title="打开文件夹">📂</button>
      </div>
      {[...grouped.entries()].map(([dir, tasks]) => (
        <div key={dir}>
          <div className="tree-dir">📁 {shortPath(dir)}</div>
          {tasks.map(t => {
            const pct = t.task_count > 0 ? Math.round(t.done_count / t.task_count * 100) : 0;
            const done = t.task_count > 0 && t.done_count >= t.task_count;
            const icon = done ? '✅' : t.status === 'running' ? '🔄' : '⏳';
            return (
              <div
                key={t.id}
                className={`tree-item ${selMasterTaskId === t.id ? 'active' : ''}`}
                onClick={() => selectMasterTask(t.id)}
              >
                <span>{icon}</span>
                <span className="tree-label">{shortGoal(t.goal)}</span>
                <span className="tree-meta">{t.done_count}/{t.task_count}</span>
                {pct > 0 && <div className="mini-bar" style={{ width: `${pct}%` }} />}
              </div>
            );
          })}
        </div>
      ))}
      {mts.length === 0 && (
        <div className="empty-hint">暂无任务，点击上方创建</div>
      )}
    </div>
  );
}

function shortPath(p: string) { return p.length > 30 ? '…' + p.slice(-28) : p; }
function shortGoal(g: string) { return g.length > 24 ? g.slice(0, 22) + '…' : g; }
