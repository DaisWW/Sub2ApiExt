import {
  $,
  LatestRequest,
  api,
  escapeHTML,
  formatCount,
  formatMedianMs,
  formatMs,
  formatPct,
  formatTime,
  normalizeStatus,
  slowLatencyThresholdMs,
  sourceLabel,
  statusClass,
  toast
} from './shared.js';

export class DashboardPanel {
  dashboard = null;
  filter = 'group';
  #requests = new LatestRequest();
  #activityRequests = new LatestRequest();
  #openHistory;
  #activeUsers = new Map();
  #activeRequests = new Map();
  #currentConcurrency = new Map();
  #concurrencyAvailable = false;
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
    if (this.#nextProbeAt && this.#nextProbeAt <= Date.now()) this.#countdownRefreshRequested = true;
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
      this.#activeRequests = new Map(
        targets
          .map((target) => [String(target?.target_key || ''), normalizeCount(target?.requests)])
          .filter(([key]) => key)
      );
      this.#currentConcurrency = new Map(
        targets
          .map((target) => [String(target?.target_key || ''), normalizeCount(target?.current_concurrency)])
          .filter(([key]) => key)
      );
      this.#concurrencyAvailable = activity.concurrency_available === true;
      this.#activityWindowSeconds = normalizeWindowSeconds(activity.window_seconds);
      this.#activityLoaded = true;
      this.render();
    } catch (error) {
      if (!this.#activityRequests.isCurrent(requestId)) return;
      if (!silent) toast(error.message || '实时活动读取失败');
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
    $('#overallMeta').textContent = `${summary.targets || 0} 个对象 · 真实请求优先，错误才探测 · 最近更新 ${formatTime(this.dashboard.generated_at)}`;
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
        $('#probeStatusText').textContent = '后台扫描';
        $('#nextProbeText').textContent = '无渠道错误不发送上游请求';
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
      $('#probeStatusText').textContent = remaining ? '后台扫描' : '错误恢复巡检';
      $('#nextProbeText').textContent = remaining ? `${remaining} 秒后 · 仅错误触发上游请求` : '等待本轮开始';
      $('#nextProbeText').hidden = Boolean(remaining);
      this.#countdownState = 'cooldown';
      this.#countdownSeconds = remaining;
    }
    if (!remaining && !$('#dashboardPanel').hidden && !this.#countdownRefreshRequested) {
      this.#countdownRefreshRequested = true;
      window.setTimeout(() => {
        if (!$('#dashboardPanel').hidden) void this.load();
      }, 800);
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
    const displayStatus = displayHealthStatus(item, status);
    const samples = Number(stats.samples || 0);
    const hasSamples = samples > 0;
    const availabilityDetail = hasSamples ? ` · ${samples} 次样本` : '';
    const availabilityLabel = `近 1 小时通过率${availabilityDetail}`;
    const availabilityValue = hasSamples ? formatPct(stats.availability) : '—';
    const recentSamples = Array.isArray(item.recent_samples) ? item.recent_samples : [];
    const currentSample = recentSamples[recentSamples.length - 1];
    const currentGridStatus = currentSample
      ? displayHealthStatus(item, normalizeStatus(currentSample.status), currentSample.latency_ms, currentSample.checked_at)
      : displayStatus;
    const availabilityTone = availabilityToneForStatus(currentGridStatus);
    const currentRate = formatCurrentRate(item.rate_multiplier);
    const currentRateLabel = item.kind === 'group' ? '当前倍率' : '账户倍率';
    const currentRateTitle = item.kind === 'group' ? '当前分组成本倍率' : '当前账户成本倍率';
    const activeUsers = this.#activityLoaded ? (this.#activeUsers.get(item.key) || 0) : null;
    const activeUsersValue = activeUsers === null ? '—' : `${formatCount(activeUsers)} 人`;
    const activeRequests = this.#activityLoaded ? (this.#activeRequests.get(item.key) || 0) : null;
    const activeRequestsValue = activeRequests === null ? '—' : `${formatCount(activeRequests)} 次`;
    const activeUsersTitle = this.#activityLoaded
      ? `${formatActivityWindow(this.#activityWindowSeconds)}内有效请求的去重用户数`
      : '正在读取实时人数';
    const activeRequestsTitle = this.#activityLoaded
      ? `${formatActivityWindow(this.#activityWindowSeconds)}内中转站已完成的有效请求数，不是当前连接数`
      : '正在读取实时请求数';
    const currentConcurrency = this.#activityLoaded && this.#concurrencyAvailable
      ? (this.#currentConcurrency.get(item.key) || 0)
      : null;
    const currentConcurrencyValue = currentConcurrency === null ? '—' : `${formatCount(currentConcurrency)} 条`;
    const currentConcurrencyTitle = this.#activityLoaded && !this.#concurrencyAvailable
      ? '中转站 Redis 实时并发数据当前不可用'
      : '直接读取中转站当前占用的账户请求并发槽位；请求结束即释放，不等于空闲聊天窗口数';
    const currentConcurrencyMetric = item.kind === 'account'
      ? `<div class="target-live-metric" title="${escapeHTML(currentConcurrencyTitle)}">
            <span class="target-live-label">当前并发请求</span>
            <strong class="target-live-value${currentConcurrency !== null && currentConcurrency > 0 ? ' has-concurrency' : ''}">${currentConcurrencyValue}</strong>
          </div>`
      : '';
    const targetNote = evidenceNote(item, status);
    const note = targetNote
      ? `<div class="target-note">${escapeHTML(targetNote)}</div>`
      : '';
    const evidenceAgeLabel = item.stale && item.latest_source
      ? `沿用最近${staleLabel(item, status)}`
      : '';
    const statusTitle = evidenceAgeLabel
      ? `当前状态：${evidenceAgeLabel}`
      : '当前状态：最新证据';
    return `
      <article class="target-card target-${displayStatus}" data-target="${escapeHTML(item.key)}" data-name="${escapeHTML(item.name)}"
        role="button" tabindex="0" aria-label="查看 ${escapeHTML(item.name)} 的历史记录">
        <div class="target-head">
          <div class="target-copy">
            <div class="target-kind">${item.kind === 'group' ? 'GROUP' : 'ACCOUNT'}</div>
            <div class="target-name" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</div>
            <div class="target-platform">${escapeHTML(item.platform || 'mixed')}${evidenceAgeLabel ? `<span class="stale-label">● ${escapeHTML(evidenceAgeLabel)}</span>` : ''}</div>
            ${note}
          </div>
          <div class="target-head-meta">
            ${currentRate ? `<span class="current-rate" title="${currentRateTitle}">${currentRateLabel} ${currentRate}</span>` : ''}
            <span class="status-badge ${statusClass(displayStatus)}" title="${escapeHTML(statusTitle)}">${targetStatusLabel(displayStatus)}</span>
          </div>
        </div>
        <div class="availability">
          <span class="availability-label">${availabilityLabel}</span>
          <strong class="availability-value ${availabilityTone}">${availabilityValue}</strong>
        </div>
        <div class="target-live${item.kind === 'group' ? ' target-live-group' : ''}">
          <div class="target-live-metric" title="${escapeHTML(activeUsersTitle)}">
            <span class="target-live-label">${formatActivityWindow(this.#activityWindowSeconds)}活跃用户</span>
            <strong class="target-live-value${activeUsers !== null && activeUsers > 0 ? ' has-users' : ''}">${activeUsersValue}</strong>
          </div>
          <div class="target-live-metric" title="${escapeHTML(activeRequestsTitle)}">
            <span class="target-live-label">${formatActivityWindow(this.#activityWindowSeconds)}请求数</span>
            <strong class="target-live-value${activeRequests !== null && activeRequests > 0 ? ' has-requests' : ''}">${activeRequestsValue}</strong>
          </div>
          ${currentConcurrencyMetric}
        </div>
        <div class="metrics">
          ${renderMetric('首字中位数', formatMedianMs(firstByte), '最近 1 小时成功样本的首字/首字节中位数', latencyMetricClass(firstByte.median_ms))}
          ${renderMetric('最快', formatMs(latency.fastest_ms), '', latencyMetricClass(latency.fastest_ms))}
          ${renderMetric('中位数', formatMedianMs(latency), '', latencyMetricClass(latency.median_ms))}
          ${renderMetric('P95', formatMs(latency.p95_ms), '95% 的成功样本耗时不超过该值')}
        </div>
        <div class="card-foot">
          ${renderStatusHistory(item.recent_samples || [], item)}
          <span>${evidenceFooter(item, status)}</span>
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

function displayHealthStatus(item, status, sampleLatencyMs = null, sampleCheckedAt = null) {
  if (status === 'failed' || status === 'error') return 'failed';
  if (status === 'disabled') return 'failed';
  if (status === 'unknown') {
    if (sampleCheckedAt) {
      const sampleAt = Date.parse(String(sampleCheckedAt));
      const triggerAt = Date.parse(String(item?.recovery_trigger_at || ''));
      if (Number.isFinite(sampleAt) && Number.isFinite(triggerAt)) {
        return sampleAt >= triggerAt ? 'failed' : 'unknown';
      }
      return String(item?.source_status || '').trim().toLowerCase() === 'error'
        ? 'failed'
        : 'unknown';
    }
    return String(item?.source_status || '').trim().toLowerCase() === 'error' || hasRecoveryTrigger(item)
      ? 'failed'
      : 'unknown';
  }
  // Group status is already the account aggregate. Its latency is diagnostic;
  // deriving the color again could turn an operational mixed group yellow.
  if (item?.kind === 'group') return status === 'degraded' ? 'degraded' : 'operational';
  const latency = Number(sampleLatencyMs);
  if (Number.isFinite(latency) && latency > 0) {
    return latency >= slowLatencyThresholdMs ? 'degraded' : 'operational';
  }
  // A card can use its latest/median latency as a fallback. A historical
  // segment without its own latency must keep its recorded status instead of
  // repainting the whole timeline with the card's current median.
  if (!sampleCheckedAt && item?.kind !== 'group' && isSlowTarget(item, status)) return 'degraded';
  if (status === 'degraded') return 'degraded';
  return 'operational';
}

function isSlowTarget(item, status = normalizeStatus(item?.status)) {
  const latestLatency = Number(item?.latest_latency_ms);
  if (isSuccessfulStatus(status) && Number.isFinite(latestLatency) && latestLatency > 0) {
    return latestLatency >= slowLatencyThresholdMs;
  }
  const samples = Array.isArray(item?.recent_samples) ? item.recent_samples : [];
  for (let index = samples.length - 1; index >= 0; index -= 1) {
    if (!isSuccessfulStatus(samples[index]?.status)) continue;
    const sampleLatency = Number(samples[index]?.latency_ms);
    if (Number.isFinite(sampleLatency) && sampleLatency > 0) {
      return sampleLatency >= slowLatencyThresholdMs;
    }
  }
  const statsLatency = Number(item?.stats?.latency?.median_ms);
  return Number.isFinite(statsLatency) && statsLatency > 0 && statsLatency >= slowLatencyThresholdMs;
}

function isSuccessfulStatus(status) {
  const normalized = normalizeStatus(status);
  return normalized === 'operational' || normalized === 'degraded';
}

function healthLabel(status) {
  if (status === 'failed' || status === 'error') return '错误/不可用';
  if (status === 'degraded') return '可用但延迟高';
  if (status === 'unknown') return '待确认';
  return '可用';
}

function statusTone(status) {
  if (status === 'failed' || status === 'error') return 'bad';
  if (status === 'degraded') return 'warn';
  if (status === 'unknown') return 'neutral';
  return 'ok';
}

function availabilityToneForStatus(status) {
  if (status === 'failed' || status === 'error') return 'bad';
  if (status === 'degraded') return 'warn';
  if (status === 'unknown') return 'neutral';
  return 'good';
}

function renderStatusHistory(samples, item) {
  const recent = Array.isArray(samples) ? samples.slice(-24) : [];
  const hasUnknownSamples = recent.some((sample) => normalizeStatus(sample?.status) === 'unknown');
  const gatewayError = String(item?.source_status || '').trim().toLowerCase() === 'error';
  const recoveryPending = hasRecoveryTrigger(item);
  const emptyTone = 'neutral';
  const emptyLabel = '待确认 · 无历史桶数据';
  const empty = Array.from({ length: Math.max(0, 24 - recent.length) }, () => `<i class="${emptyTone}" role="img" aria-label="${escapeHTML(emptyLabel)}" title="${escapeHTML(emptyLabel)}"></i>`);
  const items = recent.map((sample) => {
    const sampleStatus = normalizeStatus(sample?.status);
    const successful = sampleStatus === 'operational' || sampleStatus === 'degraded';
    const failed = sampleStatus === 'failed' || sampleStatus === 'error';
    const displaySampleStatus = sample?.source === 'source_change'
      ? 'unknown'
      : displayHealthStatus(item, sampleStatus, sample?.latency_ms, sample?.checked_at);
    const carried = Boolean(sample?.carried_from) && (failed || successful);
    const label = carried
      ? `截至 ${formatTime(sample?.checked_at)} · 无新请求，沿用 ${formatTime(sample?.carried_from)} 的${healthLabel(displaySampleStatus)}状态 · ${sourceLabel(sample?.source)}`
      : failed || successful
      ? `${formatTime(sample?.checked_at)} · ${healthLabel(displaySampleStatus)} · ${sourceLabel(sample?.source)}`
      : `${formatTime(sample?.checked_at)} · ${healthLabel(displaySampleStatus)}`;
    const tone = statusTone(displaySampleStatus);
    const classes = [tone, carried ? 'carried' : ''].filter(Boolean).join(' ');
    return `<i class="${classes}" role="img" aria-label="${escapeHTML(label)}" title="${escapeHTML(label)}"></i>`;
  });
  const caption = statusHistoryCaption(recent, item, gatewayError, recoveryPending);
  return `<div class="status-history-block">
    <div class="status-history" aria-label="24 小时内 24 段状态轨迹">${empty.concat(items).join('')}</div>
    ${statusHistoryLegend(hasUnknownSamples)}
    <span class="status-history-caption">${escapeHTML(caption)}</span>
  </div>`;
}

function statusHistoryLegend(includeNeutral = false) {
  const neutral = includeNeutral ? '<span><i class="neutral"></i>待确认</span>' : '';
  return `<div class="status-history-legend" aria-label="状态图例">
    <span><i class="ok"></i>可用</span>
    <span><i class="warn"></i>可用但延迟高</span>
    <span><i class="bad"></i>错误/不可用</span>
    ${neutral}
  </div>`;
}

function statusHistoryCaption(samples, item, gatewayError, recoveryPending) {
  if (recoveryPending) {
    return recoveryProbeFailed(item)
      ? '恢复失败 · 按退避策略重试'
      : '等待恢复验证 · 仅渠道错误触发';
  }
  if (gatewayError && displayHealthStatus(item, normalizeStatus(item?.status)) === 'failed') {
    return '渠道错误 · 等待新的恢复证据';
  }
  if (item?.kind === 'account' && ['failed', 'error'].includes(normalizeStatus(item?.status)) &&
      samples.some((sample) => ['failed', 'error'].includes(normalizeStatus(sample?.status)))) {
    return '探测失败 · 等待新的渠道证据';
  }
  if (samples.some((sample) => sample?.carried_from)) return '空档沿用最近有效状态';
  if (item?.latest_source === 'history') return '真实请求证据';
  if (item?.latest_source === 'request_error') return '真实请求错误证据';
  if (item?.latest_source === 'probe') return '主动探测证据';
  if (item?.latest_source === 'aggregate') return '账户聚合证据';
  if (displayHealthStatus(item, normalizeStatus(item?.status)) === 'unknown') {
    return item?.kind === 'group' ? '待确认 · 等待账户健康证据' : '待确认 · 等待真实请求证据';
  }
  return '当前状态';
}

function latencyMetricClass(value) {
  if (value == null || value === '') return '';
  const milliseconds = Number(value);
  if (!Number.isFinite(milliseconds)) return '';
  return milliseconds >= slowLatencyThresholdMs ? 'warn' : 'good';
}

function renderMetric(label, value, help = '', tone = '') {
  const title = help ? ` title="${escapeHTML(help)}"` : '';
  const toneClass = tone ? ` ${tone}` : '';
  return `<div><div class="metric-label"${title}>${label}</div><div class="metric-value${toneClass}">${value}</div></div>`;
}

function staleLabel(item, status) {
  if ((status === 'failed' || status === 'error') && item?.latest_source === 'request_error') return '请求错误状态';
  if (status === 'degraded') return '延迟状态';
  if (status === 'failed' || status === 'error') return '错误状态';
  if (item?.latest_source === 'probe') return '探测状态';
  if (item?.latest_source === 'history') return '请求状态';
  return '有效状态';
}

function displayEvidenceMessage(item, value) {
  const message = String(value || '').trim();
  if (item?.kind === 'group' && item?.latest_source !== 'aggregate') return '';
  return message;
}

function targetStatusLabel(displayStatus) {
  if (displayStatus === 'failed') return '当前错误/不可用';
  if (displayStatus === 'degraded') return '当前可用但延迟高';
  if (displayStatus === 'unknown') return '当前待确认';
  return '当前可用';
}

function evidenceNote(item, status) {
  const message = displayEvidenceMessage(item, item?.latest_message);
  // Do not put the empty-window implementation detail on every card. The
  // health color already represents the current baseline; risk/error details
  // remain visible when they carry actionable information.
  const gatewayError = String(item.source_status || '').trim().toLowerCase() === 'error';
  const recoveryTrigger = hasRecoveryTrigger(item);
  if (message && (status !== 'unknown' || gatewayError || recoveryTrigger)) return message;
  if (recoveryProbeFailed(item, status)) return '恢复探测失败，按退避策略重试';
  if (recoveryTrigger && (status === 'failed' || status === 'error' || status === 'unknown')) return '渠道报错，等待恢复探测';
  if (gatewayError && status === 'operational' && item.latest_source === 'probe') return '已由主动探测确认恢复，等待网关状态同步';
  if (gatewayError && status === 'operational' && item.latest_source === 'history') return '已由真实请求确认恢复，等待网关状态同步';
  if (gatewayError && !recoveryTrigger && (status === 'failed' || status === 'error' || status === 'unknown')) {
    return '账户处于错误状态；没有新的渠道错误，暂不发送上游请求';
  }
  if (status === 'operational' && item.latest_source === 'history') return '已由真实请求确认可用';
  if (status === 'operational' && item.latest_source === 'probe') return '已由主动探测确认恢复';
  if ((status === 'failed' || status === 'error') && item.latest_source === 'request_error') return '真实请求报错，等待恢复确认';
  if ((status === 'failed' || status === 'error') && item.latest_source === 'probe') return '探测失败；没有新的渠道错误，当前不重试';
  return '';
}

function evidenceFooter(item, status) {
  if (!item.latest_source) {
    if (item.kind === 'group') return '等待账户健康证据';
    return String(item.source_status || '').trim().toLowerCase() === 'error'
      ? (hasRecoveryTrigger(item) ? '等待恢复探测' : '等待新的渠道错误')
      : '错误后才主动探测';
  }
  if (recoveryProbeFailed(item, status)) {
    return `恢复失败 · 主动探测 · ${formatTime(item.last_checked_at)}`;
  }
  if (hasRecoveryTrigger(item)) return '等待恢复探测';
  if ((status === 'failed' || status === 'error') && item.latest_source === 'probe' && !hasRecoveryTrigger(item)) {
    return `历史探测失败 · 当前不重试 · ${formatTime(item.last_checked_at)}`;
  }
  const label = item.stale
    ? '沿用证据'
    : status === 'operational' && item.latest_source === 'probe' ? '恢复证据' : '最新证据';
  return `${label} · ${sourceLabel(item.latest_source)} · ${formatTime(item.last_checked_at)}`;
}

function hasRecoveryTrigger(item) {
  return Boolean(item?.recovery_trigger_at);
}

function recoveryProbeFailed(item, status = normalizeStatus(item?.status)) {
  return hasRecoveryTrigger(item) && item.latest_source === 'probe' &&
    (status === 'failed' || status === 'error');
}
