import assert from 'node:assert/strict';
import test from 'node:test';
import { DashboardPanel } from './assets/js/dashboard.js';
import { formatTime } from './assets/js/shared.js';

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
  { name: 'long total duration never repaints an idle account', kind: 'account', status: 'operational', latency: 25000, last: 'operational', display: 'operational' },
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
      current_health: {
        samples: scenario.currentSamples || 0,
        first_byte_samples: scenario.currentSamples || 0,
        first_byte: { median_ms: 2000 },
        applied: Boolean(scenario.currentSamples)
      },
      last_request_health: scenario.noSamples ? {} : {
        samples: 8, first_byte_samples: 8,
        first_byte: { median_ms: scenario.reason === 'slow' ? 25000 : 2000 },
        latest_at: '2026-10-09T01:00:00Z', applied: !scenario.currentSamples
      },
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
    assert.doesNotMatch(html, /metric-value warn">1m 06s/);
    assert.doesNotMatch(html, /metric-value warn">1m 19s/);
    assert.match(html, /总耗时 P95/);

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

test('current decision explains its exact first byte window', () => {
  const html = renderTarget({
    kind: 'group', key: 'group:10', name: 'Test', status: 'degraded', health_reason: 'slow',
    current_health: {
      samples: 8, first_byte_samples: 8, first_byte: { median_ms: 25000 }, applied: true
    },
    stats: { samples: 40, availability: 100, latency: { median_ms: 90000, p95_ms: 120000 } }
  });
  assert.match(html, /近 5 分钟首字中位数 25s · 8 次请求/);
  assert.match(html, /metric-value warn">25s/);
  assert.doesNotMatch(html, /metric-value warn">(?:1m 30s|2m 00s)/);
});

test('sparse first byte measurements stay neutral and explicit', () => {
  const html = renderTarget({
    kind: 'account', key: 'account:1', status: 'operational',
    current_health: { samples: 10, first_byte_samples: 4, first_byte: { median_ms: 30000 }, applied: true }
  });
  assert.match(html, /样本不足（首字 4\/5）/);
  assert.doesNotMatch(html, /metric-value warn/);
  assert.match(html, /target-card target-operational/);
});

test('idle state shows the original evidence time', () => {
  const html = renderTarget({
    kind: 'group', key: 'group:1', status: 'operational',
    current_health: { samples: 0 },
    last_request_health: {
      samples: 6, first_byte_samples: 6, first_byte: { median_ms: 2800 },
      latest_at: '2026-10-09T01:02:03Z', applied: true
    }
  });
  assert.match(html, /近 5 分钟无请求 · 沿用上次状态 · 证据/);
  assert.match(html, /上次 5 分钟首字中位数 2\.8s · 6 次请求/);
});

test('upstream errors have their own warning label', () => {
  const html = renderTarget({
    kind: 'group', key: 'group:1', status: 'degraded', health_reason: 'upstream_error',
    current_health: { samples: 10, hard_failures: 2, applied: true }
  });
  assert.match(html, /当前可用但部分请求报错/);
  assert.match(html, /最终请求失败 2\/10/);
  assert.doesNotMatch(html, /当前可用但延迟高/);
});

test('rate limits explain upstream attempts separately from final outcomes', () => {
  const html = renderTarget({
    kind: 'group', key: 'group:1', status: 'degraded', health_reason: 'rate_limited',
    current_health: {
      samples: 8, first_byte_samples: 8, first_byte: { median_ms: 2000 },
      rate_limited: 2, attempts: 10, rate_limit_rate: 20, applied: true
    }
  });
  assert.match(html, /当前可用但阶段性限速/);
  assert.match(html, /429 尝试 2\/10（20\.00%）/);
});

for (const current of [false, true]) {
  test(`${current ? 'current' : 'idle'} request metrics stay neutral after a newer recovery probe`, () => {
    const requestAt = '2026-10-09T01:02:03Z';
    const probeAt = '2026-10-09T01:04:03Z';
    const health = {
      samples: 8, first_byte_samples: 8, first_byte: { median_ms: 25000 },
      latest_at: requestAt, applied: false
    };
    const html = renderTarget({
      kind: 'account', key: 'account:1', status: 'operational',
      latest_source: 'probe', last_checked_at: probeAt,
      current_health: current ? health : { samples: 0 },
      last_request_health: current ? {} : health
    });
    assert.match(html, /target-card target-operational/);
    assert.doesNotMatch(html, /metric-value warn/);
    assert.ok(html.includes(`仅供参考 · 请求证据 ${formatTime(requestAt)}`));
    assert.ok(html.includes(`状态依据：主动探测 · ${formatTime(probeAt)}`));
  });
}

for (const [reason, label] of [['upstream_error', '请求错误状态'], ['rate_limited', '限速状态']]) {
  test(`idle ${reason} warning uses its actual reason`, () => {
    const html = renderTarget({
      kind: 'group', key: 'group:1', status: 'degraded', health_reason: reason,
      latest_source: 'aggregate', stale: true,
      last_request_health: { samples: 10, applied: true }
    });
    assert.ok(html.includes(`沿用最近${label}`));
    assert.doesNotMatch(html, /沿用最近延迟状态/);
  });
}
