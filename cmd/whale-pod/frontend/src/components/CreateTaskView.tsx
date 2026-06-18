import { useState, useEffect } from 'react';
import { useStore } from '../store';
import { api } from '../wails';

export default function CreateTaskView() {
  const { workDir, teams, startTask } = useStore();
  const [goal, setGoal] = useState('');
  const [team, setTeam] = useState('');
  const [dir, setDir] = useState(workDir);
  const [loading, setLoading] = useState(false);

  useEffect(() => { setDir(workDir); }, [workDir]);

  const handleSubmit = async () => {
    if (!goal.trim()) return;
    setLoading(true);
    if (dir !== workDir) await api.setWorkDir(dir);
    const err = await startTask(goal.trim(), team);
    if (err) alert(err);
    else setGoal('');
    setLoading(false);
  };

  return (
    <div className="create-task-view">
      <div className="create-row">
        <input
          className="goal-input"
          placeholder="输入任务目标，选择团队后点执行…"
          value={goal}
          onChange={e => setGoal(e.target.value)}
          onKeyDown={e => e.key === 'Enter' && handleSubmit()}
        />
        <select value={team} onChange={e => setTeam(e.target.value)} className="team-select">
          <option value="">默认</option>
          {(teams || []).map(t => <option key={t} value={t}>{t}</option>)}
        </select>
        <button className="btn-primary" onClick={handleSubmit} disabled={loading}>
          {loading ? '⏳' : '▶'}
        </button>
      </div>
      <div className="create-dir">
        <input type="text" value={dir} onChange={e => setDir(e.target.value)} placeholder={workDir} />
      </div>
    </div>
  );
}
