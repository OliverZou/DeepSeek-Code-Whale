import { useState, useMemo } from 'react';
import { useStore } from '../store';
import type { TeamInfo, AgentInfo, SummonedItem } from '../types';

const ALL = '全部';

export default function ExpertPanel() {
  const { teamDetails, agentDetails, summonedItems, summonAndOpen, dismissItem } = useStore();
  const [tab, setTab] = useState<'experts' | 'teams'>('experts');
  const [cat, setCat] = useState(ALL);
  const [search, setSearch] = useState('');

  const agentCategories = [ALL, ...new Set(agentDetails.map((a: AgentInfo) => a.category || '其他').filter(Boolean))];
  const filteredAgents = cat === ALL ? agentDetails : agentDetails.filter((a: AgentInfo) => (a.category || '其他') === cat);

  const teamCategories = [ALL, ...new Set(teamDetails.map((t: TeamInfo) => t.category || '其他').filter(Boolean))];
  const filteredTeams = cat === ALL ? teamDetails : teamDetails.filter((t: TeamInfo) => (t.category || '其他') === cat);

  const categories = tab === 'experts' ? agentCategories : teamCategories;

  const q = search.trim().toLowerCase();
  const searchedAgents = q
    ? filteredAgents.filter((a: AgentInfo) =>
        (a.role || a.name).toLowerCase().includes(q) ||
        a.description?.toLowerCase().includes(q) ||
        a.whenToUse?.toLowerCase().includes(q) ||
        a.skills?.some(s => s.toLowerCase().includes(q)))
    : filteredAgents;
  const searchedTeams = q
    ? filteredTeams.filter((t: TeamInfo) =>
        (t.label || t.name).toLowerCase().includes(q) ||
        t.description?.toLowerCase().includes(q) ||
        t.roles?.some(r => r.toLowerCase().includes(q)))
    : filteredTeams;

  const items = tab === 'experts' ? searchedAgents : searchedTeams;

  return (
    <div style={{ height: '100%', overflow: 'auto', background: '#141414' }}>
      <div style={{ padding: '24px 28px 12px' }}>
        <div style={{ display: 'flex', gap: 16, alignItems: 'baseline' }}>
          <button onClick={() => { setTab('experts'); setCat(ALL); }} style={{
            background: 'none', border: 'none', cursor: 'pointer', padding: 0,
            fontSize: 22, fontWeight: tab === 'experts' ? 700 : 400,
            color: tab === 'experts' ? '#fff' : '#888',
          }}>专家</button>
          <button onClick={() => { setTab('teams'); setCat(ALL); }} style={{
            background: 'none', border: 'none', cursor: 'pointer', padding: 0,
            fontSize: 22, fontWeight: tab === 'teams' ? 700 : 400,
            color: tab === 'teams' ? '#fff' : '#888',
          }}>专家团</button>
          <span style={{ marginLeft: 'auto', fontSize: 12, color: '#555' }}>
            {items.length} 项
          </span>
        </div>
        <div style={{ display: 'flex', gap: 8, marginTop: 14, flexWrap: 'wrap' }}>
          {categories.map((c: string) => (
            <button key={c} onClick={() => setCat(c)} style={{
              padding: '4px 14px', borderRadius: 14, border: 'none', cursor: 'pointer',
              fontSize: 12, whiteSpace: 'nowrap',
              background: c === cat ? 'rgba(76,175,80,0.18)' : 'rgba(255,255,255,0.06)',
              color: c === cat ? '#4CAF50' : '#aaa',
            }}>{c}</button>
          ))}
        </div>
        <div style={{ marginTop: 12 }}>
          <input
            type="text"
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="搜索专家名称、技能、用途…"
            style={{
              width: '100%', boxSizing: 'border-box', padding: '7px 12px',
              background: 'rgba(255,255,255,0.06)', border: '1px solid rgba(255,255,255,0.08)',
              borderRadius: 8, color: '#ccc', fontSize: 12, outline: 'none',
            }}
          />
        </div>
      </div>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 16, padding: '0 28px 24px' }}>
        {tab === 'experts' && searchedAgents.map(a => {
          const summoned = summonedItems.some(s => s.name === a.name && s.type === 'expert');
          return (
            <ExpertCard
              key={a.name}
              agent={a}
              summoned={summoned}
              onSummon={() => summonAndOpen({ type: 'expert', name: a.name, label: a.role || a.name, category: a.category, description: a.description })}
              onDismiss={() => dismissItem(a.name, 'expert')}
            />
          );
        })}
        {tab === 'teams' && searchedTeams.map(t => {
          const summoned = summonedItems.some(s => s.name === t.name && s.type === 'team');
          return (
            <TeamCard
              key={t.name}
              team={t}
              summoned={summoned}
              onSummon={() => summonAndOpen({ type: 'team', name: t.name, label: t.label || t.name, category: t.category, description: t.description })}
              onDismiss={() => dismissItem(t.name, 'team')}
            />
          );
        })}
        {items.length === 0 && (
          <div style={{ color: '#666', padding: 40, textAlign: 'center', width: '100%' }}>
            {tab === 'experts' ? '暂无专家。请在 agents 目录下创建专家定义文件。' : '暂无专家团。请在 teams 目录下创建团队 YAML 配置。'}
          </div>
        )}
      </div>
    </div>
  );
}

function ExpertCard({ agent, summoned, onSummon, onDismiss }: {
  agent: AgentInfo; summoned: boolean; onSummon: () => void; onDismiss: () => void;
}) {
  const [hover, setHover] = useState(false);
  const [expanded, setExpanded] = useState(false);
  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      onClick={() => setExpanded(!expanded)}
      style={{
        width: 260, background: '#1e1e1e', borderRadius: 14,
        border: summoned ? '1px solid rgba(76,175,80,0.35)' : '1px solid #2a2a2a',
        padding: 18, display: 'flex', flexDirection: 'column', gap: 10,
        position: 'relative', cursor: 'pointer',
      }}
    >
      {hover && (
        <button
          onClick={e => { e.stopPropagation(); onSummon(); }}
          style={{
            position: 'absolute', top: 10, right: 10,
            padding: '3px 12px', borderRadius: 6, border: 'none',
            background: 'rgba(76,175,80,0.18)', color: '#4CAF50',
            fontSize: 12, fontWeight: 600, cursor: 'pointer',
          }}
        >召唤</button>
      )}

      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <div style={{
          width: 40, height: 40, borderRadius: '50%',
          background: 'linear-gradient(135deg, #4CAF50, #00BCD4)',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          color: '#fff', fontSize: 18, fontWeight: 700, flexShrink: 0,
        }}>{(agent.role || agent.name)[0]}</div>
        <div>
          <div style={{ fontWeight: 600, color: '#fff', fontSize: 14 }}>{agent.role || agent.name}</div>
          <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
            {agent.role && <span style={{ fontSize: 11, color: '#888' }}>{agent.name}</span>}
            {agent.category && <span style={{ fontSize: 10, color: '#4CAF50', background: 'rgba(76,175,80,0.12)', padding: '1px 8px', borderRadius: 8 }}>{agent.category}</span>}
          </div>
        </div>
      </div>
      {agent.description && (
        <div style={{ fontSize: 12, color: '#999', lineHeight: 1.6 }}>{agent.description}</div>
      )}
      {agent.whenToUse && expanded && (
        <div style={{ fontSize: 11, color: '#7aa2f7', lineHeight: 1.5, padding: '6px 8px', background: 'rgba(122,162,247,0.06)', borderRadius: 6 }}>
          <span style={{ fontWeight: 600 }}>适用场景：</span>{agent.whenToUse}
        </div>
      )}
      {agent.skills && agent.skills.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
          {agent.skills.slice(0, expanded ? undefined : 4).map(s => (
            <span key={s} style={{ padding: '2px 10px', borderRadius: 10, fontSize: 11, background: 'rgba(76,175,80,0.08)', color: '#81C784' }}>{s}</span>
          ))}
          {!expanded && agent.skills.length > 4 && <span style={{ padding: '2px 10px', borderRadius: 10, fontSize: 11, color: '#666' }}>+{agent.skills.length - 4}</span>}
        </div>
      )}
      {agent.tools && agent.tools.length > 0 && expanded && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
          {agent.tools.map(t => (
            <span key={t} style={{ padding: '1px 8px', borderRadius: 6, fontSize: 10, background: 'rgba(255,255,255,0.04)', color: '#666', border: '1px solid #2a2a2a' }}>{t}</span>
          ))}
        </div>
      )}
    </div>
  );
}

function TeamCard({ team, summoned, onSummon, onDismiss }: {
  team: TeamInfo; summoned: boolean; onSummon: () => void; onDismiss: () => void;
}) {
  const [hover, setHover] = useState(false);
  const [expanded, setExpanded] = useState(false);
  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      onClick={() => setExpanded(!expanded)}
      style={{
        width: 260, background: '#1e1e1e', borderRadius: 14,
        border: summoned ? '1px solid rgba(76,175,80,0.35)' : '1px solid #2a2a2a',
        padding: 18, display: 'flex', flexDirection: 'column', gap: 10,
        position: 'relative', cursor: 'pointer',
      }}
    >
      {hover && (
        <button
          onClick={e => { e.stopPropagation(); onSummon(); }}
          style={{
            position: 'absolute', top: 10, right: 10,
            padding: '3px 12px', borderRadius: 6, border: 'none',
            background: 'rgba(76,175,80,0.18)', color: '#4CAF50',
            fontSize: 12, fontWeight: 600, cursor: 'pointer',
          }}
        >召唤</button>
      )}

      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <div style={{
          width: 40, height: 40, borderRadius: '50%',
          background: 'linear-gradient(135deg, #9C27B0, #2196F3)',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          color: '#fff', fontSize: 18, fontWeight: 700, flexShrink: 0,
        }}>{(team.label || team.name)[0]}</div>
        <div>
          <div style={{ fontWeight: 600, color: '#fff', fontSize: 14 }}>{team.label || team.name}</div>
          <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
            {team.category && <span style={{ fontSize: 10, color: '#4CAF50', background: 'rgba(76,175,80,0.12)', padding: '1px 8px', borderRadius: 8 }}>{team.category}</span>}
            <span style={{ fontSize: 11, color: '#666' }}>{team.roles.length} 位专家</span>
          </div>
        </div>
      </div>
      {team.description && (
        <div style={{ fontSize: 12, color: '#999', lineHeight: 1.6 }}>{team.description}</div>
      )}
      {team.roles.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
          {team.roles.slice(0, expanded ? undefined : 5).map(r => (
            <span key={r} style={{ padding: '2px 10px', borderRadius: 10, fontSize: 11, background: 'rgba(255,255,255,0.06)', color: '#aaa' }}>{r}</span>
          ))}
          {!expanded && team.roles.length > 5 && <span style={{ padding: '2px 10px', borderRadius: 10, fontSize: 11, color: '#666' }}>+{team.roles.length - 5}</span>}
        </div>
      )}
    </div>
  );
}
