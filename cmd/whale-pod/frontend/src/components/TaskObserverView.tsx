import { useStore } from '../store';
import DialoguePanel from './DialoguePanel';
import ObserverPanel from './ObserverPanel';

export default function TaskObserverView() {
  const { selSubtaskId, activeTab } = useStore();
  const setTab = (t: 'dialogue' | 'observer') => useStore.setState({ activeTab: t });

  const isLeader = selSubtaskId === '__leader__';

  return (
    <div className="observer-view">
      <div className="tab-bar">
        <div className={`tab-btn ${activeTab === 'dialogue' ? 'active' : ''}`}
             onClick={() => setTab('dialogue')}>📝 对话</div>
        {isLeader && (
          <div className={`tab-btn ${activeTab === 'observer' ? 'active' : ''}`}
               onClick={() => setTab('observer')}>📊 观察</div>
        )}
      </div>
      {activeTab === 'dialogue' && <DialoguePanel />}
      {activeTab === 'observer' && isLeader && <ObserverPanel />}
    </div>
  );
}
