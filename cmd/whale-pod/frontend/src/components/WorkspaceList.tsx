import { useStore } from '../store';
import { api } from '../wails';
import CollapsibleSection from './CollapsibleSection';

export default function WorkspaceList() {
  const { openWorkspaces, masterTasks, removeWorkspace } = useStore();

  const dirs = openWorkspaces;

  const icon = (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0 }}>
      <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>
    </svg>
  );

  const action = (
    <svg
      width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"
      style={{ cursor: 'pointer', opacity: 0.5, flexShrink: 0 }}
      onClick={e => { e.stopPropagation(); api.openTerminal(); }}
    >
      <title>打开工作空间目录</title>
      <path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>
      <path d="M12 10v6"/>
      <path d="M9 13h6"/>
    </svg>
  );

  return (
    <CollapsibleSection title="工作空间" icon={icon} action={action} defaultOpen>
      {dirs.map((d: string) => {
        const count = masterTasks.filter(t => t.workspace_path === d).length;
        const name = d.split('\\').pop() || d;
        return (
          <div key={d} className="tree-item" style={{ fontSize: 12 }}>
            <span className="tree-label">{name}</span>
            {count > 0 && <span className="tree-meta">{count}</span>}
          </div>
        );
      })}
      {dirs.length === 0 && (
        <div className="empty-hint" style={{ padding: '4px 14px' }}>暂无工作空间</div>
      )}
    </CollapsibleSection>
  );
}
