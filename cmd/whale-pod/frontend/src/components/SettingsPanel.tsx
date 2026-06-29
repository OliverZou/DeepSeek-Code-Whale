import { useState, useEffect } from 'react';
import { api } from '../wails';
import type { SettingsData, MCPServerInfo } from '../types';

interface SettingsPanelProps {
  visible: boolean;
  onClose: () => void;
  embedded?: boolean;
}

export default function SettingsPanel({ visible, onClose, embedded }: SettingsPanelProps) {
  const [settings, setSettings] = useState<SettingsData>({
    apiKey: '',
    model: 'deepseek-chat',
    temperature: 0.7,
    maxTokens: 4096,
    theme: 'dark',
  });
  const [showKey, setShowKey] = useState(false);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState('');
  const [saveMsg, setSaveMsg] = useState('');
  const [mcpServers, setMcpServers] = useState<MCPServerInfo[]>([]);
  const [mcpToggling, setMcpToggling] = useState<string | null>(null);

  useEffect(() => {
    if (visible) {
      api.getSettings().then((s: SettingsData) => {
        if (s) setSettings(s);
      });
      setSaveMsg('');
      setTestResult('');
      api.listMCPServers().then((servers: MCPServerInfo[]) => {
        setMcpServers(servers || []);
      });
    }
  }, [visible]);

  const handleSave = async () => {
    setSaving(true);
    setSaveMsg('');
    const err = await api.saveSettings(settings);
    if (err) {
      setSaveMsg('❌ ' + err);
    } else {
      setSaveMsg('✅ 设置已保存');
      if (embedded) onClose();
      setTimeout(() => setSaveMsg(''), 2000);
    }
    setSaving(false);
  };

  const handleTest = async () => {
    setTesting(true);
    setTestResult('');
    const err = await api.testConnection(settings.apiKey);
    setTestResult(err ? '❌ ' + err : '✅ 连接成功！');
    setTesting(false);
  };

  if (!visible) return null;

  return (
    <>
      {!embedded && (
      <div
        onClick={onClose}
        style={{
          position: 'fixed', inset: 0, zIndex: 90,
          background: 'rgba(0,0,0,0.5)',
        }}
      />
      )}
      <div style={embedded ? {
        padding: 24, overflowY: 'auto', height: '100%', background: '#141414',
      } : {
        position: 'fixed', top: 0, right: 0, bottom: 0, width: 380,
        zIndex: 100, background: '#1a1917',
        borderLeft: '1px solid #333',
        display: 'flex', flexDirection: 'column',
        boxShadow: '-4px 0 20px rgba(0,0,0,0.5)',
      }}>
        {/* Header */}
        <div className="panel-titlebar" style={{ padding: '0 16px', justifyContent: 'space-between' }}>
          <span>⚙️ 设置</span>
          <button
            onClick={onClose}
            style={{
              width: 28, height: 28, borderRadius: 6, border: 'none',
              background: 'transparent', color: '#888', fontSize: 18,
              cursor: 'pointer', display: 'flex', alignItems: 'center',
              justifyContent: 'center',
            }}
          >✕</button>
        </div>

        <div style={{ flex: 1, overflowY: 'auto', padding: 16 }}>
          {/* API Key */}
          <div style={{ marginBottom: 20 }}>
            <label style={labelStyle}>DeepSeek API Key</label>
            <div style={{ display: 'flex', gap: 6 }}>
              <div style={{ flex: 1, position: 'relative' }}>
                <input
                  type={showKey ? 'text' : 'password'}
                  value={settings.apiKey}
                  onChange={e => setSettings(s => ({ ...s, apiKey: e.target.value }))}
                  placeholder="sk-..."
                  style={inputStyle}
                />
                <button
                  onClick={() => setShowKey(!showKey)}
                  style={{
                    position: 'absolute', right: 8, top: '50%', transform: 'translateY(-50%)',
                    background: 'none', border: 'none', color: '#888', cursor: 'pointer',
                    fontSize: 12, padding: '2px 4px',
                  }}
                >
                  {showKey ? '隐藏' : '显示'}
                </button>
              </div>
              <button
                onClick={handleTest}
                disabled={testing || !settings.apiKey.trim()}
                style={{
                  ...btnStyle,
                  background: testing ? '#333' : 'rgba(88,166,255,0.15)',
                  color: testing ? '#888' : '#58a6ff',
                  borderColor: testing ? '#333' : 'rgba(88,166,255,0.3)',
                }}
              >
                {testing ? '...' : '测试'}
              </button>
            </div>
            {testResult && (
              <div style={{
                marginTop: 6, fontSize: 12,
                color: testResult.startsWith('✅') ? '#4CAF50' : '#f85149',
              }}>
                {testResult}
              </div>
            )}
            <div style={{ fontSize: 11, color: '#555', marginTop: 4 }}>
              密钥保存在 ~/.whale/credentials.json，仅本地使用
            </div>
          </div>

          {/* Model */}
          <div style={{ marginBottom: 20 }}>
            <label style={labelStyle}>模型</label>
            <select
              value={settings.model}
              onChange={e => setSettings(s => ({ ...s, model: e.target.value }))}
              style={{ ...inputStyle, cursor: 'pointer' }}
            >
              <option value="deepseek-chat">deepseek-chat（快速，适合日常对话）</option>
              <option value="deepseek-reasoner">deepseek-reasoner（深度推理，更慢但更准）</option>
            </select>
          </div>

          {/* Temperature */}
          <div style={{ marginBottom: 20 }}>
            <label style={labelStyle}>
              Temperature: <span style={{ color: '#58a6ff', fontWeight: 600 }}>{settings.temperature.toFixed(1)}</span>
            </label>
            <input
              type="range"
              min="0"
              max="2"
              step="0.1"
              value={settings.temperature}
              onChange={e => setSettings(s => ({ ...s, temperature: parseFloat(e.target.value) }))}
              style={{
                width: '100%', marginTop: 4,
                accentColor: '#58a6ff',
              }}
            />
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 10, color: '#555' }}>
              <span>0 — 精确</span>
              <span>1 — 平衡</span>
              <span>2 — 创意</span>
            </div>
          </div>

          {/* Max Tokens */}
          <div style={{ marginBottom: 20 }}>
            <label style={labelStyle}>Max Tokens</label>
            <input
              type="number"
              min={100}
              max={32000}
              step={100}
              value={settings.maxTokens}
              onChange={e => setSettings(s => ({ ...s, maxTokens: parseInt(e.target.value) || 4096 }))}
              style={inputStyle}
            />
          </div>

          {/* Theme */}
          <div style={{ marginBottom: 20 }}>
            <label style={labelStyle}>主题</label>
            <div style={{ display: 'flex', gap: 8 }}>
              {([
                ['dark', '🌙 暗色'],
                ['light', '☀️ 亮色'],
                ['system', '💻 跟随系统'],
              ] as [string, string][]).map(([val, label]) => (
                <button
                  key={val}
                  onClick={() => setSettings(s => ({ ...s, theme: val }))}
                  style={{
                    flex: 1, padding: '8px 12px', borderRadius: 8,
                    border: settings.theme === val ? '1px solid #58a6ff' : '1px solid #333',
                    background: settings.theme === val ? 'rgba(88,166,255,0.1)' : 'transparent',
                    color: settings.theme === val ? '#58a6ff' : '#888',
                    fontSize: 13, cursor: 'pointer',
                    transition: 'all 0.15s',
                  }}
                >
                  {label}
                </button>
              ))}
            </div>
          </div>

          {/* MCP Servers */}
          <div style={{ marginBottom: 20 }}>
            <label style={labelStyle}>MCP 服务器</label>
            <div style={{ maxHeight: 300, overflowY: 'auto', borderRadius: 8, border: '1px solid #333' }}>
              {mcpServers.length === 0 && (
                <div style={{ padding: 16, color: '#666', fontSize: 12, textAlign: 'center', lineHeight: 1.8 }}>
                  未找到 MCP 服务器配置<br />
                  <span style={{ color: '#888' }}>配置文件：bin\.whale\mcp.json</span>
                </div>
              )}
              {mcpServers.map(srv => (
                <div
                  key={srv.name}
                  style={{
                    display: 'flex', alignItems: 'center', gap: 10,
                    padding: '8px 12px',
                    borderBottom: '1px solid #2a2a2a',
                  }}
                >
                  <button
                    onClick={async () => {
                      setMcpToggling(srv.name);
                      const err = await api.setMCPServerEnabled(srv.name, srv.disabled);
                      if (err) { alert(err); }
                      const servers = await api.listMCPServers();
                      setMcpServers(servers || []);
                      setMcpToggling(null);
                    }}
                    disabled={mcpToggling === srv.name}
                    style={{
                      width: 36, height: 20, borderRadius: 10,
                      border: 'none', cursor: 'pointer', flexShrink: 0,
                      background: srv.disabled ? '#333' : '#4CAF50',
                      position: 'relative', transition: 'background 0.2s',
                    }}
                  >
                    <div style={{
                      width: 16, height: 16, borderRadius: '50%', background: '#fff',
                      position: 'absolute', top: 2,
                      left: srv.disabled ? 2 : 18,
                      transition: 'left 0.2s',
                    }} />
                  </button>
                  <div style={{ flex: 1, overflow: 'hidden' }}>
                    <div style={{ fontSize: 12, color: '#ddd', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {srv.name}
                    </div>
                    <div style={{ fontSize: 10, color: '#666', display: 'flex', gap: 6, alignItems: 'center' }}>
                      <span style={{
                        color: srv.status === 'connected' ? '#4CAF50' : srv.status === 'failed' ? '#f85149' : '#888',
                      }}>
                        {srv.status === 'connected' ? '●' : srv.status === 'failed' ? '✕' : srv.status === 'starting' ? '◐' : '○'}
                      </span>
                      {srv.url && <span style={{ color: '#555' }}>{srv.url.slice(0, 40)}</span>}
                      {srv.command && <span style={{ color: '#555' }}>{srv.command}</span>}
                      {srv.tools > 0 && <span style={{ color: '#4CAF50' }}>{srv.tools} 工具</span>}
                    </div>
                    {srv.error && (
                      <div style={{ fontSize: 10, color: '#f85149', marginTop: 2, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                        {srv.error.slice(0, 80)}
                      </div>
                    )}
                  </div>
                </div>
              ))}
            </div>
            <div style={{ fontSize: 11, color: '#555', marginTop: 4 }}>
              配置文件：~/.whale/mcp.json
            </div>
          </div>
        </div>

        {/* Footer */}
        <div style={{
          padding: '12px 16px', borderTop: '1px solid #333',
          display: 'flex', alignItems: 'center', gap: 10,
        }}>
          {saveMsg && (
            <span style={{
              fontSize: 12, flex: 1,
              color: saveMsg.startsWith('✅') ? '#4CAF50' : '#f85149',
            }}>
              {saveMsg}
            </span>
          )}
          {!saveMsg && <div style={{ flex: 1 }} />}
          <button
            onClick={onClose}
            style={{ ...btnStyle, color: '#888' }}
          >
            取消
          </button>
          <button
            onClick={handleSave}
            disabled={saving}
            style={{
              ...btnStyle,
              background: saving ? '#333' : '#4CAF50',
              color: '#fff', borderColor: 'transparent',
            }}
          >
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </div>
    </>
  );
}

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: 12,
  fontWeight: 600,
  color: '#aaa',
  marginBottom: 6,
};

const inputStyle: React.CSSProperties = {
  width: '100%',
  padding: '8px 12px',
  borderRadius: 8,
  border: '1px solid #333',
  background: 'rgba(255,255,255,0.05)',
  color: '#e0e0e0',
  fontSize: 13,
  outline: 'none',
};

const btnStyle: React.CSSProperties = {
  padding: '6px 14px',
  borderRadius: 8,
  border: '1px solid #333',
  background: 'transparent',
  fontSize: 13,
  cursor: 'pointer',
  fontWeight: 500,
};
