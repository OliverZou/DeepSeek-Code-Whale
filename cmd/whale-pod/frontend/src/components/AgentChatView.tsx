import { useState } from 'react';
import { useStore } from '../store';

export default function AgentChatView() {
  const { selSubtaskId, chatMessages, sendFeedback } = useStore();
  const [msg, setMsg] = useState('');

  const handleSend = async () => {
    if (!msg.trim()) return;
    await sendFeedback(msg.trim());
    setMsg('');
  };

  return (
    <div className="chat-view">
      <div className="chat-messages">
        {chatMessages.length === 0 && (
          <div className="empty-state">暂无对话。在下方输入消息与 Agent 沟通。</div>
        )}
        {chatMessages.map((m, i) => (
          <div key={i} className={`chat-bubble ${m.from}`}>
            <div className="chat-from">{m.from === 'human' ? '👤 你' : '🤖 Agent'}</div>
            <div className="chat-content">{m.content}</div>
          </div>
        ))}
      </div>
      <div className="chat-input-bar">
        <input
          value={msg}
          onChange={e => setMsg(e.target.value)}
          onKeyDown={e => e.key === 'Enter' && handleSend()}
          placeholder="输入消息…"
        />
        <button onClick={handleSend}>发送</button>
      </div>
    </div>
  );
}
