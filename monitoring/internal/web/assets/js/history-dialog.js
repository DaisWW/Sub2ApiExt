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
  statusLabel
} from './shared.js';

export class HistoryDialog {
  #requests = new LatestRequest();
  #getWindowDays;
  #dialog;

  constructor(getWindowDays) {
    this.#getWindowDays = getWindowDays;
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
      const days = this.#getWindowDays();
      const data = await api(`/api/v1/monitor/history?target=${encodeURIComponent(target)}&days=${days}&limit=240`);
      if (!this.#requests.isCurrent(requestId) || !this.#dialog.open) return;
      this.#render(data.items || []);
    } catch (error) {
      if (!this.#requests.isCurrent(requestId) || !this.#dialog.open) return;
      $('#historyBody').innerHTML = `<tr><td colspan="4">${escapeHTML(error.message)}</td></tr>`;
    }
  }

  #render(items) {
    const successful = items.filter(isSuccessful).length;
    const availability = items.length ? successful * 100 / items.length : 0;
    $('#historySummary').innerHTML = `
      <span>样本 ${items.length}</span>
      <span>可用率 ${formatPct(availability)}</span>
      <span>真实请求为首字，主动探测为首字节近似值</span>`;
    $('#historyBody').innerHTML = items.length
      ? items.map(renderHistoryRow).join('')
      : '<tr><td colspan="4">该窗口没有历史记录</td></tr>';
  }
}

function isSuccessful(item) {
  return item.status === 'operational' || item.status === 'degraded';
}

function renderHistoryRow(item) {
  const status = normalizeStatus(item.status);
  const message = String(item.message || '请求失败');
  const error = isSuccessful(item) ? '' : `<small class="history-error" title="${escapeHTML(message)}">${escapeHTML(message)}</small>`;
  return `<tr>
    <td>${formatTime(item.checked_at)}<small class="history-source">${sourceLabel(item.source)}</small></td>
    <td class="table-status ${historyStatusClass(status)}">${statusLabel(status)}${error}</td>
    <td>${formatMs(item.first_byte_ms)}</td><td>${formatMs(item.latency_ms)}</td>
  </tr>`;
}

function sourceLabel(source) {
  if (source === 'history') return '真实请求';
  if (source === 'aggregate') return '分组聚合';
  return '主动探测';
}
