import assert from 'node:assert/strict';
import test from 'node:test';
import { UsagePanel } from './assets/js/usage.js';

test('account costs and group charges stay distinct when switching cards', () => {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) {
      const classes = new Set();
      elements.set(selector, {
        innerHTML: '',
        classList: {
          toggle(name, enabled) { if (enabled) classes.add(name); else classes.delete(name); },
          contains(name) { return classes.has(name); }
        },
        style: { setProperty() {} },
        addEventListener() {},
        setAttribute() {},
        closest() { return null; },
        querySelector() { return null; },
        querySelectorAll() { return []; }
      });
    }
    return elements.get(selector);
  };
  globalThis.document = { querySelector: element, querySelectorAll() { return []; } };
  const panel = new UsagePanel();
  const common = { base_cost: 100, total_tokens: 1000000, platform: 'openai' };
  panel.usage = {
    summary: { ...common, total_cost: 10, effective_rate_multiplier: 0.1, cost_per_million_tokens: 10 },
    accounts: [
      { ...common, name: 'Account A', key: 'account:1', total_cost: 20, effective_rate_multiplier: 0.2, cost_per_million_tokens: 20 },
      { ...common, name: 'Account B', key: 'account:2', total_cost: 30, effective_rate_multiplier: 0.3, cost_per_million_tokens: 30 }
    ],
    groups: [{ ...common, name: 'Group', key: 'group:1', total_cost: 10, effective_rate_multiplier: 0.1, cost_per_million_tokens: 10 }]
  };
  panel.render();
  const cards = element('#usageEntityCardList');
  assert.match(cards.innerHTML, /分组收费<\/span><strong>\$10\.0000/);
  assert.match(cards.innerHTML, /分组有效倍率<\/span><strong>0\.1000×/);
  assert.doesNotMatch(cards.innerHTML, /账户成本|账户有效倍率/);
  const overview = element('#usageKpiGrid').innerHTML;
  assert.match(overview, /用户扣费/);
  assert.match(overview, /收费有效倍率/);

  panel.setEntityKind('account');
  assert.match(cards.innerHTML, /账户成本<\/span><strong>\$20\.0000/);
  assert.match(cards.innerHTML, /账户有效倍率<\/span><strong>0\.2000×/);
  assert.match(cards.innerHTML, /title="账户成本 \/ 1M Tokens"/);
  assert.match(cards.innerHTML, /按所选窗口的历史请求加权计算/);
  assert.doesNotMatch(cards.innerHTML, /分组收费|分组有效倍率/);
  assert.ok(cards.innerHTML.indexOf('Account A') < cards.innerHTML.indexOf('Account B'));
  panel.setEntitySortMetric('cost');
  panel.setEntitySortDirection('desc');
  assert.ok(cards.innerHTML.indexOf('Account B') < cards.innerHTML.indexOf('Account A'));
  assert.equal(element('#usageKpiGrid').innerHTML, overview);

  panel.setEntityKind('group');
  assert.match(cards.innerHTML, /分组收费/);
  assert.match(cards.innerHTML, /0\.1000×/);
  assert.doesNotMatch(cards.innerHTML, /账户成本|账户有效倍率/);
});
