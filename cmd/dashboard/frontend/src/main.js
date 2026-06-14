// Whale Dashboard — Wails frontend
// Calls Go backend via window.go.main.App.<Method>()
// Listens for updates via window.runtime.EventsOn("update", ...)

const $ = (s) => document.querySelector(s);

let state = { masterTasks: [], selMtId: null, subtasks: [], selStId: null, activeTab: 'dialogue', unreadTasks: new Set() };

// ---------- Init ----------
function init() {
  if (typeof window.go === 'undefined') return;
  loadMasterTasks();
  window.runtime.EventsOn("update", (payload) => {
    if (payload && payload.length !== undefined) {
      state.masterTasks = payload;
      updateUI();
    } else {
      loadMasterTasks();
    }
  });

  // Listen for real-time task events from engine (代替全量轮询).
  window.runtime.EventsOn("task-event", (event) => {
    onTaskEvent(event);
  });
}

// 处理 engine 推过来的实时任务事件。
function onTaskEvent(event) {
  if (!event) return;

  // EventLeaderLog: leader 写入了新对话，刷新对话视图。
  if (event.type === 4) { // EventLeaderLog = 4
    state.unreadTasks.add('__leader__');
    if (state.selStId === '__leader__') {
      const mt = getSelMt();
      if (mt) loadDialogue(mt.workspace_id, '__leader__');
    }
    renderSubtasks();
    return;
  }

  // EventAgentLog: worker/verifier 写入了新对话，刷新匹配的子任务对话。
  if (event.type === 5) { // EventAgentLog = 5
    if (event.task_id) {
      state.unreadTasks.add(event.task_id);
      if (state.selStId === event.task_id) {
        const mt = getSelMt();
        if (mt) loadDialogue(mt.workspace_id, event.task_id);
      }
    }
    renderSubtasks();
    return;
  }

  if (!event.task_id) return;

  // 刷新 master task 列表（状态/进度变化）
  loadMasterTasks();

  // 如果事件属于当前选中的 master task，刷新子任务列表
  const mt = getSelMt();
  if (mt && state.selMtId) {
    loadSubtasks(mt.workspace_id, mt.id);
  }
}

async function loadMasterTasks() {
  try {
    const newTasks = await window.go.main.App.GetMasterTasks();
    state.masterTasks = newTasks;
    // Recover selMt reference from new data if previously selected.
    if (state.selMtId) {
      const found = newTasks.find(mt => mt.id === state.selMtId);
      if (!found) {
        // The previously selected master task is gone — deselect.
        state.selMtId = null;
        state.subtasks = [];
        state.selStId = null;
      }
    }
    // Auto-select the first master task with subtasks when nothing
    // is selected yet (e.g. planning just finished).
    if (!state.selMtId) {
      const planned = newTasks.find(mt => (mt.task_count || 0) > 0);
      if (planned) {
        state.selMtId = planned.id;
        state._lastMtId = null;
      }
    }
    updateUI();
  } catch (_) {}
}

async function loadSubtasks(wsID, mtID) {
  try {
    const newSubtasks = await window.go.main.App.GetSubtasks(wsID, mtID);
    state.subtasks = newSubtasks;
    // Preserve selected subtask ID across refreshes.
    const prevSelStId = state.selStId;
    if (state.selStId) {
      const found = newSubtasks.find(st => st.id === state.selStId);
      if (!found) state.selStId = null;
    }
    renderSubtasks();
    // Auto-select TeamLeader placeholder if nothing selected and tasks exist.
    if (newSubtasks.length > 0 && !state.selStId) {
      selectSubtask(newSubtasks[0].id);
    }
  } catch (_) {}
}

async function loadDialogue(wsID, taskID) {
  try {
    if (taskID === '__leader__') {
      const dialogue = await window.go.main.App.GetLeaderPlan(wsID);
      renderDialogue(dialogue);
    } else {
      const dialogue = await window.go.main.App.GetAgentDialogue(wsID, taskID);
      renderDialogue(dialogue);
    }
  } catch (_) {}
}

async function loadFlowchart(wsID, mtID) {
  try {
    const svg = await window.go.main.App.GetLeaderFlowchart(wsID, mtID);
    renderFlowchart(svg);
  } catch (_) {}
}

function updateUI() {
  renderSidebar();
  $('#mt-count').textContent = state.masterTasks.length + ' master tasks';
  if (state.selMtId) {
    const mt = getSelMt();
    if (mt) {
      state._lastMtId = mt.id;
      loadSubtasks(mt.workspace_id, mt.id);
    }
  } else {
    state.subtasks = [];
    state.selStId = null;
  }
}



// ---------- Sidebar: Master Tasks ----------
function renderSidebar() {
  const list = $('#mt-list');
  if (!state.masterTasks.length) {
    list.innerHTML = '<div class="empty"><div class="icon">📋</div>No master tasks</div>';
    return;
  }
  let html = '';
  for (const mt of state.masterTasks) {
    const active = state.selMtId === mt.id ? ' active' : '';
    const goalShort = esc(mt.goal).length > 50 ? esc(mt.goal).slice(0, 50) + '…' : esc(mt.goal);
    const pct = mt.task_count > 0 ? Math.round(mt.done_count / mt.task_count * 100) : 0;
    const isPlanning = mt.status === 'running' && (mt.task_count || 0) === 0;
    const hasRunning = !isPlanning && (mt.active_count || 0) > 0;
    html += `<div class="mt${active}" data-id="${mt.id}" data-goal="${esc(mt.goal)}" data-wsid="${esc(mt.workspace_id)}">
      <div class="goal" title="${esc(mt.goal)}">${goalShort}</div>
      <div class="meta">
        <span class="ws-label">📁 ${esc(mt.workspace_label)}</span>
        <span>${mt.done_count}/${mt.task_count}</span>
        <span>${fmtTime(mt.created_at)}</span>
        ${isPlanning ? `<span class="planning-indicator">⏳ 规划中...</span>` : hasRunning ? `<button class="stop-btn" data-wsid="${esc(mt.workspace_id)}" data-mtid="${mt.id}">⏹ 停止</button>` : `<button class="resume-btn" data-wsid="${esc(mt.workspace_id)}" data-mtid="${mt.id}" ${!mt.workspace_online ? 'disabled title="需要 Whale CLI 在该工作区运行"' : ''}>▶ 运行</button>`}
      </div>
      <div class="progress-bar-wrap"><div class="progress-bar-fill" style="width:${pct}%"></div></div>
    </div>`;
  }
  list.innerHTML = html;
  list.querySelectorAll('.mt').forEach(el => {
    el.onclick = (e) => {
      if (!e.target.closest('.stop-btn') && !e.target.closest('.resume-btn')) {
        selectMasterTask(el.dataset.id);
      }
    };
    el.oncontextmenu = (e) => {
      e.preventDefault();
      showMasterTaskContextMenu(e.clientX, e.clientY, el.dataset.id, el.dataset.wsid, el.dataset.goal);
    };
  });
  // Bind stop buttons for master tasks.
  list.querySelectorAll('.stop-btn').forEach(btn => {
    btn.onclick = async (e) => {
      e.stopPropagation();
      const wsid = btn.dataset.wsid;
      const mtid = btn.dataset.mtid;
      btn.textContent = '⏳';
      btn.disabled = true;
      const err = await window.go.main.App.CancelMasterTask(wsid, mtid);
      if (err) {
        btn.textContent = '⚠️';
        console.error('cancel master task:', err);
      } else {
        btn.textContent = '✅';
      }
      setTimeout(() => loadMasterTasks(), 1000);
    };
  });
  // Bind resume buttons for master tasks (suspended → resume).
  list.querySelectorAll('.resume-btn').forEach(btn => {
    btn.onclick = async (e) => {
      e.stopPropagation();
      const wsid = btn.dataset.wsid;
      const mtid = btn.dataset.mtid;
      btn.textContent = '⏳';
      btn.disabled = true;
      const err = await window.go.main.App.ResumeMasterTask(wsid, mtid);
      if (err) {
        btn.textContent = '⚠️';
        console.error('resume master task:', err);
      } else {
        btn.textContent = '✅';
      }
      setTimeout(() => loadMasterTasks(), 1000);
    };
  });
}

function selectMasterTask(id) {
  const mt = state.masterTasks.find(m => m.id === id);
  if (!mt) return;
  state.selMtId = mt.id;
  state.selStId = null;
  state.subtasks = [];
  state._lastMtId = null;
  renderSidebar();
  loadSubtasks(mt.workspace_id, mt.id);
  clearAgentPanel();
}

// ---------- Subtask List ----------
function renderSubtasks() {
  const list = $('#task-tabs');
  if (!state.subtasks.length) {
    list.innerHTML = '<div class="empty"><div class="icon">📋</div>No subtasks</div>';
    return;
  }
  let html = '';
  // Determine workspace ID for stop button calls.
  const wsid = state.selMtId ? (getSelMt() ? getSelMt().workspace_id : '') : '';
  for (const st of state.subtasks) {
    const active = state.selStId === st.id ? ' sel' : '';
    const leader = st.id === '__leader__' ? ' leader' : '';
    const stateDot = st.id === '__leader__' ? 'leader-dot'
      : st.state === 'done' ? 'done'
      : st.state === 'running' || st.state === 'producing' || st.state === 'verifying' ? 'running'
      : st.state === 'suspended' ? 'suspended'
      : st.state === 'failed' ? 'failed' : 'pending';
    const icon = st.id === '__leader__' ? '📋' : '🎭';

	
    html += `<div class="st${active}${leader}" data-id="${st.id}">
      <div class="st-title" title="${esc(st.title)}">
        <span class="state-dot ${stateDot}${state.unreadTasks.has(st.id) ? " pulse" : ""}"></span>
        ${icon} ${esc(st.title)}
	        ${state.unreadTasks.has(st.id) ? '<span class="unread-badge">●</span>' : ''}
      </div>
      <div class="st-meta">
        <span>${esc(st.role)}</span>
        <span>${st.progress}%</span>
        </div>
    </div>`;
  }
  list.innerHTML = html;
  list.querySelectorAll('.st').forEach(el => {
    el.onclick = (e) => selectSubtask(el.dataset.id);
  });
}

function selectSubtask(id) {
  state.selStId = id;
  state.activeTab = 'dialogue';
  renderSubtasks();

  const st = state.subtasks.find(s => s.id === id);
  const mt = getSelMt();
  if (!st || !mt) return;

  const wsID = mt.workspace_id;
  const mtID = mt.id;

  state.unreadTasks.delete(id);
  if (id === '__leader__') {
    // TeamLeader: show tab bar + load plan + flowchart
    $('#tab-bar').style.display = 'flex';
    $('#agent-header').textContent = '📋 任务主管';
    switchTab('dialogue');
    loadDialogue(wsID, '__leader__');
    loadFlowchart(wsID, mtID);
  } else {
    // Regular subtask: hide tab bar, show dialogue only
    $('#tab-bar').style.display = 'none';
    $('#flowchart-view').style.display = 'none';
    $('#dialogue-view').style.display = 'flex';
    $('#agent-header').textContent = `🎭 ${esc(st.role)} — ${esc(st.title)}`;
    loadDialogue(wsID, id);
  }
}

// Helper: get selected master task object.
function getSelMt() {
  return state.masterTasks.find(mt => mt.id === state.selMtId) || null;
}

// ---------- Tab Switching ----------
function switchTab(tab) {
  state.activeTab = tab;
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.tab === tab);
  });
  if (tab === 'dialogue') {
    $('#dialogue-view').style.display = 'flex';
    $('#flowchart-view').style.display = 'none';
  } else {
    $('#dialogue-view').style.display = 'none';
    $('#flowchart-view').style.display = 'flex';
  }
}

// ---------- Dialogue View ----------
function renderDialogue(dialogue) {
  const view = $('#dialogue-view');
  if (!dialogue || dialogue.length === 0) {
    view.innerHTML = '<div class="empty"><div class="icon">💬</div>No conversation data</div>';
    return;
  }
  // Preserve scroll position across refreshes.
  const wasAtBottom = view.scrollTop + view.clientHeight >= view.scrollHeight - 4;
  let html = '';
  for (const msg of dialogue) {
    const role = msg.role || '';
    const roleClass = role.startsWith('worker') ? 'role-worker'
      : role.startsWith('verifier') ? 'role-verifier'
      : role.startsWith('任务主管') ? 'role-planner'
      : 'role-input';
    const roundMatch = role.match(/round\s+(\d+)/);
    const roundBadge = roundMatch ? `<span class="round-badge">Round ${roundMatch[1]}</span>` : '';
    html += `<div class="msg ${roleClass}">
      <div class="role-label">${esc(role)} ${roundBadge}</div>
      ${esc(msg.content)}
    </div>`;
  }
  view.innerHTML = html;
  // Only auto-scroll to bottom if the user was already at the bottom.
  if (wasAtBottom) view.scrollTop = view.scrollHeight;
}

// ---------- Flowchart View ----------
function renderFlowchart(svg) {
  const view = $('#flowchart-view');
  if (!svg) {
    view.innerHTML = '<div class="empty"><div class="icon">📊</div>No plan data</div>';
    return;
  }
  view.innerHTML = svg;
}

function clearAgentPanel() {
  state.activeTab = 'dialogue';
  $('#tab-bar').style.display = 'none';
  $('#agent-header').textContent = '';
  $('#dialogue-view').innerHTML = '<div class="empty"><div class="icon">💬</div>Select a subtask</div>';
  $('#flowchart-view').innerHTML = '';
  $('#flowchart-view').style.display = 'none';
  $('#dialogue-view').style.display = 'flex';
}

// ---------- Helpers ----------
function esc(s) {
  if (typeof s !== 'string') return '';
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}
function fmtTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleTimeString();
}

// ---------- Resizer: Draggable dividers ----------
function initResizers() {
  // Create a ghost overlay to capture mousemove during drag.
  let ghost = document.getElementById('resize-ghost');
  if (!ghost) {
    ghost = document.createElement('div');
    ghost.id = 'resize-ghost';
    document.body.appendChild(ghost);
  }

  document.querySelectorAll('.resizer').forEach(resizer => {
    resizer.addEventListener('mousedown', (e) => {
      e.preventDefault();
      const targetId = resizer.dataset.target;
      const target = document.getElementById(targetId);
      if (!target) return;
      const side = resizer.dataset.side || 'right';
      const startX = e.clientX;
      const startW = target.getBoundingClientRect().width;

      ghost.classList.add('active');

      function onMove(ev) {
        let dx = ev.clientX - startX;
        if (side === 'left') dx = -dx;
        let newW = startW + dx;
        // Clamp within min/max — use computed style (from CSS classes)
        const cs = getComputedStyle(target);
        const minW = parseFloat(cs.minWidth) || 160;
        const maxW = parseFloat(cs.maxWidth) || 500;
        newW = Math.max(minW, Math.min(maxW, newW));
        target.style.width = newW + 'px';
      }

      function onUp() {
        ghost.classList.remove('active');
        document.removeEventListener('mousemove', onMove);
        document.removeEventListener('mouseup', onUp);
      }

      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    });
  });
}

// ---------- Context Menu (右键删除总任务) ----------
function showMasterTaskContextMenu(x, y, mtId, wsId, goal) {
  removeCtxMenu();

  const menu = document.createElement('div');
  menu.className = 'ctx-menu';
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  menu.innerHTML = `<div class="ctx-menu-item danger" data-action="delete">🗑️ 删除总任务</div>`;
  menu.querySelector('[data-action="delete"]').onclick = async () => {
    removeCtxMenu();
    if (!confirm(`确认删除总任务「${goal}」及其所有子任务?`)) return;
    const err = await window.go.main.App.DeleteMasterTask(wsId, mtId);
    state.selMtId = null;
    state.subtasks = [];
    state.selStId = null;
    await loadMasterTasks();
    clearAgentPanel();
    if (err) {
      // Show error after UI refresh so the list is still updated.
      console.error('删除总任务失败:', err);
    }
  };
  document.body.appendChild(menu);

  // Dismiss on any click outside.
  setTimeout(() => {
    document.addEventListener('click', removeCtxMenu, { once: true });
  }, 0);
}

function removeCtxMenu() {
  document.querySelectorAll('.ctx-menu').forEach(el => el.remove());
  document.removeEventListener('click', removeCtxMenu);
}

// ---------- Boot ----------
// Register tab click handlers
document.addEventListener('DOMContentLoaded', () => {
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.onclick = () => {
      if (state.selStId === '__leader__') {
        switchTab(btn.dataset.tab);
      }
    };
  });
  initResizers();
});

init();
// All real-time updates are now event-driven via the engine event system.
// Master task list:  task-event → loadMasterTasks + update push (2s fallback)
// Subtask list:      task-event → loadSubtasks
// Leader dialogue:   EventLeaderLog → loadDialogue
// Subtask dialogue:  EventAgentLog → loadDialogue
