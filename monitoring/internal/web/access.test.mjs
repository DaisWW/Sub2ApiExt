import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('./assets/js/shared.js', import.meta.url), 'utf8');

for (const prefix of ['/', '/monitoring/']) {
  test(`API uses the current login and same-origin base at ${prefix}`, async () => {
    let token = 'user-token';
    let responseToken = token;
    const context = vm.createContext({
      URL,
      localStorage: { getItem: () => token },
      fetch: async (url, options) => {
        assert.equal(url.href, `https://site.example${prefix}api/v1/monitor/dashboard`);
        assert.equal(options.headers.Authorization, token ? `Bearer ${token}` : undefined);
        assert.equal(options.cache, 'no-store');
        return { ok: true, json: async () => {
          token = responseToken;
          return { targets: [] };
        } };
      },
    });
    const module = new vm.SourceTextModule(source, {
      context,
      initializeImportMeta(meta) { meta.url = `https://site.example${prefix}js/shared.js`; },
    });
    await module.link(() => { throw new Error('unexpected dependency'); });
    await module.evaluate();
    const { api } = module.namespace;
    await api('/api/v1/monitor/dashboard');
    token = responseToken = '';
    await api('/api/v1/monitor/dashboard');
    token = 'admin-token';
    responseToken = 'user-token';
    await assert.rejects(api('/api/v1/monitor/dashboard'), /登录状态已变化/);
  });
}

test('blocked browser storage keeps anonymous public access available', async () => {
  const context = vm.createContext({
    URL,
    localStorage: { getItem() { throw new Error('storage blocked'); } },
    fetch: async (_url, options) => {
      assert.equal(options.headers.Authorization, undefined);
      return { ok: true, json: async () => ({ targets: [] }) };
    },
  });
  const module = new vm.SourceTextModule(source, {
    context,
    initializeImportMeta(meta) { meta.url = 'https://monitor.example/js/shared.js'; },
  });
  await module.link(() => {});
  await module.evaluate();
  await module.namespace.api('/api/v1/monitor/dashboard');
});

test('failed authorization cannot restore cached dashboard, usage or alerts', async () => {
  let status = 200;
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) elements.set(selector, {
      innerHTML: '', textContent: '',
      classList: { toggle() {}, add() {}, remove() {}, contains: () => false },
      style: { setProperty() {} },
      addEventListener() {}, setAttribute() {}, appendChild() {},
      closest() { return null; }, querySelector() { return null; },
      querySelectorAll() { return []; },
    });
    return elements.get(selector);
  };
  const context = vm.createContext({
    URL,
    localStorage: { getItem: () => null, setItem() {} },
    document: {
      querySelector: element, querySelectorAll: () => [],
      addEventListener() {}, createElement: () => element('toast'),
    },
    window: { requestAnimationFrame() {} },
    requestAnimationFrame() {},
    setTimeout() {},
    fetch: async () => ({
      ok: status === 200, status,
      json: async () => status === 200
        ? { items: [{ id: 1, target_key: 'account:2', target_name: 'secret-account', title: 'secret-alert' }] }
        : { error: '请重新登录' },
    }),
  });
  const shared = new vm.SourceTextModule(source, {
    context,
    initializeImportMeta(meta) { meta.url = 'https://site.example/monitoring/js/shared.js'; },
  });
  await shared.link(() => {});
  await shared.evaluate();
  const load = async (name) => {
    const code = await readFile(new URL(`./assets/js/${name}.js`, import.meta.url), 'utf8');
    const module = new vm.SourceTextModule(code, { context });
    await module.link(() => shared);
    await module.evaluate();
    return module.namespace;
  };
  const { DashboardPanel } = await load('dashboard');
  const { UsagePanel } = await load('usage');
  const { AlertCenter } = await load('alert-center');
  const dashboard = new DashboardPanel(() => {});
  dashboard.dashboard = {
    targets: [{ kind: 'group', key: 'group:20', name: 'secret-group' }],
    summary: { targets: 1 },
  };
  dashboard.render();
  const usage = new UsagePanel();
  usage.usage = { summary: {}, groups: [{ name: 'secret-group', key: 'group:20' }] };
  usage.render();
  const alerts = new AlertCenter();
  await alerts.load();
  assert.match(element('#alertList').innerHTML, /secret-alert/);
  status = 503;
  await alerts.load();
  assert.match(element('#alertList').innerHTML, /secret-alert/);
  status = 401;
  await dashboard.loadActivity();
  dashboard.setFilter('group');
  assert.doesNotMatch(element('#targetGrid').innerHTML, /secret-group/);
  dashboard.dashboard = {
    targets: [{ kind: 'group', key: 'group:20', name: 'secret-group' }],
    summary: { targets: 1 },
  };
  dashboard.render();
  await dashboard.load();
  dashboard.setFilter('group');
  assert.doesNotMatch(element('#targetGrid').innerHTML, /secret-group/);
  assert.doesNotMatch(element('#overallMeta').textContent, /1 个对象/);
  await usage.load();
  usage.setEntityKind('group');
  usage.setTrendMetric('tokens');
  assert.doesNotMatch(element('#usageEntityCardList').innerHTML, /secret-group/);
  await alerts.load();
  assert.doesNotMatch(element('#alertList').innerHTML, /secret-alert/);
});
