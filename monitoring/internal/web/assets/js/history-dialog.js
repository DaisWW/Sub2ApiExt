import {
  $,
  LatestRequest,
  api,
  escapeHTML,
  formatMs,
  formatPct,
  formatTime,
  historyStatusClass,
  normalizeStatus,
  slowLatencyThresholdMs,
  sourceLabel,
  statusLabel
} from './shared.js';

export class HistoryDialog {
  #requests = new LatestRequest();
  #dialog;

  constructor() {
    this.#dialog = $('#historyDialog');
    $('#closeDialog').addEventListener('click', () => this.close());
    this.#dialog.addEventListener('close', () => this.#requests.invalidate());
    this.#dialog.addEventListener('cancel', () => this.#requests.invalidate());
    this.#dialog.addEventListener('click', (event) => {
      if (event.target === this.#dialog) this.close();
    });
  }

  close() {
    this.#dialog.close();
  }

  async open(target, name, currentTarget = null) {
    const requestId = this.#requests.begin();
    $('#dialogTitle').textContent = name;
    $('#historyBody').innerHTML = '<tr><td colspan="4">读取历史中...</td></tr>';
    $('#historySummary').textContent = '';
    renderMembers(String(target || '').startsWith('group:'), currentTarget?.members || []);
    if (!this.#dialog.open) this.#dialog.showModal();
    try {
      const data = await api(`/api/v1/monitor/history?target=${encodeURIComponent(target)}&limit=240`);
      if (!this.#requests.isCurrent(requestId) || !this.#dialog.open) return;
      this.#render(data.items || [], target);
    } catch (error) {
      if (!this.#requests.isCurrent(requestId) || !this.#dialog.open) return;
      $('#historyBody').innerHTML = `<tr><td colspan="4">${escapeHTML(error.message)}</td></tr>`;
    }
  }

  #render(items, target) {
    const groupHistory = String(target || '').startsWith('group:');
    const successful = items.filter(isSuccessful).length;
    const availability = items.length ? successful * 100 / items.length : 0;
    $('#historySummary').innerHTML = `
      <span>最近 24 小时 · ${groupHistory ? '账户聚合记录' : '账户健康记录'} ${items.length}</span>
      <span>记录可用率 ${formatPct(availability)}</span>
      <span>${groupHistory ? '分组状态与卡片使用同一份账户聚合数据' : '真实请求为首字，主动探测为首字节近似值'}</span>`;
    $('#historyBody').innerHTML = items.length
      ? items.map(renderHistoryRow).join('')
      : '<tr><td colspan="4">最近 24 小时没有历史记录</td></tr>';
  }
}

function renderMembers(groupHistory, members) {
  const section = $('#historyMembers');
  if (!groupHistory) {
    section.hidden = true;
    return;
  }
  const currentMembers = Array.isArray(members) ? members : [];
  const routable = currentMembers.filter((member) => member?.routable).length;
  $('#historyMembersMeta').textContent = `${currentMembers.length} 个账户 · ${routable} 个路由候选`;
  $('#historyMemberList').innerHTML = currentMembers.length
    ? currentMembers.map(renderMember).join('')
    : '<div class="history-members-empty">当前没有可监控的成员账户</div>';
  section.hidden = false;
}

function renderMember(member) {
  const status = displayMemberStatus(member);
  const routeLabel = member?.routable ? '路由候选' : '不参与路由';
  const evidence = member?.source
    ? `${sourceLabel(member.source)} · ${formatTime(member.checked_at)}`
    : '暂无健康证据';
  const message = String(member?.message || '').trim();
  return `<div class="history-member">
    <div class="history-member-identity">
      <strong title="${escapeHTML(member?.name || '')}">${escapeHTML(member?.name || `账户 #${member?.account_id || ''}`)}</strong>
      <span>#${escapeHTML(member?.account_id || '')} · ${escapeHTML(member?.platform || '未标记平台')}</span>
    </div>
    <div class="history-member-health">
      <strong class="history-member-status ${historyStatusClass(status)}">${historyStatusLabel(status)}</strong>
      <span>${escapeHTML(routeLabel)} · ${escapeHTML(evidence)}</span>
    </div>
    <div class="history-member-latency">${formatMs(member?.latency_ms)}</div>
    ${message ? `<div class="history-member-message" title="${escapeHTML(message)}">${escapeHTML(message)}</div>` : ''}
  </div>`;
}

function displayMemberStatus(member) {
  const normalized = normalizeStatus(member?.status);
  if (normalized === 'failed' || normalized === 'error' || normalized === 'disabled') return 'failed';
  if (normalized === 'unknown') {
    return String(member?.source_status || '').trim().toLowerCase() === 'error' ? 'failed' : 'unknown';
  }
  const latency = Number(member?.latency_ms);
  if (Number.isFinite(latency) && latency > 0) {
    return latency >= slowLatencyThresholdMs ? 'degraded' : 'operational';
  }
  return normalized === 'degraded' ? 'degraded' : 'operational';
}

function isSuccessful(item) {
  const status = displayHistoryStatus(item);
  return status === 'operational' || status === 'degraded';
}

function renderHistoryRow(item) {
  const status = displayHistoryStatus(item);
  const message = String(item.message || '请求失败');
  const error = status === 'failed' ? `<small class="history-error" title="${escapeHTML(message)}">${escapeHTML(message)}</small>` : '';
  return `<tr>
    <td>${formatTime(item.checked_at)}<small class="history-source">${sourceLabel(item.source)}</small></td>
    <td class="table-status ${historyStatusClass(status)}">${historyStatusLabel(status)}${error}</td>
    <td>${formatMs(item.first_byte_ms)}</td><td>${formatMs(item.latency_ms)}</td>
  </tr>`;
}

function historyStatusLabel(status) {
  return statusLabel(status);
}

function displayHistoryStatus(item) {
  const normalized = normalizeStatus(item?.status);
  if (normalized === 'failed' || normalized === 'error' || normalized === 'disabled') return 'failed';
  if (normalized === 'unknown') return 'unknown';
  if (item?.kind === 'group') return normalized === 'degraded' ? 'degraded' : 'operational';
  const latency = Number(item?.latency_ms);
  if (Number.isFinite(latency) && latency > 0) {
    return latency >= slowLatencyThresholdMs ? 'degraded' : 'operational';
  }
  if (normalized === 'degraded') return 'degraded';
	return 'operational';
}
