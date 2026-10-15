import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('./web/app.js', import.meta.url), 'utf8');
function history() {
  const nodes = new Map(), requests = [], storage = new Map();
  const node = () => ({ dataset: {}, children: [], textContent: '', replaceChildren(...children) { this.children = children; }, showModal() { this.open = true; }, addEventListener(name, callback) { this[name] = callback; } });
  const $ = (id) => { if (!nodes.has(id)) nodes.set(id, node()); return nodes.get(id); };
  const state = { runs: [], markedRuns: new Set(), selectedRuns: { compose: '', history: '' }, cache: new Map(), refreshing: null, deletingRuns: false };
  const context = vm.createContext({ state, $, el: (_tag, _attrs, text) => text, document: { querySelectorAll: () => [] }, localStorage: { getItem: (key) => storage.get(key), removeItem: (key) => storage.delete(key) }, api: async (path, options) => { requests.push({ path, ...options }); }, guard: (fn) => fn, notice: () => {}, renderRunList: () => {} });
  const start = source.indexOf('function isActiveRun('), end = source.indexOf('function renderRunList(');
  assert.ok(start >= 0 && end > start);
  vm.runInContext('let pendingDeleteRuns = [];\n' + source.slice(start, end) + '\nrefreshRuns = async () => {};', context);
  vm.runInContext(source.split('\n').find((line) => line.startsWith("$('#delete-runs-dialog').addEventListener('close'")), context);
  return { context, state, $, requests, storage };
}
const run = (id, status = 'completed') => ({ id, status, name: id, created_at: '2026-10-15T00:00:00Z' });

test('opening or cancelling confirmation sends no delete request', async () => {
  const h = history(), r = run('one'); h.state.runs = [r];
  for (const answer of ['cancel', '']) {
    h.context.confirmRunDeletion([r]);
    assert.equal(h.$('#delete-runs-dialog').open, true);
    h.$('#delete-runs-dialog').returnValue = answer;
    await h.$('#delete-runs-dialog').close();
    assert.equal(h.requests.length, 0);
    assert.deepEqual(h.state.runs, [r]);
  }
});

test('confirmation deletes the displayed batch IDs and clears stale details', async () => {
  const h = history(), a = run('one'), b = run('two'), keep = run('keep');
  h.state.runs = [a, b, keep]; h.state.markedRuns = new Set([a.id, b.id]);
  h.state.selectedRuns = { compose: a.id, history: b.id }; h.state.detail = { run: a }; h.state.cache.set(1, {});
  h.storage.set('account_eval_current_run', a.id);
  h.context.confirmRunDeletion([a, b]);
  h.state.markedRuns = new Set([keep.id]);
  h.$('#delete-runs-dialog').returnValue = 'delete';
  await h.$('#delete-runs-dialog').close();
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].path, 'runs/delete');
  assert.equal(h.requests[0].method, 'POST');
  assert.deepEqual(JSON.parse(h.requests[0].body), { run_ids: [a.id, b.id] });
  assert.deepEqual(h.state.runs, [keep]);
  assert.equal(h.state.detail, null);
  assert.equal(h.state.cache.size, 0);
  assert.equal(h.storage.size, 0);
  assert.deepEqual(h.state.selectedRuns, { compose: '', history: '' });
  assert.equal(h.$('#run-detail').hidden, true);
});

test('single deletion waits for an in-flight refresh before deleting', async () => {
  const h = history(), r = run('one'); h.state.runs = [r];
  let release; h.state.refreshing = new Promise((resolve) => { release = resolve; });
  const deletion = h.context.deleteRuns([r.id]);
  await Promise.resolve(); assert.equal(h.requests.length, 0);
  release(); await deletion;
  assert.equal(h.requests[0].path, 'runs/one');
  assert.equal(h.requests[0].method, 'DELETE');
  assert.equal(h.state.deletingRuns, false);
});

test('delete failure preserves records and selection for retry', async () => {
  const h = history(), r = run('one'); h.state.runs = [r]; h.state.markedRuns.add(r.id); h.state.detail = { run: r };
  h.context.api = async () => { throw new Error('failed'); };
  await assert.rejects(h.context.deleteRuns([r.id]), /failed/);
  assert.deepEqual(h.state.runs, [r]);
  assert.equal(h.state.markedRuns.has(r.id), true);
  assert.equal(h.state.detail.run.id, r.id);
  assert.equal(h.state.deletingRuns, false);
});

test('active batches cannot open a delete confirmation', () => {
  const h = history();
  for (const status of ['queued', 'running', 'cancelling']) assert.throws(() => h.context.confirmRunDeletion([run(status, status)]), /结束后再删除/);
  assert.equal(h.requests.length, 0);
});
