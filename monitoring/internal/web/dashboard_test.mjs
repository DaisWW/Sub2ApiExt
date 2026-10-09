import assert from 'node:assert/strict';
import test from 'node:test';
import { DashboardPanel } from './assets/js/dashboard.js';

function renderTarget(target) {
  const elements = new Map();
  globalThis.document = {
    querySelector(selector) {
      if (!elements.has(selector)) {
        elements.set(selector, {
          innerHTML: '',
          classList: { toggle() {} },
          style: { setProperty() {} },
          addEventListener() {},
          setAttribute() {},
          querySelector() { return null; }
        });
      }
      return elements.get(selector);
    },
    querySelectorAll() { return []; }
  };
  globalThis.window = { requestAnimationFrame() {} };
  const panel = new DashboardPanel(() => {});
  panel.filter = target.kind;
  panel.dashboard = { targets: [target], summary: { targets: 1 } };
  panel.render();
  return elements.get('#targetGrid').innerHTML;
}

function sample(status, latencyMs = 1000) {
  return { status, latency_ms: latencyMs, checked_at: '2026-10-09T02:00:00Z', source: 'aggregate' };
}

const cases = [
  { name: 'slow group with an older green trajectory', status: 'degraded', reason: 'slow', last: 'operational', display: 'degraded' },
  { name: 'failed account with an older green trajectory', kind: 'account', status: 'failed', last: 'operational', display: 'failed' },
  { name: 'recovered group with an older red trajectory', status: 'operational', last: 'failed', display: 'operational' },
  { name: 'recovered account with old slow latency', kind: 'account', status: 'operational', latency: 79000, currentSamples: 12, last: 'degraded', display: 'operational' },
  { name: 'slow account without current request evidence', kind: 'account', status: 'operational', latency: 25000, last: 'operational', display: 'degraded' },
  { name: 'rate limited route with an older green trajectory', status: 'operational', reason: 'rate_limited', last: 'operational', display: 'degraded' },
  { name: 'usable mixed group with slow diagnostic metrics', status: 'operational', latency: 79000, last: 'operational', display: 'operational' },
  { name: 'explicit failure despite sparse current evidence', kind: 'account', status: 'failed', currentSamples: 2, last: 'operational', display: 'failed' },
  { name: 'carried current evidence preserves its age', status: 'operational', last: 'operational', carried: true, stale: true, display: 'operational' },
  { name: 'failed current state without hourly evidence', status: 'failed', last: 'unknown', display: 'failed' },
  { name: 'failed group without a trajectory', status: 'failed', empty: true, display: 'failed' },
  { name: 'unknown group without evidence', status: 'unknown', empty: true, noSamples: true, display: 'operational' }
];

for (const scenario of cases) {
  test(scenario.name, () => {
    const recentSamples = scenario.empty ? [] : [sample('failed'), sample('degraded', 79000), sample(scenario.last)];
    if (scenario.carried) recentSamples.at(-1).carried_from = '2026-10-09T01:00:00Z';
    const originalSamples = structuredClone(recentSamples);
    const target = {
      key: `${scenario.kind || 'group'}:1`,
      kind: scenario.kind || 'group',
      name: 'Test route',
      platform: 'openai',
      status: scenario.status,
      health_reason: scenario.reason || '',
      latest_source: scenario.noSamples ? '' : scenario.kind === 'account' ? 'history' : 'aggregate',
      latest_latency_ms: scenario.latency || 1000,
      current_health: { samples: scenario.currentSamples || 0 },
      stale: Boolean(scenario.stale),
      stats: {
        samples: scenario.noSamples ? 0 : 50,
        availability: 100,
        first_byte: { median_ms: 66000 },
        latency: { median_ms: 79000 }
      },
      recent_samples: recentSamples
    };
    const html = renderTarget(target);
    const tone = { operational: 'good', degraded: 'warn', failed: 'bad' }[scenario.display];
    assert.match(html, new RegExp(`target-card target-${scenario.display}`));
    assert.match(html, new RegExp(`status-badge status-${scenario.display}`));
    const availability = html.match(/<strong class="availability-value ([^"]+)"[^>]*>([^<]+)<\/strong>/);
    assert.equal(availability[1], tone);
    assert.equal(availability[2], scenario.noSamples ? '—' : '100.00%');
    assert.match(html, /metric-value warn">1m 06s/);

    const history = html.match(/<div class="status-history"[^>]*>([\s\S]*?)<\/div>/)[1];
    const cells = [...history.matchAll(/<i class="([^"]+)"[^>]*aria-label="([^"]*)"/g)];
    assert.equal(cells.length, 24);
    const currentTone = scenario.display === 'operational' ? 'ok' : tone;
    assert.equal(cells.at(-1)[1], `${currentTone}${scenario.stale ? ' carried' : ''}`);
    assert.match(cells.at(-1)[2], /当前/);
    if (!scenario.empty) {
      assert.equal(cells.at(-3)[1], 'bad');
      assert.equal(cells.at(-2)[1], 'warn');
      if (scenario.last === 'unknown') assert.match(cells.at(-1)[2], /数据不足/);
      else assert.match(cells.at(-1)[2], /轨迹记录：.*账户聚合/);
      if (scenario.carried) assert.match(cells.at(-1)[2], /无新请求，沿用/);
    } else {
      assert.equal(cells.at(-2)[1], 'ok');
    }
    assert.deepEqual(target.recent_samples, originalSamples);
  });
}
