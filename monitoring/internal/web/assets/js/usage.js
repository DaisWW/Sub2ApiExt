import {
  $,
  LatestRequest,
  activateToggle,
  api,
  escapeHTML,
  formatCount,
  formatTime,
  formatTokens,
  formatUSD,
  toast
} from './shared.js';

const donutColors = ['#77a9ef', '#54d6ae', '#e8b85f', '#c18df0', '#f27a82', '#8fd3ff', '#a4d17a', '#d8a0ff'];
const minimumDonutPercent = 0.5;
const usageMetrics = new Set(['tokens', 'cost', 'unit_cost']);
const shareMetricSet = new Set(['tokens', 'cost']);

export class UsagePanel {
  usage = null;
  period = '24h';
  entityKind = 'group';
  trendMetric = 'tokens';
  shareMetrics = { model: 'tokens', group: 'tokens' };
  #requests = new LatestRequest();

  constructor() {
    $('#usageBarChart').closest('.bar-chart-wrap')?.addEventListener('wheel', isolateChartWheel, { passive: false });
    document.querySelectorAll('[data-share-metric]').forEach((button) => {
      button.addEventListener('click', () => this.setShareMetric(button.dataset.shareKind, button.dataset.shareMetric, button));
    });
    document.querySelectorAll('[data-trend-metric]').forEach((button) => {
      button.addEventListener('click', () => this.setTrendMetric(button.dataset.trendMetric, button));
    });
    document.querySelectorAll('[data-usage-entity]').forEach((button) => {
      button.addEventListener('click', () => this.setEntityKind(button.dataset.usageEntity, button));
    });
  }

  setPeriod(period) {
    this.period = period;
  }

  setTrendMetric(metric, button) {
    if (!usageMetrics.has(metric)) return;
    this.trendMetric = metric;
    activateToggle('[data-trend-metric]', button);
    this.#renderTrend();
  }

  setEntityKind(kind, button) {
    if (kind !== 'group' && kind !== 'account') return;
    this.entityKind = kind;
    activateToggle('[data-usage-entity]', button);
    this.#renderEntityCards();
  }

  setShareMetric(kind, metric, button) {
    if (!(kind in this.shareMetrics) || !shareMetricSet.has(metric)) return;
    this.shareMetrics[kind] = metric;
    activateToggle(`[data-share-kind="${kind}"][data-share-metric]`, button);
    this.#renderShares();
  }

  async load({ silent = false } = {}) {
    const requestId = this.#requests.begin();
    if (!silent) setLoading(true, this.entityKind);
    try {
      const usage = await api(`/api/v1/monitor/usage-ranking?period=${encodeURIComponent(this.period)}&limit=10`);
      if (!this.#requests.isCurrent(requestId)) return;
      this.usage = usage;
      this.render();
    } catch (error) {
      if (!this.#requests.isCurrent(requestId)) return;
      const message = error?.message || '用量读取失败';
      showUsageError(message);
      toast(message);
    } finally {
      if (this.#requests.isCurrent(requestId)) setLoading(false);
    }
  }

  render() {
    if (!this.usage) return;
    const summary = this.usage.summary || {};
    $('#usageMeta').textContent = `${this.usage.period_label || '用量窗口'} · ${formatTime(this.usage.generated_at)} 更新`;
    $('#usageKpiGrid').innerHTML = usageKPIs(summary).map(renderUsageKPI).join('');
    $('#usageDetailStrip').innerHTML = usageDetailStrip(summary);
    this.#renderEntityCards();
    this.#renderShares();
    this.#renderTrend();
  }

  #renderShares() {
    if (!this.usage) return;
    const summary = this.usage.summary || {};
    const modelMetric = this.shareMetrics.model;
    const groupMetric = this.shareMetrics.group;
    renderShareDonut('modelDonutLayout', this.usage.models || [], summary, '暂无模型用量', '模型', modelMetric);
    renderShareDonut('groupDonutLayout', this.usage.groups || [], summary, '暂无分组用量', '分组', groupMetric);
  }

  #renderEntityCards() {
    const group = this.entityKind === 'group';
    $('#usageEntityEyebrow').textContent = group ? 'GROUP USAGE' : 'ACCOUNT USAGE';
    $('#usageEntityTitle').textContent = group ? '分组缓存与成本' : '账户缓存与成本';
    if (!this.usage) return;
    renderUsageCards(
      this.usage[group ? 'groups' : 'accounts'],
      group ? '暂无分组用量数据' : '暂无账户用量数据',
      this.entityKind
    );
  }

  #renderTrend() {
    if (!this.usage) return;
    renderUsageBars(this.usage.timeline || [], this.usage.bucket, this.trendMetric, this.usage.period);
  }
}

function setLoading(loading, entityKind = 'group') {
  $('#usageSection').classList.toggle('is-loading', loading);
  $('#usageSection').setAttribute('aria-busy', String(loading));
  if (!loading) return;
  $('#usageKpiGrid').innerHTML = '<div class="loading-state usage-loading">读取用量数据中...</div>';
  $('#usageDetailStrip').innerHTML = '<div class="loading-state usage-detail-loading">读取成本构成中...</div>';
  $('#usageEntityCardList').innerHTML = `<div class="loading-state usage-entity-loading">读取${entityKind === 'account' ? '账户' : '分组'}用量中...</div>`;
  $('#usageMeta').textContent = '正在更新用量窗口…';
}

function showUsageError(message) {
  const safeMessage = escapeHTML(message);
  $('#usageMeta').textContent = '用量读取失败';
  $('#usageKpiGrid').innerHTML = `<div class="empty-state usage-loading">${safeMessage}</div>`;
  $('#usageDetailStrip').innerHTML = '';
  $('#usageEntityCardList').innerHTML = `<div class="empty-state usage-entity-loading">${safeMessage}</div>`;
  $('#modelDonutLayout').innerHTML = `<div class="empty-state donut-empty">${safeMessage}</div>`;
  $('#groupDonutLayout').innerHTML = `<div class="empty-state donut-empty">${safeMessage}</div>`;
  $('#usageYAxis').innerHTML = '';
  $('#usageBarChart').classList.add('is-empty');
  $('#usageBarChart').innerHTML = `<div class="empty-state">${safeMessage}</div>`;
  $('#usageChannelLegend').innerHTML = '';
  $('#usageTrendBucket').textContent = '无法读取';
}

function usageKPIs(summary) {
  return [
    ['总 Tokens', formatTokens(summary.total_tokens), '', ''],
    ['请求数', formatCount(summary.requests), '', ''],
    ['费用', formatUSD(summary.total_cost), '', ''],
    ['有效倍率', formatMultiplier(summary.effective_rate_multiplier), '实际成本 / 原始成本', ''],
    ['每百万 Tokens 成本', formatUnitCost(summary.cost_per_million_tokens), '实际成本 / 1M Tokens', '']
  ];
}

function usageDetailStrip(summary) {
  const details = [
    ['输入 Tokens', formatTokens(summary.input_tokens)],
    ['输出 Tokens', formatTokens(summary.output_tokens)],
    ['Cache Create', formatTokens(summary.cache_creation_tokens)],
    ['Cache Read', formatTokens(summary.cache_read_tokens)],
    ['原始输入成本', formatUSD(summary.input_cost)],
    ['原始输出成本', formatUSD(summary.output_cost)],
    ['原始 Cache Create 成本', formatUSD(summary.cache_creation_cost)],
    ['原始 Cache Read 成本', formatUSD(summary.cache_read_cost)],
    ['原始 Token 成本', formatUSD(summary.token_cost)],
    ['原始非 Token 成本', formatUSD(summary.non_token_cost)]
  ];
  return `<div class="usage-detail-heading">窗口构成</div>
    <div class="usage-detail-grid">${details.map(([label, value]) =>
      `<div><span>${label}</span><strong>${value}</strong></div>`).join('')}</div>
    <div class="usage-detail-note">每百万 Tokens 成本按实际成本加权计算；输入/输出 Tokens 已包含图片 Tokens，不重复相加；非 Token 成本包含按次、按图等未拆分费用。</div>`;
}

function renderUsageKPI([label, value, note, color]) {
  return `<article class="usage-kpi">
    <div class="kpi-label">${label}</div>
    <div class="usage-kpi-value ${color}">${value}</div>
    ${note ? `<div class="kpi-note">${note}</div>` : ''}
  </article>`;
}

function renderUsageCards(sourceItems, emptyText, kind) {
  const container = $('#usageEntityCardList');
  const items = (Array.isArray(sourceItems) ? sourceItems : [])
    .slice()
    .sort((left, right) => String(left.name || '').trim().localeCompare(String(right.name || '').trim(), 'zh-CN')
      || String(left.key).localeCompare(String(right.key), 'zh-CN'));
  if (!items.length) {
    container.innerHTML = '<div class="empty-state usage-entity-empty">' + emptyText + '</div>';
    return;
  }
  container.innerHTML = items.map((item) => renderUsageCard(item, kind)).join('');
}

function renderUsageCard(item, kind) {
  const inputTokens = nonNegativeNumber(item?.input_tokens);
  const outputTokens = nonNegativeNumber(item?.output_tokens);
  const cacheCreationTokens = nonNegativeNumber(item?.cache_creation_tokens);
  const cacheReadTokens = nonNegativeNumber(item?.cache_read_tokens);
  const denominator = inputTokens + cacheReadTokens;
  const hitRate = Math.min(100, nonNegativeNumber(item?.cache_hit_rate));
  const unitCost = unitCostValue(item);
  const name = item?.name || '未命名';
  const platform = item?.platform && item.platform !== 'unknown' ? item.platform : '未知平台';
  const totalTokens = nonNegativeNumber(item?.total_tokens);
  const requests = nonNegativeNumber(item?.requests);
  const baseCost = nonNegativeNumber(item?.base_cost);
  const totalCost = nonNegativeNumber(item?.total_cost);
  const nonTokenCost = nonNegativeNumber(item?.non_token_cost);
  const kindLabel = kind === 'group' ? '分组' : '账户';
  const kindClass = kind === 'group' ? ' group-usage-card' : '';
  const context = item?.context || '';
  return `<article class="usage-entity-card${kindClass}">
    <div class="usage-card-head">
      <div class="usage-card-title">
        <span>${escapeHTML(kindLabel)}</span>
        <h4 title="${escapeHTML(name)}">${escapeHTML(name)}</h4>
        ${context ? `<small>${escapeHTML(context)}</small>` : ''}
      </div>
      <span class="usage-platform">${escapeHTML(platform)}</span>
    </div>
    <div class="usage-card-primary">
      <div class="usage-card-primary-metric cache-metric"><span>缓存命中率</span><strong>${hitRate.toFixed(2)}%</strong></div>
      <div class="usage-card-primary-metric unit-cost-metric"><span>每百万 Tokens</span><strong>${formatUnitCost(unitCost)}</strong></div>
    </div>
    <div class="usage-card-details">
      <div><span>输入 Tokens</span><strong>${formatTokens(inputTokens)}</strong></div>
      <div><span>输出 Tokens</span><strong>${formatTokens(outputTokens)}</strong></div>
      <div><span>Cache Create</span><strong>${formatTokens(cacheCreationTokens)}</strong></div>
      <div><span>Cache Read</span><strong>${formatTokens(cacheReadTokens)}</strong></div>
      <div><span>总 Tokens</span><strong>${formatTokens(totalTokens)}</strong></div>
      <div><span>原始成本</span><strong>${formatUSD(baseCost)}</strong></div>
      <div><span>实际成本</span><strong>${formatUSD(totalCost)}</strong></div>
      <div><span>非 Token 成本</span><strong>${formatUSD(nonTokenCost)}</strong></div>
      <div><span>有效倍率</span><strong>${formatMultiplier(item?.effective_rate_multiplier)}</strong></div>
    </div>
    <div class="usage-card-foot">
      <span>${formatCount(requests)} 次请求</span>
      <span>缓存分母 ${formatTokens(denominator)} · ${formatTokens(totalTokens)} Tokens</span>
    </div>
  </article>`;
}

function renderShareDonut(containerId, sourceItems, summary, emptyText, itemLabel, metric) {
  const container = $(`#${containerId}`);
  const sliceTotal = shareSliceTotal(summary, metric);
  const total = shareMetricValue(summary, metric);
  const items = shareItems(sourceItems, summary, metric);
  if (!items.length) {
    container.innerHTML = `<div class="empty-state donut-empty">${emptyText}</div>`;
    return;
  }
  let offset = 0;
  const tooltipId = `${containerId}Tooltip`;
  const slices = items.map((item, index) => {
    const percent = item.sliceValue * 100 / sliceTotal;
    const slice = renderDonutSlice(item, index, percent, offset, sliceTotal, metric);
    offset += percent;
    return slice;
  });
  const totalLabel = `${itemLabel}按 ${shareMetricLabel(metric)} 分布，共 ${formatShareDetail(total, metric)}`;
  container.innerHTML = `
    <div class="donut" role="img" aria-label="${escapeHTML(totalLabel)}">
      <svg class="donut-svg" viewBox="0 0 100 100" aria-hidden="true" focusable="false">${slices.join('')}</svg>
      <div class="donut-hole"><strong>${formatShareValue(total, metric)}</strong><span>${shareMetricLabel(metric)}</span></div>
    </div>
    <div class="donut-legend">${items.map((item, index) => renderLegendItem(item, sliceTotal, index, tooltipId, metric)).join('')}</div>
    <div class="chart-tooltip" id="${tooltipId}" role="tooltip" hidden></div>`;
  bindChartTooltip(container, '[data-chart-tooltip]', $(`#${tooltipId}`));
}

function shareItems(sourceItems, summary, metric) {
  const sliceTotal = shareSliceTotal(summary, metric);
  if (sliceTotal <= 0) return [];
  const ranked = (Array.isArray(sourceItems) ? sourceItems : [])
    .slice()
    .filter((item) => shareSliceValue(item, metric) > 0)
    .sort((left, right) => shareMetricValue(right, metric) - shareMetricValue(left, metric)
      || String(left.name || left.key || '').localeCompare(String(right.name || right.key || ''), 'zh-CN'))
    .map((item) => ({
      name: item.name || item.key || '未命名',
      value: shareMetricValue(item, metric),
      sliceValue: shareSliceValue(item, metric),
      context: item?.context || ''
    }));
  const minimumSliceValue = sliceTotal * minimumDonutPercent / 100;
  const items = ranked.slice(0, 7).filter((item) => item.sliceValue >= minimumSliceValue);
  const shown = items.reduce((sum, item) => sum + item.sliceValue, 0);
  const remainder = Math.max(sliceTotal - shown, 0);
  if (remainder > 0) {
    items.push({
      name: '其他',
      value: remainder,
      sliceValue: remainder,
      context: '未展开的其余对象'
    });
  }
  return items;
}

function renderDonutSlice(item, index, percent, offset, total, metric) {
  const label = shareTooltipLabel(item, total, metric);
  const color = donutColors[index % donutColors.length];
  return `<path class="donut-slice" d="${donutSlicePath(percent, offset)}"
    fill="${color}" fill-rule="evenodd"
    data-chart-tooltip="${escapeHTML(label)}">
    <title>${escapeHTML(label)}</title>
  </path>`;
}

function donutSlicePath(percent, offset) {
  const safePercent = Math.min(100, Math.max(0, Number(percent) || 0));
  const safeOffset = Number(offset) || 0;
  const center = 50;
  const outerRadius = 48;
  const innerRadius = 30;
  const startAngle = (safeOffset / 100) * Math.PI * 2 - Math.PI / 2;
  const endAngle = ((safeOffset + safePercent) / 100) * Math.PI * 2 - Math.PI / 2;
  const point = (radius, angle) => `${(center + radius * Math.cos(angle)).toFixed(4)} ${(center + radius * Math.sin(angle)).toFixed(4)}`;

  if (safePercent >= 99.999) {
    return `M ${center} ${center - outerRadius}
      A ${outerRadius} ${outerRadius} 0 1 1 ${center} ${center + outerRadius}
      A ${outerRadius} ${outerRadius} 0 1 1 ${center} ${center - outerRadius}
      M ${center} ${center - innerRadius}
      A ${innerRadius} ${innerRadius} 0 1 0 ${center} ${center + innerRadius}
      A ${innerRadius} ${innerRadius} 0 1 0 ${center} ${center - innerRadius} Z`;
  }

  const largeArc = safePercent > 50 ? 1 : 0;
  return `M ${point(outerRadius, startAngle)}
    A ${outerRadius} ${outerRadius} 0 ${largeArc} 1 ${point(outerRadius, endAngle)}
    L ${point(innerRadius, endAngle)}
    A ${innerRadius} ${innerRadius} 0 ${largeArc} 0 ${point(innerRadius, startAngle)} Z`;
}

function renderLegendItem(item, total, index, tooltipId, metric) {
  const percent = total ? item.sliceValue * 100 / total : 0;
  const label = shareTooltipLabel(item, total, metric);
  return `<div class="legend-item" tabindex="0" title="${escapeHTML(label)}" aria-label="${escapeHTML(label)}"
    data-chart-tooltip="${escapeHTML(label)}" aria-describedby="${tooltipId}">
    <i style="background:${donutColors[index % donutColors.length]}"></i>
    <span class="legend-copy"><strong>${escapeHTML(item.name)}</strong>${item.context ? `<small>${escapeHTML(item.context)}</small>` : ''}</span><b>${formatShareValue(item.value, metric)}</b><em>${percent.toFixed(1)}%</em>
  </div>`;
}

function shareMetricValue(item, metric) {
  const field = metric === 'cost' ? 'total_cost' : 'total_tokens';
  return nonNegativeNumber(item?.[field]);
}

function shareSliceTotal(summary, metric) {
  return shareMetricValue(summary, metric);
}

function shareSliceValue(item, metric) {
  return shareMetricValue(item, metric);
}

function shareMetricLabel(metric) {
  if (metric === 'cost') return '成本';
  return 'Tokens';
}

function formatShareValue(value, metric) {
  if (metric === 'tokens') return formatTokens(value);
  const amount = Math.max(0, Number(value || 0));
  if (amount >= 1e6) return `$${(amount / 1e6).toFixed(2)}M`;
  if (amount >= 1e3) return `$${(amount / 1e3).toFixed(2)}K`;
  if (amount >= 100) return `$${amount.toFixed(0)}`;
  if (amount >= 1) return `$${amount.toFixed(2)}`;
  return formatUSD(amount);
}

function formatShareDetail(value, metric) {
  if (metric === 'cost') return formatUSD(value);
  return `${formatTokens(value)} Tokens`;
}

function shareTooltipLabel(item, total, metric) {
  const percent = total ? item.sliceValue * 100 / total : 0;
  const share = `${percent.toFixed(1)}%`;
  const context = item.context ? ` · ${item.context}` : '';
  return `${item.name}${context} · ${formatShareDetail(item.value, metric)} · ${share}`;
}

function bindChartTooltip(container, selector, tooltip) {
  if (!tooltip) return;
  container.querySelectorAll(selector).forEach((target) => {
    target.addEventListener('pointerenter', (event) => showChartTooltip(tooltip, target, event.clientX, event.clientY));
    target.addEventListener('pointermove', (event) => positionChartTooltip(tooltip, event.clientX, event.clientY));
    target.addEventListener('pointerleave', () => hideChartTooltip(tooltip));
    target.addEventListener('focus', () => {
      const rect = target.getBoundingClientRect();
      showChartTooltip(tooltip, target, rect.right, rect.top);
    });
    target.addEventListener('blur', () => hideChartTooltip(tooltip));
  });
}

function showChartTooltip(tooltip, target, x, y) {
  tooltip.textContent = target.dataset.chartTooltip || '';
  tooltip.hidden = false;
  positionChartTooltip(tooltip, x, y);
}

function positionChartTooltip(tooltip, x, y) {
  if (tooltip.hidden) return;
  const margin = 12;
  const gap = 14;
  const rect = tooltip.getBoundingClientRect();
  const maxLeft = Math.max(margin, window.innerWidth - rect.width - margin);
  const maxTop = Math.max(margin, window.innerHeight - rect.height - margin);
  const left = Math.min(Math.max(margin, x + gap), maxLeft);
  const top = Math.min(Math.max(margin, y + gap), maxTop);
  tooltip.style.left = `${left}px`;
  tooltip.style.top = `${top}px`;
}

function hideChartTooltip(tooltip) {
  tooltip.hidden = true;
}

function renderUsageBars(timeline, bucket, metric, period) {
  const limit = { '24h': 24, '7d': 7, '15d': 15, '30d': 30 }[period];
  const points = limit ? timeline.slice(-limit) : timeline;
  const chart = $('#usageBarChart');
  const tooltip = $('#usageTrendTooltip');
  const channels = usageChannels(points);
  const colors = new Map(channels.map((name, index) => [name, channelColor(index)]));
  const values = metric === 'unit_cost'
    ? points.flatMap((item) => pointChannels(item).map((channel) => trendValue(channel, metric)))
    : points.map((item) => trendValue(item, metric));
  $('#usageTrendBucket').textContent = trendBucketLabel(bucket);
  chart.style.setProperty('--point-count', String(Math.max(points.length, 1)));
  chart.style.setProperty('--point-width', `${metric === 'unit_cost' ? Math.max(42, channels.length * 12 + 14) : 42}px`);
  chart.classList.toggle('is-empty', !points.length || values.every((value) => value <= 0));
  chart.classList.toggle('is-grouped', metric === 'unit_cost');
  hideChartTooltip(tooltip);
  renderChannelLegend(channels, colors);
  if (chart.classList.contains('is-empty')) {
    $('#usageYAxis').innerHTML = '';
    chart.innerHTML = `<div class="empty-state">该时间范围没有${trendMetricLabel(metric)}数据</div>`;
    return;
  }
  const floor = metric === 'tokens' ? 4 : 0.0004;
  const maximum = niceMaximum(Math.max(...values, floor));
  renderTrendAxis(maximum, metric);
  chart.innerHTML = points.map((item) => renderUsageBar(item, bucket, maximum, metric, channels, colors)).join('');
  bindChartTooltip($('#usageTrendChart'), '#usageBarChart [data-chart-tooltip]', tooltip);
  requestAnimationFrame(() => {
    const scroller = chart.closest('.bar-chart-wrap');
    scroller.scrollLeft = scroller.scrollWidth - scroller.clientWidth;
  });
}

function trendValue(item, metric) {
  if (metric === 'unit_cost') return unitCostValue(item);
  const value = metric === 'cost' ? item?.total_cost : item?.total_tokens;
  return nonNegativeNumber(value);
}

function niceMaximum(value) {
  const exponent = 10 ** Math.floor(Math.log10(value));
  const fraction = value / exponent;
  const factor = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10;
  return factor * exponent;
}

function renderTrendAxis(maximum, metric) {
  const ticks = Array.from({ length: 5 }, (_, index) => maximum * (4 - index) / 4);
  $('#usageYAxis').innerHTML = ticks.map((value) => `<span>${formatTrendTick(value, metric)}</span>`).join('');
}

function renderUsageBar(item, bucket, maximum, metric, channelNames, colors) {
  const value = trendValue(item, metric);
  const labels = bucketLabels(item.start_at, bucket);
  const channels = pointChannels(item);
  const details = channels
    .filter((channel) => trendValue(channel, metric) > 0)
    .map((channel) => `${channel.name}: ${formatTrendValue(trendValue(channel, metric), metric)}`);
  const tooltip = [`${labels.full} · ${formatTrendValue(value, metric)} · ${formatCount(item.requests)} 次请求`, ...details].join('\n');
  const bars = metric === 'unit_cost'
    ? renderGroupedChannels(channels, channelNames, colors, maximum, labels.full, metric)
    : renderStackedChannels(channels, colors, maximum, labels.full, metric);
  return `<div class="bar-column" tabindex="0" aria-label="${escapeHTML(tooltip)}"
      aria-describedby="usageTrendTooltip" data-chart-tooltip="${escapeHTML(tooltip)}">
    <div class="bar-track">${bars}</div>
    <span>${escapeHTML(labels.short)}</span>
  </div>`;
}

function renderStackedChannels(channels, colors, maximum, bucketLabel, metric) {
  const total = channels.reduce((sum, channel) => sum + trendValue(channel, metric), 0);
  const height = total > 0 ? Math.max(3, total * 100 / maximum) : 0;
  const segments = channels
    .filter((channel) => trendValue(channel, metric) > 0)
    .map((channel) => {
      const value = trendValue(channel, metric);
      const label = `${bucketLabel} · ${channel.name} · ${formatTrendValue(value, metric)} · ${formatCount(channel.requests)} 次请求`;
      return `<i class="channel-bar-segment" style="background:${colors.get(channel.name)};flex-grow:${value}"
        data-chart-tooltip="${escapeHTML(label)}" aria-label="${escapeHTML(label)}"></i>`;
    }).join('');
  return `<div class="stacked-bar" style="height:${height}%">${segments}</div>`;
}

function renderGroupedChannels(channels, channelNames, colors, maximum, bucketLabel, metric) {
  const byName = new Map(channels.map((channel) => [channel.name, channel]));
  const bars = channelNames.map((name) => {
    const channel = byName.get(name);
    const value = trendValue(channel, metric);
    const height = value > 0 ? Math.max(3, value * 100 / maximum) : 0;
    if (!channel || value <= 0) return '<i class="channel-unit-bar" aria-hidden="true"></i>';
    const label = `${bucketLabel} · ${name} · ${formatTrendValue(value, metric)} · ${formatCount(channel.requests)} 次请求`;
    return `<i class="channel-unit-bar" style="height:${height}%;background:${colors.get(name)}"
      data-chart-tooltip="${escapeHTML(label)}" aria-label="${escapeHTML(label)}"></i>`;
  }).join('');
  return `<div class="grouped-bar">${bars}</div>`;
}

function usageChannels(points) {
  const totals = new Map();
  points.forEach((item) => pointChannels(item).forEach((channel) => {
    totals.set(channel.name, (totals.get(channel.name) || 0) + nonNegativeNumber(channel.total_tokens));
  }));
  return [...totals.keys()].sort((left, right) => totals.get(right) - totals.get(left)
    || left.localeCompare(right, 'zh-CN'));
}

function pointChannels(item) {
  if (Array.isArray(item?.channels) && item.channels.length) {
    return item.channels.map((channel) => ({ ...channel, name: channel.name || '未归属渠道' }));
  }
  if (trendValue(item, 'tokens') <= 0 && trendValue(item, 'cost') <= 0) return [];
  return [{
    name: '全部渠道', requests: item?.requests,
    total_tokens: item?.total_tokens, total_cost: item?.total_cost,
    base_cost: item?.base_cost,
    input_tokens: item?.input_tokens, output_tokens: item?.output_tokens,
    cache_creation_tokens: item?.cache_creation_tokens, cache_read_tokens: item?.cache_read_tokens,
    input_cost: item?.input_cost, output_cost: item?.output_cost,
    cache_creation_cost: item?.cache_creation_cost, cache_read_cost: item?.cache_read_cost,
    non_token_cost: item?.non_token_cost,
    cost_per_million_tokens: item?.cost_per_million_tokens
  }];
}

function renderChannelLegend(channels, colors) {
  $('#usageChannelLegend').innerHTML = channels.map((name) =>
    `<span><i style="background:${colors.get(name)}"></i>${escapeHTML(name)}</span>`
  ).join('');
}

function channelColor(index) {
  if (index < donutColors.length) return donutColors[index];
  return `hsl(${Math.round(index * 137.508) % 360} 62% 65%)`;
}

function bucketLabels(value, bucket) {
  const date = new Date(value);
  const hasTime = bucket === 'minute' || bucket === 'hour';
  const short = hasTime
    ? `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`
    : `${date.getMonth() + 1}/${date.getDate()}`;
  const full = date.toLocaleString('zh-CN', {
    month: 'numeric', day: 'numeric',
    hour: hasTime ? '2-digit' : undefined,
    minute: hasTime ? '2-digit' : undefined
  });
  return { short, full };
}

function trendBucketLabel(bucket) {
  if (bucket === 'minute') return '按分钟';
  if (bucket === 'day') return '按天';
  return '按小时';
}

function formatTrendTick(value, metric) {
  if (metric === 'tokens') return formatTokens(value);
  if (metric === 'unit_cost') return formatUnitCost(value);
  if (value === 0) return '$0';
  if (value >= 1000) return `$${(value / 1000).toFixed(value >= 10000 ? 0 : 1)}K`;
  if (value >= 100) return `$${value.toFixed(0)}`;
  if (value >= 1) return `$${value.toFixed(value >= 10 ? 0 : 1)}`;
  if (value >= 0.01) return `$${value.toFixed(2)}`;
  return `$${value.toFixed(4)}`;
}

function formatTrendValue(value, metric) {
  if (metric === 'tokens') return `${formatTokens(value)} Tokens`;
  if (metric === 'unit_cost') return `${formatUnitCost(value)} / 1M Tokens`;
  return formatUSD(value);
}

function trendMetricLabel(metric) {
  if (metric === 'tokens') return ' Tokens';
  if (metric === 'unit_cost') return '每百万 Tokens 成本';
  return '成本';
}

function formatUnitCost(value) {
  return formatUSD(nonNegativeNumber(value));
}

function unitCostValue(item) {
  const reported = Number(item?.cost_per_million_tokens);
  return Number.isFinite(reported) ? Math.max(reported, 0) : costPerMillion(item?.total_cost, item?.total_tokens);
}

function costPerMillion(totalCost, totalTokens) {
  const tokens = nonNegativeNumber(totalTokens);
  return tokens > 0 ? nonNegativeNumber(totalCost) * 1_000_000 / tokens : 0;
}

function formatMultiplier(value) {
  if (value == null || (typeof value === 'string' && value.trim() === '')) return '—';
  const multiplier = Number(value);
  return Number.isFinite(multiplier) && multiplier >= 0 ? `${multiplier.toFixed(4)}×` : '—';
}

function nonNegativeNumber(value) {
  const number = Number(value || 0);
  return Number.isFinite(number) ? Math.max(number, 0) : 0;
}

function isolateChartWheel(event) {
  const container = event.currentTarget;
  const delta = Math.abs(event.deltaX) > Math.abs(event.deltaY) ? event.deltaX : event.deltaY;
  if (!delta) return;
  if (container.scrollWidth <= container.clientWidth) return;
  event.preventDefault();
  container.scrollLeft = Math.max(0, Math.min(container.scrollWidth - container.clientWidth, container.scrollLeft + delta));
}
