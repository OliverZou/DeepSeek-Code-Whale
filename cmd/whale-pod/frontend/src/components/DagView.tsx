import { useRef, useEffect, useMemo } from 'react';
import { useStore } from '../store';
import type { Subtask } from '../types';

const NODE_W = 160;
const NODE_H = 44;
const H_GAP = 24;
const V_GAP = 56;
const PAD = 20;

const stateColor: Record<string, string> = {
  done: '#4CAF50',
  producing: '#d29922',
  assigned: '#d29922',
  verifying: '#2196F3',
  pending_confirmation: '#FF9800',
  suspended: '#888',
  failed: '#f44336',
};

const stateLabel: Record<string, string> = {
  done: '✅',
  producing: '🔄',
  assigned: '🔄',
  verifying: '🔍',
  pending_confirmation: '⚠️',
  suspended: '⏸',
  failed: '❌',
  pending: '⏳',
};

interface LayoutNode {
  id: string;
  title: string;
  role: string;
  state: string;
  x: number;
  y: number;
  parentIds: string[];
  batchIdx: number;
}

function layoutDag(subtasks: Subtask[]): { nodes: LayoutNode[]; width: number; height: number } {
  const tasks = subtasks.filter(s => s.id !== '__leader__');
  if (tasks.length === 0) return { nodes: [], width: 0, height: 0 };

  const batches = new Map<string, Subtask[]>();
  for (const t of tasks) {
    const bid = t.batch_id || 'default';
    if (!batches.has(bid)) batches.set(bid, []);
    batches.get(bid)!.push(t);
  }

  const batchOrder = [...batches.keys()];
  const nodes: LayoutNode[] = [];
  let maxY = 0;

  for (let bi = 0; bi < batchOrder.length; bi++) {
    const batch = batches.get(batchOrder[bi])!;
    const y = PAD + bi * (NODE_H + V_GAP);
    const totalW = batch.length * NODE_W + (batch.length - 1) * H_GAP;
    const startX = PAD;

    for (let ni = 0; ni < batch.length; ni++) {
      const t = batch[ni];
      nodes.push({
        id: t.id,
        title: t.title,
        role: t.role,
        state: t.state,
        x: startX + ni * (NODE_W + H_GAP),
        y,
        parentIds: t.parent_ids || [],
        batchIdx: bi,
      });
      maxY = y + NODE_H;
    }
  }

  const maxX = Math.max(...nodes.map(n => n.x + NODE_W), 0);
  return { nodes, width: maxX + PAD, height: maxY + PAD };
}

export default function DagView() {
  const subtasks = useStore(s => s.subtasks);
  const selSubtaskId = useStore(s => s.selSubtaskId);
  const selectSubtask = useStore(s => s.selectSubtask);
  const svgRef = useRef<SVGSVGElement>(null);

  const { nodes, width, height } = useMemo(() => layoutDag(subtasks || []), [subtasks]);

  const nodeMap = useMemo(() => {
    const m = new Map<string, LayoutNode>();
    for (const n of nodes) m.set(n.id, n);
    return m;
  }, [nodes]);

  if (nodes.length === 0) {
    return (
      <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#666', fontSize: 12 }}>
        暂无子任务
      </div>
    );
  }

  const edges: { x1: number; y1: number; x2: number; y2: number }[] = [];
  for (const n of nodes) {
    for (const pid of n.parentIds) {
      const p = nodeMap.get(pid);
      if (p) {
        edges.push({
          x1: p.x + NODE_W / 2,
          y1: p.y + NODE_H,
          x2: n.x + NODE_W / 2,
          y2: n.y,
        });
      }
    }
  }

  return (
    <div style={{ height: '100%', overflow: 'auto', background: '#0d0d0d' }}>
      <svg
        ref={svgRef}
        width={Math.max(width, 200)}
        height={Math.max(height, 100)}
        style={{ display: 'block', minWidth: '100%' }}
      >
        <defs>
          <marker id="arrowhead" markerWidth="8" markerHeight="6" refX="8" refY="3" orient="auto">
            <polygon points="0 0, 8 3, 0 6" fill="#444" />
          </marker>
        </defs>

        {edges.map((e, i) => (
          <line
            key={i}
            x1={e.x1} y1={e.y1} x2={e.x2} y2={e.y2}
            stroke="#333" strokeWidth="1.5" markerEnd="url(#arrowhead)"
          />
        ))}

        {nodes.map(n => {
          const color = stateColor[n.state] || '#555';
          const selected = n.id === selSubtaskId;
          return (
            <g
              key={n.id}
              onClick={() => selectSubtask(n.id)}
              style={{ cursor: 'pointer' }}
            >
              <rect
                x={n.x} y={n.y}
                width={NODE_W} height={NODE_H}
                rx={8} ry={8}
                fill={selected ? 'rgba(76,175,80,0.12)' : '#1a1a1a'}
                stroke={selected ? '#4CAF50' : color}
                strokeWidth={selected ? 2 : 1}
                opacity={n.state === 'done' ? 0.7 : 1}
              />
              <text
                x={n.x + 8} y={n.y + 17}
                fill="#ccc" fontSize={11} fontWeight={600}
                style={{ pointerEvents: 'none' }}
              >
                {stateLabel[n.state] || '⏳'} {n.title.length > 12 ? n.title.slice(0, 11) + '…' : n.title}
              </text>
              <text
                x={n.x + 8} y={n.y + 33}
                fill="#666" fontSize={10}
                style={{ pointerEvents: 'none' }}
              >
                {n.role} · {n.state}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}