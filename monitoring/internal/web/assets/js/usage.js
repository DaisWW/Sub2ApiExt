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

export class UsagePanel {
  usage = null;
  period = '24h';
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
  }

  setPeriod(period) {
    this.period = period;
  }

  setTrendMetric(metric, button) {
    if (metric !== 'tokens' && metric !== 'cost') return;
    this.trendMetric = metric;
    activateToggle('[data-trend-metric]', button);
    this.#renderTrend();
  }

  setShareMetric(kind, metric, button) {
    if (!(kind in this.shareMetrics) || (metric !== 'tokens' && metric !== 'cost')) return;
    this.shareMetrics[kind] = metric;
    activateToggle(`[data-share-kind="${kind}"][data-share-metric]`, button);
    this.#renderShares();
  }

  async load({ silent = false } = {}) {
    const requestId = this.#requests.begin();
    if (!silent) setLoading(true);
    try {
      const usage = await api(`/api/v1/monitor/usage-ranking?period=${encodeURIComponent(this.period)}&limit=10`);
      if (!this.#requests.isCurrent(requestId)) return;
      this.usage = usage;
      this.render();
    } catch (error) {
      if (!this.#requests.isCurrent(requestId)) return;
      $('#usageKpiGrid').innerHTML = `<div class="empty-state usage-loading">${escapeHTML(error.message)}</div>`;
      $('#usageMeta').textContent = '用量读取失败';
      toast(error.message);
    } finally {
      if (this.#requests.isCurrent(requestId)) setLoading(false);
    }
  }

  render() {
    if (!this.usage) return;
    const summary = this.usage.summary || {};
    $('#usageMeta').textContent = `${this.usage.period_label || '用量窗口'} · ${formatTime(this.usage.generated_at)} 更新`;
    $('#usageKpiGrid').innerHTML = usageKPIs(summary).map(renderUsageKPI).join('');
    renderCacheRates('accountCacheRateList', this.usage.accounts, '暂无账户缓存数据');
    renderCacheRates('groupCacheRateList', this.usage.groups, '暂无分组缓存数据');
    this.#renderShares();
    this.#renderTrend();
  }

  #renderShares() {
    if (!this.usage) return;
    const summary = this.usage.summary || {};
    const modelMetric = this.shareMetrics.model;
    const groupMetric = this.shareMetrics.group;
    renderShareDonut('modelDonutLayout', this.usage.models || [], shareMetricValue(summary, modelMetric), '暂无模型用量', '模型', modelMetric);
    renderShareDonut('groupDonutLayout', this.usage.groups || [], shareMetricValue(summary, groupMetric), '暂无分组用量', '分组', groupMetric);
  }

  #renderTrend() {
    if (!this.usage) return;
    renderUsageBars(this.usage.timeline || [], this.usage.bucket, this.trendMetric, this.usage.period);
  }
}

function setLoading(loading) {
  $('#usageSection').classList.toggle('is-loading', loading);
  $('#usageSection').setAttribute('aria-busy', String(loading));
  if (!loading) return;
  $('#usageKpiGrid').innerHTML = '<div class="loading-state usage-loading">读取用量数据中...</div>';
  $('#accountCacheRateList').innerHTML = '<div class="loading-state cache-rate-loading">读取缓存数据中...</div>';
  $('#groupCacheRateList').innerHTML = '<div class="loading-state cache-rate-loading">读取缓存数据中...</div>';
  $('#usageMeta').textContent = '正在更新用量窗口…';
}

function usageKPIs(summary) {
  return [
    ['总 Tokens', formatTokens(summary.total_tokens), '', ''],
    ['输入 Tokens', formatTokens(summary.input_tokens), '', ''],
    ['输出 Tokens', formatTokens(summary.output_tokens), '', ''],
    ['请求数', formatCount(summary.requests), '', ''],
    ['实际消耗', formatUSD(summary.total_cost), '', '']
  ];
}

function renderUsageKPI([label, value, note, color]) {
  return `<article class="usage-kpi">
    <div class="kpi-label">${label}</div>
    <div class="usage-kpi-value ${color}">${value}</div>
    ${note ? `<div class="kpi-note">${note}</div>` : ''}
  </article>`;
}

function renderCacheRates(containerId, sourceItems, emptyText) {
  const container = $('#' + containerId);
  const items = Array.isArray(sourceItems) ? sourceItems : [];
  if (!items.length) {
    container.innerHTML = '<div class="empty-state cache-rate-empty">' + emptyText + '</div>';
    return;
  }
  container.innerHTML = items.map(renderCacheRate).join('');
}

function renderCacheRate(item) {
  const inputTokens = Math.max(0, Number(item?.input_tokens || 0));
  const cacheReadTokens = Math.max(0, Number(item?.cache_read_tokens || 0));
  const denominator = inputTokens + cacheReadTokens;
  const hitRate = Math.min(100, Math.max(0, Number(item?.cache_hit_rate || 0)));
  const name = item?.name || '未命名';
  return '<div class="cache-rate-row">' +
    '<div class="cache-rate-name" title="' + escapeHTML(name) + '">' + escapeHTML(name) + '</div>' +
    '<div class="cache-rate-stat cache-rate-hit"><span>命中率</span><strong>' + hitRate.toFixed(2) + '%</strong></div>' +
    '<div class="cache-rate-stat"><span>Cache Read</span><strong>' + formatTokens(cacheReadTokens) + '</strong></div>' +
    '<div class="cache-rate-stat"><span>分母（输入 + 读取）</span><strong>' + formatTokens(denominator) + '</strong></div>' +
  '</div>';
}

function renderShareDonut(containerId, sourceItems, total, emptyText, itemLabel, metric) {
  const safeTotal = Math.max(Number(total || 0), 0);
  const container = $(`#${containerId}`);
  const items = shareItems(sourceItems, safeTotal, metric);
  if (!items.length) {
    container.innerHTML = `<div class="empty-state donut-empty">${emptyText}</div>`;
    return;
  }
  let offset = 0;
  const tooltipId = `${containerId}Tooltip`;
  const slices = items.map((item, index) => {
    const percent = item.value * 100 / safeTotal;
    const slice = renderDonutSlice(item, index, percent, offset, safeTotal, metric);
    offset += percent;
    return slice;
  });
  const totalLabel = `${itemLabel}占比，共 ${formatShareDetail(safeTotal, metric)}`;
  container.innerHTML = `
    <div class="donut" role="img" aria-label="${escapeHTML(totalLabel)}">
      <svg class="donut-svg" viewBox="0 0 100 100" aria-hidden="true" focusable="false">${slices.join('')}</svg>
      <div class="donut-hole"><strong>${formatShareValue(safeTotal, metric)}</strong><span>${shareMetricLabel(metric)}</span></div>
    </div>
    <div class="donut-legend">${items.map((item, index) => renderLegendItem(item, safeTotal, index, tooltipId, metric)).join('')}</div>
    <div class="chart-tooltip" id="${tooltipId}" role="tooltip" hidden></div>`;
  bindChartTooltip(container, '[data-chart-tooltip]', $(`#${tooltipId}`));
}

function shareItems(sourceItems, total, metric) {
  if (total <= 0) return [];
  const ranked = sourceItems
    .slice()
    .sort((left, right) => shareMetricValue(right, metric) - shareMetricValue(left, metric)
      || String(left.name || left.key || '').localeCompare(String(right.name || right.key || ''), 'zh-CN'))
    .map((item) => ({
      name: item.name || item.key || '未命名',
      value: shareMetricValue(item, metric)
    }));
  const items = ranked.slice(0, 7);
  const shown = items.reduce((sum, item) => sum + item.value, 0);
  const remainder = Math.max(total - shown, 0);
  if (remainder > 0) items.push({ name: '其他', value: remainder });
  return items.filter((item) => item.value > 0);
}

function renderDonutSlice(item, index, percent, offset, total, metric) {
  const label = shareTooltipLabel(item, total, metric);
  return `<circle class="donut-slice" cx="50" cy="50" r="39" pathLength="100"
    stroke="${donutColors[index % donutColors.length]}" stroke-dasharray="${percent} ${100 - percent}"
    stroke-dashoffset="${-offset}" transform="rotate(-90 50 50)"
    data-chart-tooltip="${escapeHTML(label)}">
    <title>${escapeHTML(label)}</title>
  </circle>`;
}

function renderLegendItem(item, total, index, tooltipId, metric) {
  const percent = total ? item.value * 100 / total : 0;
  const label = shareTooltipLabel(item, total, metric);
  return `<div class="legend-item" tabindex="0" title="${escapeHTML(label)}" aria-label="${escapeHTML(label)}"
    data-chart-tooltip="${escapeHTML(label)}" aria-describedby="${tooltipId}">
    <i style="background:${donutColors[index % donutColors.length]}"></i>
    <span>${escapeHTML(item.name)}</span><strong>${formatShareValue(item.value, metric)}</strong><em>${percent.toFixed(1)}%</em>
  </div>`;
}

function shareMetricValue(item, metric) {
  const field = metric === 'cost' ? 'total_cost' : 'total_tokens';
  return Math.max(0, Number(item?.[field] || 0));
}

function shareMetricLabel(metric) {
  return metric === 'cost' ? '成本' : 'Tokens';
}

function formatShareValue(value, metric) {
  if (metric !== 'cost') return formatTokens(value);
  const amount = Math.max(0, Number(value || 0));
  if (amount >= 1e6) return `$${(amount / 1e6).toFixed(2)}M`;
  if (amount >= 1e3) return `$${(amount / 1e3).toFixed(2)}K`;
  if (amount >= 100) return `$${amount.toFixed(0)}`;
  if (amount >= 1) return `$${amount.toFixed(2)}`;
  return formatUSD(amount);
}

function formatShareDetail(value, metric) {
  return metric === 'cost' ? formatUSD(value) : `${formatTokens(value)} Tokens`;
}

function shareTooltipLabel(item, total, metric) {
  const percent = total ? item.value * 100 / total : 0;
  return `${item.name} · ${formatShareDetail(item.value, metric)} · ${percent.toFixed(1)}%`;
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
  const values = points.map((item) => trendValue(item, metric));
  $('#usageTrendBucket').textContent = bucket === 'day' ? '按天' : '按小时';
  chart.style.setProperty('--point-count', String(Math.max(points.length, 1)));
  chart.classList.toggle('is-empty', !points.length || values.every((value) => value <= 0));
  hideChartTooltip(tooltip);
  if (chart.classList.contains('is-empty')) {
    $('#usageYAxis').innerHTML = '';
    chart.innerHTML = `<div class="empty-state">该时间范围没有${metric === 'cost' ? '成本' : ' Tokens'}数据</div>`;
    return;
  }
  const floor = metric === 'tokens' ? 4 : 0.0004;
  const maximum = niceMaximum(Math.max(...values, floor));
  renderTrendAxis(maximum, metric);
  chart.innerHTML = points.map((item) => renderUsageBar(item, bucket, maximum, metric)).join('');
  bindChartTooltip($('#usageTrendChart'), '#usageBarChart [data-chart-tooltip]', tooltip);
  requestAnimationFrame(() => {
    const scroller = chart.closest('.bar-chart-wrap');
    scroller.scrollLeft = scroller.scrollWidth - scroller.clientWidth;
  });
}

function trendValue(item, metric) {
  const value = metric === 'cost' ? item.total_cost : item.total_tokens;
  return Math.max(0, Number(value || 0));
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

function renderUsageBar(item, bucket, maximum, metric) {
  const value = trendValue(item, metric);
  const height = value > 0 ? Math.max(3, value * 100 / maximum) : 0;
  const labels = bucketLabels(item.start_at, bucket);
  const valueLabel = metric === 'cost' ? formatUSD(value) : `${formatTokens(value)} Tokens`;
  const tooltip = `${labels.full} · ${valueLabel} · ${formatCount(item.requests)} 次请求`;
  return `<div class="bar-column" tabindex="0" aria-label="${escapeHTML(tooltip)}"
      aria-describedby="usageTrendTooltip" data-chart-tooltip="${escapeHTML(tooltip)}">
    <div class="bar-track"><i class="bar-fill" style="height:${height}%"></i></div>
    <span>${escapeHTML(labels.short)}</span>
  </div>`;
}

function bucketLabels(value, bucket) {
  const date = new Date(value);
  const short = bucket === 'hour'
    ? `${String(date.getHours()).padStart(2, '0')}:00`
    : `${date.getMonth() + 1}/${date.getDate()}`;
  const full = date.toLocaleString('zh-CN', {
    month: 'numeric', day: 'numeric',
    hour: bucket === 'hour' ? '2-digit' : undefined,
    minute: bucket === 'hour' ? '2-digit' : undefined
  });
  return { short, full };
}

function formatTrendTick(value, metric) {
  if (metric === 'tokens') return formatTokens(value);
  if (value === 0) return '$0';
  if (value >= 1000) return `$${(value / 1000).toFixed(value >= 10000 ? 0 : 1)}K`;
  if (value >= 100) return `$${value.toFixed(0)}`;
  if (value >= 1) return `$${value.toFixed(value >= 10 ? 0 : 1)}`;
  if (value >= 0.01) return `$${value.toFixed(2)}`;
  return `$${value.toFixed(4)}`;
}

function isolateChartWheel(event) {
  const container = event.currentTarget;
  const delta = Math.abs(event.deltaX) > Math.abs(event.deltaY) ? event.deltaX : event.deltaY;
  if (!delta) return;
  if (container.scrollWidth <= container.clientWidth) return;
  event.preventDefault();
  container.scrollLeft = Math.max(0, Math.min(container.scrollWidth - container.clientWidth, container.scrollLeft + delta));
}
