import {
  $,
  LatestRequest,
  api,
  escapeHTML,
  formatCount,
  formatTime,
  toast
} from './shared.js';

export class ActivityPanel {
  activity = null;
  #requests = new LatestRequest();

  async load({ silent = false } = {}) {
    const requestId = this.#requests.begin();
    if (!silent) setActivityLoading(true);
    try {
      const activity = await api('/api/v1/monitor/activity');
      if (!this.#requests.isCurrent(requestId)) return;
      this.activity = activity;
      this.render();
    } catch (error) {
      if (!this.#requests.isCurrent(requestId)) return;
      const message = error?.message || '实时活动读取失败';
      if (this.activity && silent) {
        $('#activityMeta').textContent = `实时活动更新失败 · 保留上次数据`;
      } else {
        showActivityError(message);
      }
      if (!silent) toast(message);
    } finally {
      if (this.#requests.isCurrent(requestId)) setActivityLoading(false);
    }
  }

  render() {
    if (!this.activity) return;
    const summary = this.activity.summary || {};
    const windowSeconds = Number(this.activity.window_seconds || 300);
    const windowLabel = formatActivityWindow(windowSeconds);
    $('#activityMeta').textContent = `${windowLabel} · ${formatTime(this.activity.generated_at)} 更新`;
    $('#activityKpiGrid').innerHTML = [
      ['窗口活跃用户', summary.active_users],
      ['有效请求', summary.requests],
      ['使用渠道', summary.channels],
      ['使用账户', summary.accounts]
    ].map(renderActivityKPI).join('');
    renderActivityEntities($('#activityChannelList'), this.activity.channels, 'channel');
    renderActivityEntities($('#activityAccountList'), this.activity.accounts, 'account');
    renderActivityRoutes($('#activityRouteList'), this.activity.routes);
  }
}

function formatActivityWindow(seconds) {
  const value = Number(seconds || 300);
  const minutes = Number.isFinite(value) && value > 0 ? Math.max(1, Math.round(value / 60)) : 5;
  return `近 ${minutes} 分钟`;
}

function renderActivityKPI([label, value]) {
  return `<article class="activity-kpi">
    <div class="kpi-label">${label}</div>
    <div class="activity-kpi-value">${formatCount(value)}</div>
  </article>`;
}

function renderActivityEntities(container, sourceItems, kind) {
  const items = Array.isArray(sourceItems) ? sourceItems : [];
  if (!items.length) {
    container.innerHTML = '<div class="activity-empty">暂无窗口内有效请求</div>';
    return;
  }
  container.innerHTML = items.map((item) => {
    const name = item?.name || (kind === 'channel' ? '未归属渠道' : '未命名账户');
    const secondary = kind === 'channel'
      ? `${formatCount(item?.accounts)} 个账户`
      : `${formatCount(item?.channels)} 个渠道`;
    return `<div class="activity-row">
      <div class="activity-row-main">
        <strong title="${escapeHTML(name)}">${escapeHTML(name)}</strong>
        <span>${formatCount(item?.active_users)} 个活跃用户</span>
      </div>
      <div class="activity-row-meta">
        <strong>${formatCount(item?.requests)} 次请求</strong>
        <span>${secondary}</span>
      </div>
    </div>`;
  }).join('');
}

function renderActivityRoutes(container, sourceItems) {
  const items = Array.isArray(sourceItems) ? sourceItems : [];
  if (!items.length) {
    container.innerHTML = '<div class="activity-empty">暂无窗口内有效路由</div>';
    return;
  }
  container.innerHTML = items.map((item) => {
    const channel = item?.channel_name || '未归属渠道';
    const account = item?.account_name || '未命名账户';
    return `<div class="activity-route-row">
      <div class="activity-route-names">
        <strong title="${escapeHTML(channel)}">${escapeHTML(channel)}</strong>
        <span aria-hidden="true">→</span>
        <strong title="${escapeHTML(account)}">${escapeHTML(account)}</strong>
      </div>
      <div class="activity-row-meta">
        <strong>${formatCount(item?.active_users)} 个活跃用户</strong>
        <span>${formatCount(item?.requests)} 次请求</span>
      </div>
    </div>`;
  }).join('');
}

function setActivityLoading(loading) {
  $('#activitySection').classList.toggle('is-loading', loading);
  $('#activitySection').setAttribute('aria-busy', String(loading));
  if (!loading) return;
  $('#activityKpiGrid').innerHTML = '<div class="loading-state activity-loading">读取实时活动中...</div>';
  $('#activityChannelList').innerHTML = '<div class="activity-empty">读取渠道活动中...</div>';
  $('#activityAccountList').innerHTML = '<div class="activity-empty">读取账户活动中...</div>';
  $('#activityRouteList').innerHTML = '<div class="activity-empty">读取路由活动中...</div>';
  $('#activityMeta').textContent = '正在更新实时活动…';
}

function showActivityError(message) {
  const safeMessage = escapeHTML(message);
  $('#activityMeta').textContent = '实时活动读取失败';
  $('#activityKpiGrid').innerHTML = `<div class="empty-state activity-loading">${safeMessage}</div>`;
  $('#activityChannelList').innerHTML = `<div class="activity-empty">${safeMessage}</div>`;
  $('#activityAccountList').innerHTML = `<div class="activity-empty">${safeMessage}</div>`;
  $('#activityRouteList').innerHTML = `<div class="activity-empty">${safeMessage}</div>`;
}
