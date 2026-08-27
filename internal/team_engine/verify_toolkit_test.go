package team_engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureVerifyToolkit(t *testing.T) {
	dir := t.TempDir()
	wb, err := NewWhiteboard(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.EnsureVerifyToolkit(); err != nil {
		t.Fatalf("ensure toolkit: %v", err)
	}
	path := wb.VerifyToolkitPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	if !strings.Contains(string(data), "module.exports") {
		t.Fatal("shim must export its API")
	}
	// 幂等:再次调用不报错、不覆盖。
	if err := wb.EnsureVerifyToolkit(); err != nil {
		t.Fatalf("ensure toolkit again: %v", err)
	}
}

// TestDomShimUsable 用 node 对预置 shim 做冒烟验证——脚手架必须真实可用,
// 否则 worker 引用它会白烧轮数(v11/v15 手搓 shim 的教训)。
func TestDomShimUsable(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	wb, err := NewWhiteboard(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.EnsureVerifyToolkit(); err != nil {
		t.Fatal(err)
	}
	shim := filepath.ToSlash(wb.VerifyToolkitPath())

	probe := fmt.Sprintf(`
const { document, window, el, makeEvent } = require(%q);
const assert = require('assert');`, shim) + `
// 元素 + classList
const board = document.createElement('div');
board.id = 'board';
board.classList.add('board', 'grid');
assert(board.classList.contains('board'));
assert.strictEqual(board.className, 'board grid');
// 子节点 + textContent
const cell = el('div', 'cell', '4');
board.appendChild(cell);
assert.strictEqual(board.children.length, 1);
// getElementById / querySelector(支持直接 id 属性)
document._root.appendChild(board);
assert.strictEqual(document.getElementById('board'), board);
assert.strictEqual(document.querySelector('.cell'), cell);
// 事件绑定与派发
let key = null;
board.addEventListener('keydown', e => { key = e.keyCode; });
board.dispatchEvent(makeEvent('keydown', { keyCode: 39 }));
assert.strictEqual(key, 39);
// 内存 localStorage
window.localStorage.setItem('best', '1024');
assert.strictEqual(window.localStorage.getItem('best'), '1024');
// innerHTML 静态解析
const frag = el('div');
frag.innerHTML = '<div class="row"><span class="tile">2</span></div>';
assert.strictEqual(frag.querySelector('.tile').textContent, '2');
console.log('SHIM_OK');
`
	cmd := exec.Command("node", "-e", strings.TrimSpace(probe))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SHIM_OK") {
		t.Fatalf("shim probe did not pass:\n%s", out)
	}
}
