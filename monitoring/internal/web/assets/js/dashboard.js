import {
  $,
  LatestRequest,
  api,
  availabilityClass,
  escapeHTML,
  formatMedianMs,
  formatMs,
  formatPct,
  formatTime,
  normalizeStatus,
  statusClass,
  statusLabel,
  toast
} from './shared.js';

export class DashboardPanel {
  dashboard = null;
  filter = 'group';
  windowDays = 1;
  #requests = new LatestRequest();
  #openHistory;
  #nextProbeAt = 0;
  #intervalSeconds = 0;
  #probeRunning = false;
  #countdownRefreshRequested = false;

  constructor(openHistory) {
    this.#openHistory = openHistory;
    window.setInterval(() => this.#renderProbeCountdown(), 1000);
  }

  setFilter(filter) {
    this.filter = filter;
    this.render();
  }

  setWindow(days) {
    this.windowDays = days;
  }

  async load() {
    const requestId = this.#requests.begin();
    try {
      const dashboard = await api(`/api/v1/monitor/dashboard?window=${this.windowDays}`);
      if (!this.#requests.isCurrent(requestId)) return;
      this.dashboard = dashboard;
      this.render();
    } catch (error) {
      if (!this.#requests.isCurrent(requestId)) return;
      $('#targetGrid').innerHTML = `<div class="empty-state">${escapeHTML(error.message)}</div>`;
      toast(error.message);
    }
  }

  render() {
    if (!this.dashboard) return;
    this.#renderOverview();
    const targets = this.#visibleTargets();
    $('#sectionMeta').textContent = `${targets.length} / ${(this.dashboard.targets || []).length} 个对象`;
    $('#targetGrid').innerHTML = targets.length
      ? targets.map((target) => this.#renderTarget(target)).join('')
      : '<div class="empty-state">当前筛选没有对象</div>';
    this.#bindTargetCards();
  }

  #renderOverview() {
    const summary = this.dashboard.summary || {};
    $('#overallTitle').textContent = summary.targets ? '服务状态' : '等待探测数据';
    $('#overallMeta').textContent = `${summary.targets || 0} 个对象 · 最近更新 ${formatTime(this.dashboard.generated_at)}`;
    this.#probeRunning = Boolean(this.dashboard.probe_running);
    this.#intervalSeconds = Math.max(0, Number(this.dashboard.interval_seconds || 0));
    const nextProbeAt = new Date(this.dashboard.next_probe_at || '');
    const nextProbeValue = Number.isNaN(nextProbeAt.getTime()) ? 0 : nextProbeAt.getTime();
    if (nextProbeValue !== this.#nextProbeAt) this.#countdownRefreshRequested = false;
    this.#nextProbeAt = nextProbeValue;
    this.#renderProbeCountdown();
  }

  #renderProbeCountdown() {
    const status = $('#probeStatus');
    status.classList.toggle('is-running', this.#probeRunning);
    if (this.#probeRunning) {
      $('#probeCountdown').textContent = '···';
      $('#probeStatusText').textContent = '正在探测';
      $('#nextProbeText').textContent = '本轮进行中';
      return;
    }
    if (!this.#nextProbeAt || !this.#intervalSeconds) {
      status.style.setProperty('--cooldown-progress', '0turn');
      $('#probeCountdown').textContent = '--';
      $('#probeStatusText').textContent = '等待调度';
      $('#nextProbeText').textContent = '正在同步周期';
      return;
    }
    const remaining = Math.max(0, Math.ceil((this.#nextProbeAt - Date.now()) / 1000));
    const progress = Math.min(1, remaining / this.#intervalSeconds);
    status.style.setProperty('--cooldown-progress', `${progress}turn`);
    $('#probeCountdown').textContent = formatCountdown(remaining);
    $('#probeStatusText').textContent = remaining ? '下次探测' : '即将探测';
    $('#nextProbeText').textContent = remaining ? `${remaining} 秒后` : '等待本轮开始';
    if (!remaining && !this.#countdownRefreshRequested) {
      this.#countdownRefreshRequested = true;
      window.setTimeout(() => void this.load(), 800);
    }
  }

  #visibleTargets() {
    return (this.dashboard.targets || [])
      .filter((target) => target.kind === this.filter)
      .sort((left, right) => {
        return String(left.name || '').trim().localeCompare(String(right.name || '').trim(), 'zh-CN')
          || String(left.key).localeCompare(String(right.key), 'zh-CN');
      });
  }

  #renderTarget(item) {
    const stats = item.stats || {};
    const latency = stats.latency || {};
    const status = normalizeStatus(item.status);
    const source = sourceLabel(item.latest_source);
    const samples = Number(stats.samples || 0);
    const hasSamples = samples > 0;
    const availability = hasSamples ? Math.max(0, Math.min(100, Number(stats.availability || 0))) : 0;
    const availabilityLabel = hasSamples ? `${samples} 次样本` : '暂无样本';
    const availabilityValue = hasSamples ? formatPct(stats.availability) : '—';
    const availabilityTone = hasSamples ? availabilityClass(stats.availability) : 'neutral';
    return `
      <article class="target-card target-${status}" data-target="${escapeHTML(item.key)}" data-name="${escapeHTML(item.name)}"
        role="button" tabindex="0" aria-label="查看 ${escapeHTML(item.name)} 的历史记录">
        <div class="target-head">
          <div>
            <div class="target-kind">${item.kind === 'group' ? 'GROUP' : 'ACCOUNT'}</div>
            <div class="target-name" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</div>
            <div class="target-platform">${escapeHTML(item.platform || 'mixed')}${item.stale ? '<span class="stale-label">● 数据滞后</span>' : ''}</div>
          </div>
          <span class="status-badge ${statusClass(status)}">${statusLabel(status)}</span>
        </div>
        <div class="availability">
          <span class="availability-label">${this.windowDays} 天可用率 · ${availabilityLabel}</span>
          <strong class="availability-value ${availabilityTone}">${availabilityValue}</strong>
        </div>
        <div class="metrics">
          ${renderMetric('首字/首字节', formatMs(item.latest_first_byte_ms))}
          ${renderMetric('最快', formatMs(latency.fastest_ms))}
          ${renderMetric('中位数', formatMedianMs(latency))}
          ${renderMetric('P95', formatMs(latency.p95_ms), '95% 的成功样本耗时不超过该值')}
        </div>
        <div class="card-foot">
          ${renderStatusHistory(item.recent_samples || [], this.windowDays)}
          <span>${source} · ${formatTime(item.last_checked_at)}</span>
        </div>
      </article>`;
  }

  #bindTargetCards() {
    document.querySelectorAll('.target-card').forEach((card) => {
      const open = () => this.#openHistory(card.dataset.target, card.dataset.name);
      card.addEventListener('click', open);
      card.addEventListener('keydown', (event) => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        open();
      });
    });
  }
}

function formatCountdown(seconds) {
  if (seconds < 60) return String(seconds);
  const minutes = Math.floor(seconds / 60);
  return `${minutes}:${String(seconds % 60).padStart(2, '0')}`;
}

function renderStatusHistory(samples, windowDays) {
  const recent = samples.slice(-24);
  const empty = Array.from({ length: 24 - recent.length }, () => '<i aria-hidden="true"></i>');
  const items = recent.map((sample) => {
    const successful = sample.status === 'operational' || sample.status === 'degraded';
    const failed = sample.status === 'failed' || sample.status === 'error';
    const label = failed || successful
      ? `${formatTime(sample.checked_at)} · ${statusLabel(sample.status)} · ${sourceLabel(sample.source)}`
      : `${formatTime(sample.checked_at)} · 暂无采样`;
    const tone = successful ? 'ok' : failed ? 'bad' : '';
    return `<i${tone ? ` class="${tone}"` : ''} role="img" aria-label="${escapeHTML(label)}" title="${escapeHTML(label)}"></i>`;
  });
  return `<div class="status-history" aria-label="${windowDays} 天内 24 段状态轨迹">${empty.concat(items).join('')}</div>`;
}

function renderMetric(label, value, help = '') {
  const title = help ? ` title="${escapeHTML(help)}"` : '';
  return `<div><div class="metric-label"${title}>${label}</div><div class="metric-value">${value}</div></div>`;
}

function sourceLabel(source) {
  if (source === 'history') return '真实请求';
  if (source === 'aggregate') return '分组聚合';
  return source ? '主动探测' : '暂无来源';
}
