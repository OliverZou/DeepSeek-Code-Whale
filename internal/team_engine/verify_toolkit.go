package team_engine

import (
	"os"
	"path/filepath"
)

// verifyToolkitDir 是团队验证脚手架目录(whiteboard 下,worker 可读,
// 不属于任何任务交付物,不会污染工作区根)。
const verifyToolkitDir = "verify-toolkit"

// domShimFilename 是标准 DOM 验证脚手架文件名。
const domShimFilename = "dom-shim.js"

// VerifyToolkitPath 返回本 run 的 DOM 验证脚手架绝对路径(可能尚未写入,
// 由 EnsureVerifyToolkit 保证存在)。
func (wb *Whiteboard) VerifyToolkitPath() string {
	return filepath.Join(wb.baseDir, verifyToolkitDir, domShimFilename)
}

// EnsureVerifyToolkit 把标准 DOM 验证脚手架写入 whiteboard 目录(幂等)。
//
// 背景:DOM/交互类 worker 常因环境缺 jsdom/Playwright(只有 chromium 二进制
// 缓存)而手搓假 DOM shim,几十条 node -e 内联脚本留在会话历史里反复重放,
// 是交互 worker token 的最大单一来源(v11 交互 worker 1.12M、v15 DOM worker
// 341K 主要花在这)。预置一个经过验证、无依赖的 shim,worker 直接 require,
// 禁止重复发明。
func (wb *Whiteboard) EnsureVerifyToolkit() error {
	path := wb.VerifyToolkitPath()
	if _, err := os.Stat(path); err == nil {
		return nil // 已存在,幂等
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(domShimSource), 0644)
}

// domShimSource 是标准 DOM 验证 shim(纯 Node,无依赖)。
// worker 用它对 DOM 交互交付物做行为验证(渲染/事件路由/分数更新),
// 而不是从头发明 DOM mock。
const domShimSource = `// 标准 DOM 验证脚手架(whale team 预置,无依赖)。
// 用法: const { document, window, el, makeEvent } = require('<此文件路径>');
// 然后把被测代码的 document/window/localStorage 换成这里的实现:
//   1) 被测代码若是 CJS 且引用全局 document/window,直接 require 后设置
//      globalThis.document / globalThis.window = ... 再加载被测文件;
//   2) 断言用 Node 内置 assert / node:test。
// 覆盖:元素工厂、classList、子节点、textContent/innerHTML、dataset、属性、
// 事件绑定与派发、getElementById/querySelector、内存 localStorage。
// 禁止:为同一个交付物再手写第二套 DOM mock(易错且烧轮数)。

'use strict';

function makeClassList(el) {
  const set = new Set(el._className ? el._className.split(/\s+/).filter(Boolean) : []);
  return {
    add(...c) { for (const x of c) set.add(x); el._className = [...set].join(' '); },
    remove(...c) { for (const x of c) set.delete(x); el._className = [...set].join(' '); },
    contains(c) { return set.has(c); },
    toggle(c, force) {
      const on = force !== undefined ? !!force : !set.has(c);
      on ? set.add(c) : set.delete(c);
      el._className = [...set].join(' ');
      return on;
    },
    get value() { return [...set]; },
  };
}

function makeElement(tag, className, text) {
  const el = {
    tagName: String(tag).toUpperCase(),
    nodeType: 1,
    children: [],
    style: {},
    dataset: {},
    attributes: {},
    _className: className || '',
    _innerHTML: '',
    _listeners: {},
    classList: null,
    parentNode: null,
    textContent: '',
    appendChild(child) {
      if (child == null) return child;
      if (child.parentNode) child.parentNode.removeChild(child);
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    removeChild(child) {
      const i = this.children.indexOf(child);
      if (i >= 0) this.children.splice(i, 1);
      child.parentNode = null;
      return child;
    },
    replaceChildren(...nodes) {
      for (const c of [...this.children]) this.removeChild(c);
      for (const n of nodes) this.appendChild(n);
    },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    addEventListener(type, fn) {
      (this._listeners[type] = this._listeners[type] || []).push(fn);
    },
    removeEventListener(type, fn) {
      const l = this._listeners[type] || [];
      const i = l.indexOf(fn);
      if (i >= 0) l.splice(i, 1);
    },
    dispatchEvent(ev) {
      ev.target = ev.target || this;
      ev.currentTarget = this;
      for (const fn of [...(this._listeners[ev.type] || [])]) fn(ev);
      // 冒泡到父级
      if (ev.bubbles !== false && this.parentNode) this.parentNode.dispatchEvent(ev);
      return !ev.defaultPrevented;
    },
    querySelector(sel) {
      return queryIn(this.children, sel);
    },
    querySelectorAll(sel) {
      const out = [];
      collectBySelector(this.children, sel, out);
      return out;
    },
    closest(sel) {
      let node = this;
      while (node) {
        if (matchesSelector(node, sel)) return node;
        node = node.parentNode;
      }
      return null;
    },
    click() {
      this.dispatchEvent(makeEvent('click'));
    },
    get innerHTML() { return this._innerHTML; },
    set innerHTML(v) {
      this._innerHTML = String(v);
      this.children = [];
      parseHTMLInto(this, this._innerHTML);
    },
  };
  el.classList = makeClassList(el);
  Object.defineProperty(el, 'className', {
    get() { return el._className; },
    set(v) { el._className = String(v); },
  });
  el.textContent = text != null ? String(text) : '';
  return el;
}

function stripTags(s) {
  return String(s).replace(/<[^>]*>/g, '');
}

// parseHTMLInto 递归解析静态 HTML 片段(支持嵌套标签与 class/id),用于
// innerHTML 赋值后的结构查询。属性只识别 class / id / data-*,其余忽略。
function parseHTMLInto(parent, html) {
  const re = /<([a-zA-Z0-9]+)((?:\s+class="[^"]*")?)((?:\s+id="[^"]*")?)((?:\s+data-[^=]*="[^"]*")*)\s*>(.*?)<\/\1>/gs;
  let last = 0;
  let m;
  while ((m = re.exec(html)) != null) {
    last = re.lastIndex;
    const cls = /class="([^"]*)"/.exec(m[2]);
    const idm = /id="([^"]*)"/.exec(m[3]);
    const el = makeElement(m[1], cls ? cls[1] : '', '');
    if (idm) el.id = idm[1];
    const inner = m[5];
    if (/<[a-zA-Z]/.test(inner)) {
      parseHTMLInto(el, inner);
    } else {
      el.textContent = stripTags(inner);
    }
    parent.appendChild(el);
  }
  // 标签之间的纯文本(如 "text <b>x</b> more")保留在父级文本。
  if (last < html.length && html.slice(last).trim()) {
    parent.appendChild(makeElement('text', '', stripTags(html.slice(last))));
  }
}

function matchesSelector(el, sel) {
  if (!el || el.nodeType !== 1) return false;
  sel = String(sel).trim();
  if (sel.startsWith('#')) {
    const id = el.attributes.id ?? el.id;
    return id === sel.slice(1);
  }
  if (sel.startsWith('.')) return el.classList.contains(sel.slice(1));
  return el.tagName.toLowerCase() === sel.toLowerCase();
}

function queryIn(children, sel) {
  for (const c of children) {
    if (matchesSelector(c, sel)) return c;
    const deep = queryIn(c.children, sel);
    if (deep) return deep;
  }
  return null;
}

function collectBySelector(children, sel, out) {
  for (const c of children) {
    if (matchesSelector(c, sel)) out.push(c);
    collectBySelector(c.children, sel, out);
  }
}

function memoryStorage() {
  const store = new Map();
  return {
    get length() { return store.size; },
    getItem(k) { return store.has(String(k)) ? store.get(String(k)) : null; },
    setItem(k, v) { store.set(String(k), String(v)); },
    removeItem(k) { store.delete(String(k)); },
    clear() { store.clear(); },
    key(i) { return [...store.keys()][i] ?? null; },
  };
}

const document = {
  readyState: 'complete',
  _root: makeElement('html'),
  createElement: (tag) => makeElement(tag),
  getElementById(id) {
    const found = queryIn(this._root.children, '#' + id);
    if (!found) return null;
    const fid = found.attributes.id ?? found.id;
    return fid === id ? found : null;
  },
  querySelector(sel) {
    return queryIn(this._root.children, sel);
  },
  querySelectorAll(sel) {
    const out = [];
    collectBySelector(this._root.children, sel, out);
    return out;
  },
  addEventListener() {},
  removeEventListener() {},
  get body() { return this._root; },
  get documentElement() { return this._root; },
  createTextNode: (t) => ({ nodeType: 3, textContent: String(t) }),
};

function makeEvent(type, props) {
  return Object.assign({
    type,
    bubbles: true,
    cancelable: true,
    defaultPrevented: false,
    preventDefault() { this.defaultPrevented = true; },
    stopPropagation() { this.bubbles = false; },
    key: '',
    keyCode: 0,
    code: '',
    target: null,
    currentTarget: null,
    touches: [],
    changedTouches: [],
  }, props || {});
}

const window = {
  document,
  addEventListener() {},
  removeEventListener() {},
  innerWidth: 1280,
  innerHeight: 800,
  localStorage: memoryStorage(),
  requestAnimationFrame: (fn) => setTimeout(() => fn(Date.now()), 0),
  cancelAnimationFrame: (id) => clearTimeout(id),
};

module.exports = { document, window, el: makeElement, makeEvent, memoryStorage };
`
