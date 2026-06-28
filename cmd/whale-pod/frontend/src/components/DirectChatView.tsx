import { useState, useRef, useEffect } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import ChatContent from './ChatContent';

export default function DirectChatView() {
  const { directMessages, directChatTaskId, sendDirectChat, selMasterTaskId, selAgentId, summonedItems, startDirectChat, masterTasks, isStreaming, streamingContent, streamingThinking, abortStreaming, regenerateLast, deleteMessage } = useStore();
  const [input, setInput] = useState('');
  const [focused, setFocused] = useState(false);
  const [sending, setSending] = useState(false);
  const [deepThink, setDeepThink] = useState(false);
  const [streamThinkingExpanded, setStreamThinkingExpanded] = useState(false);
  const [hoveredMsg, setHoveredMsg] = useState<number | null>(null);
  const [quotedMsg, setQuotedMsg] = useState<{ content: string } | null>(null);
  const [chatMode, setChatMode] = useState<'chat' | 'plan' | 'agent'>(() => {
    try { return (localStorage.getItem('whale_chat_mode') as 'chat' | 'plan' | 'agent') || 'chat'; }
    catch { return 'chat'; }
  });

  const updateChatMode = (mode: 'chat' | 'plan' | 'agent') => {
    setChatMode(mode);
    try { localStorage.setItem('whale_chat_mode', mode); } catch { /* ignore */ }
  };
  const [expandedThinking, setExpandedThinking] = useState<Set<number>>(new Set());
  const msgsRef = useRef<HTMLDivElement>(null);
  const taRef = useRef<HTMLTextAreaElement>(null);

  const isNewChat = !selMasterTaskId && !directChatTaskId;

  const agentLabel = (() => {
    if (!selAgentId || selAgentId === 'whale:') return 'Whale';
    const item = summonedItems.find(s => s.type + ':' + s.name === selAgentId);
    return item?.label || selAgentId;
  })();

  const agentType = (() => {
    if (!selAgentId || selAgentId === 'whale:') return 'whale' as const;
    const item = summonedItems.find(s => s.type + ':' + s.name === selAgentId);
    return item?.type || 'expert' as const;
  })();

  const charCount = input.trim().length;

  useEffect(() => {
    if (msgsRef.current) {
      msgsRef.current.scrollTop = msgsRef.current.scrollHeight;
    }
  }, [directMessages, streamingContent, streamingThinking]);

  // Load messages when task changed (with race-condition guard).
  const taskId = directChatTaskId || selMasterTaskId;

  const workspacePath = (() => {
    if (!taskId) return '';
    const mt = masterTasks.find(t => t.id === taskId);
    return mt?.workspace_path || '';
  })();

  useEffect(() => {
    if (!taskId) return;
    let cancelled = false;
    useStore.setState({ directMessages: [] });
    api.getChatMessages(taskId).then(msgs => {
      if (!cancelled) useStore.setState({ directMessages: msgs || [] });
    });
    return () => { cancelled = true; };
  }, [taskId]);

  const handleSend = async () => {
    let msg = input.trim();
    if (!msg || sending) return;
    setInput('');
    setSending(true);

    // Prepend quoted content as markdown blockquote
    if (quotedMsg) {
      const blockquote = quotedMsg.content.split('\n').map(l => `> ${l}`).join('\n');
      msg = blockquote + '\n\n' + msg;
      setQuotedMsg(null);
    }

    if (isNewChat) {
      await startDirectChat(msg, undefined, selAgentId || undefined, deepThink);
    } else {
      await sendDirectChat(msg, deepThink);
    }
    setSending(false);
  };

  const keyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#141414' }}>
      {/* messages */}
      <div ref={msgsRef} style={{ flex: 1, overflowY: 'auto', padding: '16px 10% 8px' }}>
        {directMessages.length === 0 && (
          <div style={{ textAlign: 'center', color: '#666', marginTop: 40 }}>
            {isNewChat ? `向 ${agentLabel} 发起对话` : '开始新对话'}
          </div>
        )}
        {directMessages.map((m, i) => (
          <div
            key={i}
            onMouseEnter={() => setHoveredMsg(i)}
            onMouseLeave={() => { if (hoveredMsg === i) setHoveredMsg(null); }}
            style={{
              position: 'relative',
              display: 'flex', flexDirection: 'column',
              alignItems: m.from === 'human' ? 'flex-end' : 'flex-start',
              marginBottom: 16,
            }}
          >
            {m.from !== 'human' && (
              <div style={{ fontSize: 13, color: '#888', marginBottom: 4, marginLeft: 4 }}>
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, width: '100%' }}>
                  {agentType === 'whale' ? (
                    <img src="/whale.png" width="24" height="24" alt="" style={{ borderRadius: 4 }} />
                  ) : (
                    <div style={{
                      width: 24, height: 24, borderRadius: '50%', flexShrink: 0,
                      background: agentType === 'team' ? 'linear-gradient(135deg, #9C27B0, #2196F3)' : 'linear-gradient(135deg, #4CAF50, #00BCD4)',
                      display: 'flex', alignItems: 'center', justifyContent: 'center',
                      color: '#fff', fontSize: 11, fontWeight: 700,
                    }}>{agentLabel[0]}</div>
                  )}
                  {agentLabel}
                  {m.durationMs != null && (
                    <span
                      onClick={() => {
                        if (!m.thinking) return;
                        setExpandedThinking(prev => {
                          const next = new Set(prev);
                          next.has(i) ? next.delete(i) : next.add(i);
                          return next;
                        });
                      }}
                      style={{
                        display: 'inline-flex', alignItems: 'center', gap: 3,
                        cursor: m.thinking ? 'pointer' : 'default',
                        color: '#555', fontSize: 11, fontWeight: 400, marginLeft: 4,
                      }}
                    >
                       <span style={{ display: 'inline-flex', alignItems: 'center', gap: 1 }}>
                        思考 {(m.durationMs / 1000).toFixed(1)}s
                      </span>
                      {m.thinking && (
                        <svg width="8" height="5" viewBox="0 0 8 5" style={{
                          opacity: 0.5, transition: 'transform 0.15s',
                          transform: expandedThinking.has(i) ? 'rotate(0deg)' : 'rotate(-90deg)',
                        }}>
                          <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
                        </svg>
                      )}
                    </span>
                  )}
                  {/* Hover actions — copy + quote on every AI message, regenerate/delete on last */}
                  {hoveredMsg === i && !isStreaming && (
                    <span style={{ display: 'inline-flex', gap: 2, marginLeft: 4 }}>
                      <button
                        onClick={(e) => { e.stopPropagation(); navigator.clipboard.writeText(m.content); }}
                        title="复制"
                        style={{
                          padding: '1px 5px', border: 'none', background: 'transparent',
                          color: '#999', fontSize: 10, cursor: 'pointer',
                        }}
                      ><svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ marginRight: 2, flexShrink: 0 }}><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg> 复制</button>
                      <button
                        onClick={(e) => {
                          e.stopPropagation();
                          setQuotedMsg({ content: m.content });
                        }}
                        title="引用到输入框"
                        style={{
                          padding: '1px 5px', border: 'none', background: 'transparent',
                          color: '#999', fontSize: 10, cursor: 'pointer',
                        }}
                      ><svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ marginRight: 2, flexShrink: 0 }}><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg> 引用</button>
                      {i === directMessages.length - 1 && (
                        <>
                          <button
                            onClick={(e) => { e.stopPropagation(); regenerateLast(deepThink); }}
                            title="重新生成"
                            style={{
                              padding: '1px 5px', border: 'none', background: 'transparent',
                              color: '#999', fontSize: 10, cursor: 'pointer',
                            }}
                          ><svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ marginRight: 2, flexShrink: 0 }}><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg> 重新生成</button>
                          <button
                            onClick={(e) => { e.stopPropagation(); deleteMessage(i); }}
                            title="删除"
                            style={{
                              padding: '1px 5px', border: 'none', background: 'transparent',
                              color: '#999', fontSize: 10, cursor: 'pointer',
                            }}
                          ><svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ marginRight: 2, flexShrink: 0 }}><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg> 删除</button>
                        </>
                      )}
                    </span>
                  )}
                </span>
              </div>
            )}
            {/* Hover actions for user messages — copy only */}
            {m.from === 'human' && hoveredMsg === i && !isStreaming && (
              <div style={{ position: 'absolute', top: -22, right: 0 }}>
                <button
                  onClick={(e) => { e.stopPropagation(); navigator.clipboard.writeText(m.content); }}
                  title="复制"
                  style={{
                    padding: '1px 5px', border: 'none', background: 'transparent',
                    color: '#999', fontSize: 10, cursor: 'pointer',
                  }}
                ><svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ marginRight: 2, flexShrink: 0 }}><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg> 复制</button>
              </div>
            )}
            {/* thinking content */}
            {m.thinking && expandedThinking.has(i) && (
              <div style={{
                marginLeft: 34, marginBottom: 4, padding: '8px 12px',
                background: 'rgba(0,0,0,0.25)', borderRadius: 8,
                fontSize: 12, color: '#777', lineHeight: 1.5,
                maxWidth: '80%',
              }}>
                {m.thinking}
              </div>
            )}
            <div
              className={`chat-md ${m.from}`}
              style={{
                maxWidth: '80%', padding: '10px 16px', borderRadius: 14,
                fontSize: 14, lineHeight: 1.6,
                wordBreak: 'break-word',
                background: m.from === 'human' ? 'rgba(76,175,80,0.15)' : '#1e1e1e',
                color: m.from === 'human' ? '#fff' : '#e0e0e0',
                border: m.from === 'human' ? '1px solid rgba(76,175,80,0.25)' : '1px solid #333',
                borderBottomRightRadius: m.from === 'human' ? 4 : 14,
                borderBottomLeftRadius: m.from === 'human' ? 14 : 4,
              }}
            >
              <ChatContent content={m.content} />
            </div>
          </div>
        ))}
        {sending && !isStreaming && (
          <div style={{ color: '#666', fontSize: 12, padding: 8 }}>AI 思考中…</div>
        )}
        {/* Streaming bubble */}
        {isStreaming && (
          <div style={{
            display: 'flex', flexDirection: 'column',
            alignItems: 'flex-start',
            marginBottom: 16,
          }}>
            <div style={{ fontSize: 13, color: '#888', marginBottom: 4, marginLeft: 4 }}>
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                {agentType === 'whale' ? (
                  <img src="/whale.png" width="24" height="24" alt="" style={{ borderRadius: 4 }} />
                ) : (
                  <div style={{
                    width: 24, height: 24, borderRadius: '50%', flexShrink: 0,
                    background: agentType === 'team' ? 'linear-gradient(135deg, #9C27B0, #2196F3)' : 'linear-gradient(135deg, #4CAF50, #00BCD4)',
                    display: 'flex', alignItems: 'center', justifyContent: 'center',
                    color: '#fff', fontSize: 11, fontWeight: 700,
                  }}>{agentLabel[0]}</div>
                )}
                {agentLabel}
                <span style={{ color: '#555', fontSize: 11, fontWeight: 400 }}>
                  <span className="typing-dots">{streamingContent ? '输出中' : '思考中'}</span>
                </span>
              </span>
            </div>
            {/* streaming thinking */}
            {streamingThinking && (
              <div style={{ marginLeft: 34, marginBottom: 4, width: '80%' }}>
                <div
                  onClick={() => setStreamThinkingExpanded(!streamThinkingExpanded)}
                  style={{
                    display: 'inline-flex', alignItems: 'center', gap: 4,
                    cursor: 'pointer', color: '#555', fontSize: 11,
                    padding: '4px 8px', borderRadius: 6,
                    background: 'rgba(0,0,0,0.2)',
                  }}
                >
                  <span>🧠 思考中…</span>
                  <svg width="8" height="5" viewBox="0 0 8 5" style={{
                    transition: 'transform 0.15s',
                    transform: streamThinkingExpanded ? 'rotate(0deg)' : 'rotate(-90deg)',
                  }}>
                    <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
                  </svg>
                </div>
                {streamThinkingExpanded && (
                  <div style={{
                    marginTop: 4, padding: '8px 12px',
                    background: 'rgba(0,0,0,0.25)', borderRadius: 8,
                    fontSize: 12, color: '#777', lineHeight: 1.5,
                  }}>
                    {streamingThinking}
                  </div>
                )}
              </div>
            )}
            {/* streaming content */}
            {streamingContent && (
              <div
                className="chat-md agent"
                style={{
                  maxWidth: '80%', padding: '10px 16px', borderRadius: 14,
                  fontSize: 14, lineHeight: 1.6,
                  wordBreak: 'break-word',
                  background: '#1e1e1e', color: '#e0e0e0',
                  border: '1px solid #333',
                  borderBottomLeftRadius: 4,
                }}
              >
                <ChatContent content={streamingContent} />
              </div>
            )}

          </div>
        )}
        {/* action confirmation */}
        {directMessages.length > 0 && (() => {
          const last = directMessages[directMessages.length - 1];
          if (last.from === 'agent' && last.needsAction) {
            // Only show if current mode is lower than needed
            const needed = last.actionType || 'agent';
            const modeRank: Record<string, number> = { chat: 0, plan: 1, agent: 2 };
            if (modeRank[chatMode] >= modeRank[needed]) return null;
            const label = needed === 'agent' ? '需要我执行这些操作吗？' : '需要我帮你规划一下吗？';
            const targetMode = needed === 'agent' ? 'agent' as const : 'plan' as const;
            return (
              <div style={{
                display: 'flex', justifyContent: 'flex-start', padding: '0 0 12px',
              }}>
                <button
                  onClick={() => updateChatMode(targetMode)}
                  style={{
                    marginLeft: 34, padding: '8px 18px', borderRadius: 10,
                    border: '1px solid #4CAF50', background: 'rgba(76,175,80,0.12)',
                    color: '#4CAF50', fontSize: 13, fontWeight: 600, cursor: 'pointer',
                  }}
                >{label}</button>
              </div>
            );
          }
          return null;
        })()}
      </div>

      {/* input */}
      <div style={{ paddingTop: 0, paddingBottom: 8, paddingLeft: '10%', paddingRight: '10%' }}>
        <div style={{
          position: 'relative',
          border: `1px solid ${focused ? '#4CAF50' : '#333'}`,
          borderRadius: 12, background: '#1e1e1e',
          transition: 'border-color 0.2s',
          boxShadow: focused ? '0 0 0 2px rgba(76,175,80,0.12)' : 'none',
        }}>
          {/* quoted message preview */}
          {quotedMsg && (
            <div style={{
              margin: '8px 10px 0 10px', padding: '6px 10px',
              borderLeft: '3px solid #4CAF50', borderRadius: 4,
              background: 'rgba(76,175,80,0.05)',
              display: 'flex', alignItems: 'flex-start', gap: 8,
            }}>
              <span style={{
                flex: 1, fontSize: 12, color: '#999', lineHeight: 1.5,
                overflow: 'hidden', display: '-webkit-box',
                WebkitLineClamp: 3, WebkitBoxOrient: 'vertical',
                wordBreak: 'break-word',
              }}>{quotedMsg.content}</span>
              <button
                onClick={() => setQuotedMsg(null)}
                style={{
                  flexShrink: 0, width: 18, height: 18, borderRadius: 4,
                  border: 'none', background: 'transparent',
                  color: '#666', fontSize: 12, cursor: 'pointer',
                  display: 'flex', alignItems: 'center', justifyContent: 'center',
                }}
              >✕</button>
            </div>
          )}
          <textarea
            ref={taRef}
            value={input}
            onChange={e => setInput(e.target.value)}
            onFocus={() => setFocused(true)}
            onBlur={() => setFocused(false)}
            onKeyDown={keyDown}
            placeholder={isNewChat ? `输入问题或任务，Enter 开始和 ${agentLabel} 对话…` : '输入消息… Enter 发送'}
            style={{
              width: '100%', minHeight: 136, maxHeight: 280,
              background: 'transparent', border: 'none', outline: 'none',
              color: '#e0e0e0', fontSize: 14, lineHeight: 1.6,
              padding: '12px 46px 12px 14px', resize: 'none',
              fontFamily: 'inherit'
            }}
          />

          {/* bottom toolbar: 专家 | ... | 深度思考 模式切换 [Enter] [send] */}
          <div style={{
            position: 'absolute', bottom: 6, left: 10, right: 10,
            display: 'flex', alignItems: 'center', gap: 4,
          }}>
            {/* agent label — left */}
            <span style={{ fontSize: 12, color: '#888', whiteSpace: 'nowrap' }}>{isNewChat ? `新对话 · ${agentLabel}` : `和 ${agentLabel} 对话`}</span>

            {/* spacer */}
            <div style={{ flex: 1 }} />

            {/* deep think toggle */}
            <div
              onClick={() => setDeepThink(!deepThink)}
              title="开启后使用 deepseek-reasoner 模型，回复更深入但更慢"
              style={{
                display: 'flex', alignItems: 'center', gap: 4,
                cursor: 'pointer', opacity: deepThink ? 1 : 0.35,
                transition: 'opacity 0.15s', marginRight: 6,
              }}
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke={deepThink ? '#4CAF50' : '#888'} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M15 14c.2-1 .7-1.7 1.5-2.5 1-.9 1.5-2.2 1.5-3.5A6 6 0 0 0 6 8c0 1 .2 2.2 1.5 3.5.7.7 1.3 1.5 1.5 2.5"/>
                <path d="M9 18h6"/>
                <path d="M10 22h4"/>
              </svg>
              <span style={{ fontSize: 12, color: deepThink ? '#4CAF50' : '#555' }}>深度思考</span>
            </div>

            {/* mode selector */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 2, marginRight: 4 }}>
              {([
                ['chat' as const, 'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z'],
                ['plan' as const, 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M16 13H8 M16 17H8 M10 9H8'],
                ['agent' as const, 'M13 2L3 14h9l-1 8 10-12h-9l1-8z'],
              ]).map(([m, d]) => (
                <div
                  key={m}
                  onClick={() => updateChatMode(m as 'chat' | 'plan' | 'agent')}
                  title={m === 'chat' ? '纯聊天' : m === 'plan' ? '规划模式' : '操作模式'}
                  style={{
                    padding: '2px 4px', cursor: 'pointer',
                    color: chatMode === m ? '#4CAF50' : '#555',
                    transition: 'all 0.15s',
                  }}
                >
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                    <path d={d}/>
                  </svg>
                </div>
              ))}
            </div>

            {/* Enter hint + send/stop button */}
            {isStreaming ? (
              <>
                <span style={{ fontSize: 10, color: '#e74c3c' }}>生成中</span>
                <button
                  onClick={() => { abortStreaming(); setSending(false); }}
                  style={{
                    width: 28, height: 28, borderRadius: 8, border: 'none',
                    background: '#e74c3c',
                    cursor: 'pointer',
                    display: 'flex', alignItems: 'center', justifyContent: 'center',
                    flexShrink: 0,
                  }}
                  title="停止生成"
                >
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="white">
                    <rect x="3" y="3" width="18" height="18" rx="3" />
                  </svg>
                </button>
              </>
            ) : (
              <>
                <span style={{ fontSize: 10, color: '#555' }}>
                  {charCount > 0 ? `${charCount}字` : 'Enter'}
                </span>
                <button
                  onClick={handleSend}
                  disabled={!charCount || sending}
                  style={{
                    width: 28, height: 28, borderRadius: 8, border: 'none',
                    background: (charCount && !sending) ? '#4CAF50' : '#333',
                    cursor: (charCount && !sending) ? 'pointer' : 'default',
                    display: 'flex', alignItems: 'center', justifyContent: 'center',
                    opacity: (charCount && !sending) ? 1 : 0.4,
                    transition: 'all .2s', flexShrink: 0,
                  }}
                >
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="white" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                    <line x1="12" y1="5" x2="12" y2="19" />
                    <polyline points="5 12 12 5 19 12" />
                  </svg>
                </button>
              </>
            )}
          </div>
        </div>
        {/* workspace path — bottom-left, outside input box, aligned with text */}
        <div style={{ height: workspacePath ? 33 : 0, display: 'flex', alignItems: 'center', gap: 5, paddingLeft: 14, marginTop: workspacePath ? 8 : 0, opacity: workspacePath ? 1 : 0, overflow: 'hidden', transition: 'height 0.2s, margin 0.2s, opacity 0.2s' }}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="#666" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0 }}>
            <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>
          </svg>
          {workspacePath && <span style={{ fontSize: 13, color: '#777', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={workspacePath}>{workspacePath}</span>}
        </div>
      </div>
    </div>
  );
}
