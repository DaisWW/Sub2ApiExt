import {
  $,
  LatestRequest,
  api,
  availabilityClass,
  escapeHTML,
  formatCount,
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
  #requests = new LatestRequest();
  #activityRequests = new LatestRequest();
  #openHistory;
  #activeUsers = new Map();
  #activityLoaded = false;
  #activityWindowSeconds = 300;
  #nextProbeAt = 0;
  #intervalSeconds = 0;
  #probeRunning = false;
  #countdownRefreshRequested = false;
  #countdownState = '';
  #countdownSeconds = null;

  constructor(openHistory) {
    this.#openHistory = openHistory;
    this.#scheduleCountdownFrame();
  }

  setFilter(filter) {
    this.filter = filter;
    this.render();
  }

  async load() {
    const requestId = this.#requests.begin();
    try {
      const dashboard = await api('/api/v1/monitor/dashboard');
      if (!this.#requests.isCurrent(requestId)) return;
      this.dashboard = dashboard;
      this.render();
    } catch (error) {
      if (!this.#requests.isCurrent(requestId)) return;
      $('#targetGrid').innerHTML = `<div class="empty-state">${escapeHTML(error.message)}</div>`;
      toast(error.message);
    }
  }

  async loadActivity({ silent = false } = {}) {
    const requestId = this.#activityRequests.begin();
    try {
      const activity = await api('/api/v1/monitor/activity');
      if (!this.#activityRequests.isCurrent(requestId)) return;
      const targets = Array.isArray(activity.targets) ? activity.targets : [];
      this.#activeUsers = new Map(
        targets
          .map((target) => [String(target?.target_key || ''), normalizeCount(target?.active_users)])
          .filter(([key]) => key)
      );
      this.#activityWindowSeconds = normalizeWindowSeconds(activity.window_seconds);
      this.#activityLoaded = true;
      this.render();
    } catch (error) {
      if (!this.#activityRequests.isCurrent(requestId)) return;
      if (!silent) toast(error.message || '实时人数读取失败');
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
    $('#overallMeta').textContent = `${summary.targets || 0} 个对象 · 24 小时统计 · 最近更新 ${formatTime(this.dashboard.generated_at)}`;
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
      if (this.#countdownState !== 'running') {
        $('#probeCountdown').textContent = '···';
        $('#probeStatusText').textContent = '正在巡检';
        $('#nextProbeText').textContent = '本轮进行中';
        $('#nextProbeText').hidden = false;
        this.#countdownState = 'running';
        this.#countdownSeconds = null;
      }
      return;
    }
    if (!this.#nextProbeAt || !this.#intervalSeconds) {
      status.style.setProperty('--cooldown-progress', '0turn');
      if (this.#countdownState !== 'idle') {
        $('#probeCountdown').textContent = '--';
        $('#probeStatusText').textContent = '等待调度';
        $('#nextProbeText').textContent = '正在同步周期';
        $('#nextProbeText').hidden = false;
        this.#countdownState = 'idle';
        this.#countdownSeconds = null;
      }
      return;
    }
    const remainingMs = Math.max(0, this.#nextProbeAt - Date.now());
    const remaining = Math.ceil(remainingMs / 1000);
    const progress = Math.min(1, remainingMs / (this.#intervalSeconds * 1000));
    status.style.setProperty('--cooldown-progress', `${progress}turn`);
    if (this.#countdownState !== 'cooldown' || this.#countdownSeconds !== remaining) {
      $('#probeCountdown').textContent = formatCountdown(remaining);
      $('#probeStatusText').textContent = remaining ? '下次巡检' : '即将巡检';
      $('#nextProbeText').textContent = remaining ? `${remaining} 秒后` : '等待本轮开始';
      $('#nextProbeText').hidden = Boolean(remaining);
      this.#countdownState = 'cooldown';
      this.#countdownSeconds = remaining;
    }
    if (!remaining && !this.#countdownRefreshRequested) {
      this.#countdownRefreshRequested = true;
      window.setTimeout(() => void this.load(), 800);
    }
  }

  #scheduleCountdownFrame() {
    window.requestAnimationFrame(() => {
      this.#renderProbeCountdown();
      this.#scheduleCountdownFrame();
    });
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
    const firstByte = stats.first_byte || {};
    const latency = stats.latency || {};
    const status = normalizeStatus(item.status);
    const source = sourceLabel(item.latest_source);
    const samples = Number(stats.samples || 0);
    const hasSamples = samples > 0;
    const availabilityLabel = hasSamples ? `${samples} 次样本` : '暂无样本';
    const availabilityValue = hasSamples ? formatPct(stats.availability) : '—';
    const availabilityStatus = item.kind === 'group' && status === 'degraded' ? '' : status;
    const availabilityTone = hasSamples ? availabilityClass(stats.availability, availabilityStatus) : 'neutral';
    const currentRate = formatCurrentRate(item.rate_multiplier);
    const currentRateLabel = item.kind === 'group' ? '当前倍率' : '账户倍率';
    const currentRateTitle = item.kind === 'group' ? '当前分组成本倍率' : '当前账户成本倍率';
    const groupNote = item.kind === 'group' && item.latest_message
      ? `<div class="target-note">${escapeHTML(item.latest_message)}</div>`
      : '';
    const activeUsers = this.#activityLoaded ? (this.#activeUsers.get(item.key) || 0) : null;
    const activeUsersValue = activeUsers === null ? '—' : `${formatCount(activeUsers)} 人`;
    const activeUsersTitle = this.#activityLoaded
      ? `${formatActivityWindow(this.#activityWindowSeconds)}内有效请求的去重用户数`
      : '正在读取实时人数';
    return `
      <article class="target-card target-${status}" data-target="${escapeHTML(item.key)}" data-name="${escapeHTML(item.name)}"
        role="button" tabindex="0" aria-label="查看 ${escapeHTML(item.name)} 的历史记录">
        <div class="target-head">
          <div class="target-copy">
            <div class="target-kind">${item.kind === 'group' ? 'GROUP' : 'ACCOUNT'}</div>
            <div class="target-name" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</div>
            <div class="target-platform">${escapeHTML(item.platform || 'mixed')}${item.stale ? `<span class="stale-label">● ${escapeHTML(staleLabel(item, status))}</span>` : ''}</div>
            ${groupNote}
          </div>
          <div class="target-head-meta">
            ${currentRate ? `<span class="current-rate" title="${currentRateTitle}">${currentRateLabel} ${currentRate}</span>` : ''}
            <span class="status-badge ${statusClass(status)}">${targetStatusLabel(item, status)}</span>
          </div>
        </div>
        <div class="availability">
          <span class="availability-label">24 小时窗口观测通过率 · ${availabilityLabel}</span>
          <strong class="availability-value ${availabilityTone}">${availabilityValue}</strong>
        </div>
        <div class="target-live" title="${escapeHTML(activeUsersTitle)}">
          <span class="target-live-label">${formatActivityWindow(this.#activityWindowSeconds)}活跃用户</span>
          <strong class="target-live-value${activeUsers !== null && activeUsers > 0 ? ' has-users' : ''}">${activeUsersValue}</strong>
        </div>
        <div class="metrics">
          ${renderMetric('首字中位数', formatMedianMs(firstByte), '最近 24 小时成功样本的首字/首字节中位数')}
          ${renderMetric('最快', formatMs(latency.fastest_ms))}
          ${renderMetric('中位数', formatMedianMs(latency))}
          ${renderMetric('P95', formatMs(latency.p95_ms), '95% 的成功样本耗时不超过该值')}
        </div>
        <div class="card-foot">
          ${renderStatusHistory(item.recent_samples || [])}
          <span>最新证据 · ${source} · ${formatTime(item.last_checked_at)}</span>
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

function normalizeCount(value) {
  const count = Number(value);
  return Number.isFinite(count) && count >= 0 ? Math.floor(count) : 0;
}

function normalizeWindowSeconds(value) {
  const seconds = Number(value);
  return Number.isFinite(seconds) && seconds > 0 ? seconds : 300;
}

function formatActivityWindow(seconds) {
  return `近 ${Math.max(1, Math.round(seconds / 60))} 分钟`;
}

function formatCurrentRate(value) {
  if (value === null || value === undefined || value === '') return '';
  const rate = Number(value);
  return Number.isFinite(rate) ? rate.toFixed(4) : '';
}

function renderStatusHistory(samples) {
  const recent = samples.slice(-24);
  const empty = Array.from({ length: 24 - recent.length }, () => '<i aria-hidden="true"></i>');
  const items = recent.map((sample) => {
    const degraded = sample.status === 'degraded';
    const successful = sample.status === 'operational' || sample.status === 'degraded';
    const failed = sample.status === 'failed' || sample.status === 'error';
    const carried = Boolean(sample.carried_from) && (failed || successful);
    const label = carried
      ? `截至 ${formatTime(sample.checked_at)} · 无新采样，沿用 ${formatTime(sample.carried_from)} 的${statusLabel(sample.status)}状态 · ${sourceLabel(sample.source)}`
      : failed || successful
        ? `${formatTime(sample.checked_at)} · ${statusLabel(sample.status)} · ${sourceLabel(sample.source)}`
        : `${formatTime(sample.checked_at)} · 暂无采样`;
    const tone = degraded ? 'warn' : successful ? 'ok' : failed ? 'bad' : '';
    const classes = [tone, carried ? 'carried' : ''].filter(Boolean).join(' ');
    return `<i${classes ? ` class="${classes}"` : ''} role="img" aria-label="${escapeHTML(label)}" title="${escapeHTML(label)}"></i>`;
  });
  return `<div class="status-history" aria-label="24 小时内 24 段状态轨迹">${empty.concat(items).join('')}</div>`;
}

function renderMetric(label, value, help = '') {
  const title = help ? ` title="${escapeHTML(help)}"` : '';
  return `<div><div class="metric-label"${title}>${label}</div><div class="metric-value">${value}</div></div>`;
}

function sourceLabel(source) {
  if (source === 'history') return '真实请求';
  if (source === 'aggregate') return '分组候选检查';
  return source ? '主动探测' : '暂无来源';
}

function targetStatusLabel(item, status) {
  if (item.kind !== 'group') return statusLabel(status);
  if (status === 'degraded' && item.latest_source === 'aggregate' && isPendingAggregateFailure(item.latest_message)) {
    return '候选待确认';
  }
  if ((status === 'failed' || status === 'error') && item.latest_source === 'aggregate') return '候选不可用';
  return statusLabel(status);
}

function isPendingAggregateFailure(message) {
  return String(message || '').includes('等待下一轮确认');
}

function staleLabel(item, status) {
  if (status === 'failed' || status === 'error') return '等待恢复巡检';
  if (item.latest_source === 'probe') return '无流量，已验证';
  if (item.latest_source === 'history') return '无近期请求';
  return '无近期数据';
}
