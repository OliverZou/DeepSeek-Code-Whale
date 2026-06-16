// Whale Dashboard — Wails frontend
// Calls Go backend via window.go.main.App.<Method>()
// Listens for updates via window.runtime.EventsOn("update", ...)

const $ = (s) => document.querySelector(s);

let state = { masterTasks: [], selMtId: null, subtasks: [], selStId: null, activeTab: 'dialogue', unreadTasks: new Set() };

// ---------- Init ----------
function init() {
  if (typeof window.go === 'undefined') { document.getElementById('mt-count').textContent = 'FATAL: no Wails bridge'; return; }
  loadMasterTasks();
  window.runtime.EventsOn("update", (payload) => {
    if (payload && typeof payload.length === 'number') {
      state.masterTasks = payload;
      updateUI();
    } else {
      document.getElementById('mt-count').textContent = 'update: bad payload type=' + typeof payload;
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


	// EventStateChanged (type=0): batch/task lifecycle changes.
	// task_id is empty for global state changes — still refresh.
	if (event.type === 0) {
		loadMasterTasks().then(() => {
			// Auto-select the first master task with subtasks if none selected.
			if (!state.selMtId && state.masterTasks.length > 0) {
				let planned = state.masterTasks.find(mt => (mt.task_count || 0) > 0);
				if (!planned) planned = state.masterTasks.find(mt => mt.status === 'running');
					if (planned) selectMasterTask(planned.id);
			}
		});
		const mt0 = getSelMt();
		if (mt0 && state.selMtId) {
			loadSubtasks(mt0.workspace_id, mt0.id);
		}
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
    document.getElementById('mt-count').textContent = (newTasks ? newTasks.length : 0) + ' master tasks';
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
      let planned = newTasks.find(mt => (mt.task_count || 0) > 0);
      if (!planned) planned = newTasks.find(mt => mt.status === 'running');
      if (planned) {
        state.selMtId = planned.id;
        state._lastMtId = null;
        loadSubtasks(planned.workspace_id, planned.id);
      }
    }
    updateUI();
  } catch (e) { console.error("loadMasterTasks failed:", e); }
}

async function loadSubtasks(wsID, mtID) {
  try {
    const newSubtasks = await window.go.main.App.GetSubtasks(wsID, mtID);
    // Detect changes: mark subtasks as unread when state/progress changes.
    const prevMap = new Map();
    for (const st of state.subtasks) { prevMap.set(st.id, st); }
    for (const st of newSubtasks) {
      const prev = prevMap.get(st.id);
      if (!prev || prev.state !== st.state || prev.progress !== st.progress) {
        state.unreadTasks.add(st.id);
      }
    }
    state.subtasks = newSubtasks;
    // Preserve selected subtask ID across refreshes.
    const prevSelStId = state.selStId;
    if (state.selStId) {
      const found = newSubtasks.find(st => st.id === state.selStId);
      if (!found) state.selStId = null;
    }
    renderSubtasks();
    // Auto-select first real subtask (skip __leader__) on first load.
    if (newSubtasks.length > 0 && !state.selStId && !prevSelStId) {
      const firstReal = newSubtasks.find(st => st.id !== '__leader__');
      selectSubtask(firstReal ? firstReal.id : newSubtasks[0].id);
    }
  } catch (e) {
    window.go.main.App.LogFrontend('loadSubtasks: ' + (e.message || e));
  }
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
  } catch (e) {
    window.go.main.App.LogFrontend('loadDialogue: ' + (e.message || e));
  }
}

async function loadFlowchart(wsID, mtID) {
  try {
    const svg = await window.go.main.App.GetLeaderFlowchart(wsID, mtID);
    renderFlowchart(svg);
  } catch (e) {
    window.go.main.App.LogFrontend('loadFlowchart: ' + (e.message || e));
  }
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
    // Auto-select first available master task.  Safe to call on every
    // poll — if already selected this is a no-op.
    let planned = state.masterTasks.find(mt => (mt.task_count || 0) > 0);
    if (!planned) planned = state.masterTasks.find(mt => mt.status === 'running');
    if (planned) {
      state.selMtId = planned.id;
      loadSubtasks(planned.workspace_id, planned.id);
    }
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
    // Placeholder: idle (no DB yet) or ready (DB exists, no master tasks).
    // Show a clean one-liner — no meta clutter, progress bar, or buttons.
    if (mt.status === 'idle' || mt.status === 'ready') {
      html += `<div class="mt idle" data-id="" data-goal="${esc(mt.goal)}" data-wsid="${esc(mt.workspace_id)}">
        <div class="goal" title="${esc(mt.goal)}">${esc(mt.goal)}</div>
      </div>`;
      continue;
    }
    const active = state.selMtId === mt.id ? ' active' : '';
    const goalShort = esc(mt.goal).length > 50 ? esc(mt.goal).slice(0, 50) + '…' : esc(mt.goal);
    const pct = mt.task_count > 0 ? Math.round(mt.done_count / mt.task_count * 100) : 0;
    const isRunning = mt.status === 'running' || (mt.workspace_online && (mt.task_count > 0 && mt.done_count < mt.task_count));
    const isPlanning = !isRunning && (mt.task_count || 0) === 0;
    const hasRunning = isRunning || (mt.active_count || 0) > 0;
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
      if (!el.dataset.id) return;
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
// ---------- Subtask List ----------
function renderSubtasks() {
  const list = $('#task-tabs');
  if (!state.subtasks.length) {
    list.innerHTML = '<div class="empty"><div class="icon">📋</div>No subtasks</div>';
    return;
  }
  try {
  let html = '';
  // Determine workspace ID for stop button calls.
  const wsid = state.selMtId ? (getSelMt() ? getSelMt().workspace_id : '') : '';
  // Recursive tree render helper.
  const renderTree = (tasks, depth) => {
    for (const st of tasks) {
      const active = state.selStId === st.id ? ' sel' : '';
      const leader = st.id === '__leader__' ? ' leader' : '';
      const exhausted = (st.retry_count > 0 && st.max_retries > 0 && st.retry_count >= st.max_retries);
      const stateDot = st.id === '__leader__' ? 'leader-dot'
        : st.state === 'done' ? 'done'
        : st.state === 'running' || st.state === 'producing' || st.state === 'verifying' ? 'running'
        : st.state === 'suspended' ? 'suspended'
        : st.state === 'failed' ? 'failed'
        : exhausted ? 'failed'
        : 'pending';
      const icon = st.id === '__leader__' ? '📋'
        : (st.children && st.children.length > 0) ? '📂'
        : '🎭';
      const indent = depth * 18; // 18px per level for visible but compact hierarchy
      const hasChildren = st.children && st.children.length > 0;
      const retryBadge = exhausted ? '<span class="retry-badge" title="重试耗尽，已被重新分解">🔄</span>' : '';
      // Show child count for re-decomposed (management) nodes.
      const childBadge = hasChildren ? `<span class="child-count">${st.children.length}↳</span>` : '';
      // Tree connector line for child items.
      const treeLine = depth > 0 ? '<span class="tree-line"></span>' : '';

      html += `<div class="st${active}${leader}" data-id="${st.id}" style="padding-left:${indent + 8}px">
        <div class="st-title" title="${esc(st.title)}">
          ${treeLine}<span class="state-dot ${stateDot}${state.unreadTasks.has(st.id) ? " pulse" : ""}"></span>
          ${icon} ${esc(st.title)} ${retryBadge}${childBadge}
          ${state.unreadTasks.has(st.id) ? '<span class="unread-badge">●</span>' : ''}
        </div>
        <div class="st-meta">
          <span>${esc(st.role)}</span>
          ${st.output ? '<span class="st-output" title="' + esc(st.output) + '">📄 ' + esc(st.output).substring(0, 40) + (st.output.length > 40 ? '…' : '') + '</span>' : ''}
          ${exhausted ? '<span class="exhausted-label">已重分解</span>' : ''}
          <span>${st.progress}%</span>
        </div>
      </div>`;
      // Render children if any.
      if (hasChildren) {
        renderTree(st.children, depth + 1);
      }
    }
  };
  renderTree(state.subtasks, 0);
  list.innerHTML = html;
  list.querySelectorAll('.st').forEach(el => {
    el.onclick = (e) => selectSubtask(el.dataset.id);
  });
  } catch (e) {
    window.go.main.App.LogFrontend('renderSubtasks: ' + (e.message || e));
    list.innerHTML = '<div class="empty">Render error: ' + (e.message || e) + '</div>';
}
}


// Recursively find a subtask by ID in the nested children tree.
function findSubtask(tasks, id) {
  for (const t of tasks) {
    if (t.id === id) return t;
    if (t.children && t.children.length > 0) {
      const found = findSubtask(t.children, id);
      if (found) return found;
    }
  }
  return null;
}

function selectSubtask(id) {
  state.selStId = id;
  state.activeTab = 'dialogue';
  renderSubtasks();

  const st = findSubtask(state.subtasks, id);
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
  // Extract viewBox dimensions and set them as explicit SVG width/height
  // so the SVG renders at exactly its viewBox size.
  const m = svg.match(/viewBox="0 0 (\d+) (\d+)"/);
  if (m) {
    svg = svg.replace('<svg', `<svg width="${m[1]}" height="${m[2]}"`);
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
	    if (err) {
	      console.error('删除总任务失败:', err);
	      return;
	    }
	    // Clear local state immediately, then reload from backend.
	    state.selMtId = null;
	    state.subtasks = [];
	    state.selStId = null;
	    state.masterTasks = (state.masterTasks || []).filter(mt => mt.id !== mtId);
	    updateUI();
	    clearAgentPanel();
	    // Reload in background, then auto-select if tasks remain.
	    setTimeout(async () => {
	      await loadMasterTasks();
	      if (!state.selMtId && state.masterTasks.length > 0) {
	        let planned = state.masterTasks.find(mt => (mt.task_count || 0) > 0);
	        if (!planned) planned = state.masterTasks.find(mt => mt.status === 'running');
					if (planned) selectMasterTask(planned.id);
	      }
	    }, 200);
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
