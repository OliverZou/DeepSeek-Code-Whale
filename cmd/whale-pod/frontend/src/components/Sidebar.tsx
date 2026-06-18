import FunctionList from './FunctionList';
import TaskTree from './TaskTree';
import TeamList from './TeamList';

export default function Sidebar() {
  return (
    <div id="sidebar">
      <h2>🐳 Whale Pod</h2>
      <FunctionList />
      <TaskTree />
      <TeamList />
    </div>
  );
}
