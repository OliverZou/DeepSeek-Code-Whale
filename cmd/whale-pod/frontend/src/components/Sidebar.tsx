import FunctionList from './FunctionList';
import TaskTree from './TaskTree';
import TeamList from './TeamList';

export default function Sidebar() {
  return (
    <div id="sidebar">
      <div className="panel-titlebar">
        <span className="titlebar-label">🐳 Whale Pod</span>
      </div>
      <FunctionList />
      <TaskTree />
      <TeamList />
    </div>
  );
}
