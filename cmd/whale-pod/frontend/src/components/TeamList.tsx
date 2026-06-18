import { useStore } from '../store';

export default function TeamList() {
  const teams = useStore(s => s.teams) || [];

  return (
    <div className="section">
      <div className="section-title">
        <span>Agent 团队</span>
        <span>
          <button className="btn-icon" title="添加团队">＋</button>
          <button className="btn-icon" title="添加 Agent">👤＋</button>
        </span>
      </div>
      {teams.map(team => (
        <div key={team}>
          <div className="tree-dir">🏢 {team}</div>
        </div>
      ))}
    </div>
  );
}
