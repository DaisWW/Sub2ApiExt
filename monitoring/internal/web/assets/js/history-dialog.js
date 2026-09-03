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

  async open(target, name) {
    const requestId = this.#requests.begin();
    $('#dialogTitle').textContent = name;
    $('#historyBody').innerHTML = '<tr><td colspan="4">读取历史中...</td></tr>';
    $('#historySummary').textContent = '';
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
    const visibleItems = items;
    const groupHistory = String(target || '').startsWith('group:');
    const successful = visibleItems.filter(isSuccessful).length;
    const availability = visibleItems.length ? successful * 100 / visibleItems.length : 0;
    $('#historySummary').innerHTML = `
      <span>最近 24 小时 · ${groupHistory ? '账户聚合记录' : '账户健康记录'} ${visibleItems.length}</span>
      <span>记录可用率 ${formatPct(availability)}</span>
      <span>${groupHistory ? '分组状态与卡片使用同一份账户聚合数据' : '真实请求为首字，主动探测为首字节近似值'}</span>`;
    $('#historyBody').innerHTML = visibleItems.length
      ? visibleItems.map(renderHistoryRow).join('')
      : '<tr><td colspan="4">最近 24 小时没有历史记录</td></tr>';
  }
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

function sourceLabel(source) {
  if (source === 'history') return '真实请求';
  if (source === 'aggregate') return '账户聚合';
  if (source === 'probe') return '主动探测';
  if (source === 'request_error') return '真实请求错误';
  if (source === 'cache') return '缓存证据';
  return '当前状态';
}
