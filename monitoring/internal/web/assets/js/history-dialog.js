import {
  $,
  LatestRequest,
  api,
  escapeHTML,
  formatMs,
  formatPct,
  formatTime,
  latencyMetricClass,
  normalizeStatus,
  slowLatencyThresholdMs,
  sourceLabel
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
    const groupHistory = String(target || '').startsWith('group:');
    $('#dialogTitle').textContent = name;
    setAccountColumnVisible(groupHistory);
    $('#historyBody').innerHTML = `<tr><td colspan="${groupHistory ? 4 : 3}">读取历史中...</td></tr>`;
    $('#historySummary').textContent = '';
    if (!this.#dialog.open) this.#dialog.showModal();
    try {
      const data = await api(`/api/v1/monitor/history?target=${encodeURIComponent(target)}&limit=240`);
      if (!this.#requests.isCurrent(requestId) || !this.#dialog.open) return;
      this.#render(data.items || [], target);
    } catch (error) {
      if (!this.#requests.isCurrent(requestId) || !this.#dialog.open) return;
      $('#historyBody').innerHTML = `<tr><td colspan="${groupHistory ? 4 : 3}">${escapeHTML(error.message)}</td></tr>`;
    }
  }

  #render(items, target) {
    const groupHistory = String(target || '').startsWith('group:');
    setAccountColumnVisible(groupHistory);
    const successful = items.filter(isSuccessful).length;
    const availability = items.length ? successful * 100 / items.length : null;
    const availabilityLabel = groupHistory ? '真实请求可用率' : '记录可用率';
    const availabilityValue = availability === null ? '—' : formatPct(availability);
    $('#historySummary').innerHTML = `
      <span>最近 24 小时 · ${groupHistory ? '分组真实请求记录' : '账户健康记录'} ${items.length}</span>
      <span>${availabilityLabel} ${availabilityValue}</span>
      <span>${groupHistory ? '每条真实请求标注实际经由账户' : '真实请求为首字，主动探测为首字节近似值'}</span>`;
    $('#historyBody').innerHTML = items.length
      ? items.map((item) => renderHistoryRow(item, groupHistory)).join('')
      : `<tr><td colspan="${groupHistory ? 4 : 3}">最近 24 小时没有历史记录</td></tr>`;
  }
}

function setAccountColumnVisible(groupHistory) {
  const header = $('#historyAccountHeader');
  if (header) header.hidden = !groupHistory;
}

function isSuccessful(item) {
  const status = displayHistoryStatus(item);
  return status === 'operational' || status === 'degraded';
}

function renderHistoryRow(item, groupHistory) {
  const status = displayHistoryStatus(item);
  const message = String(item.message || '请求失败');
  const account = historyAccountLabel(item, groupHistory);
  const firstByteTone = latencyMetricClass(item.first_byte_ms);
  const latencyTone = latencyMetricClass(item.latency_ms);
  const errorDetail = status === 'failed'
    ? `<small class="history-error" title="${escapeHTML(message)}">${escapeHTML(message)}</small>`
    : '';
  return `<tr>
    <td>${formatTime(item.checked_at)}<small class="history-source">${sourceLabel(item.source)}</small>${errorDetail}</td>
    ${groupHistory ? `<td class="history-account">${escapeHTML(account)}</td>` : ''}
    <td class="latency-value${firstByteTone ? ` ${firstByteTone}` : ''}">${formatMs(item.first_byte_ms)}</td>
    <td class="latency-value${latencyTone ? ` ${latencyTone}` : ''}">${formatMs(item.latency_ms)}</td>
  </tr>`;
}

function historyAccountLabel(item, groupHistory) {
  const accountName = String(item?.account_name || '').trim();
  const accountID = Number(item?.account_id);
  if (accountName && Number.isSafeInteger(accountID) && accountID > 0 && accountName !== `账户 #${accountID}`) {
    return `${accountName} (#${accountID})`;
  }
  if (accountName) return accountName;
  if (Number.isSafeInteger(accountID) && accountID > 0) return `账户 #${accountID}`;
  return groupHistory ? '未知账户' : '当前账户';
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
