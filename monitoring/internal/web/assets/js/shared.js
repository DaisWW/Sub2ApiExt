export const $ = (selector) => document.querySelector(selector);

// A slow response is still usable; the dashboard only promotes it to yellow
// once the end-to-end latency reaches twenty seconds.
export const slowLatencyThresholdMs = 20000;

export class LatestRequest {
  #version = 0;

  begin() {
    this.#version += 1;
    return this.#version;
  }

  isCurrent(version) {
    return version === this.#version;
  }

  invalidate() {
    this.#version += 1;
  }
}

export async function api(path) {
  const response = await fetch(path);
  if (!response.ok) {
    const payload = await response.json().catch(() => ({}));
    throw new Error(payload.error || `HTTP ${response.status}`);
  }
  return response.json();
}

export function escapeHTML(value) {
  const entities = { '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' };
  return String(value ?? '').replace(/[&<>'"]/g, (character) => entities[character]);
}

const knownStatuses = new Set(['operational', 'degraded', 'failed', 'error', 'unknown', 'disabled']);

export function normalizeStatus(status) {
  return knownStatuses.has(status) ? status : 'unknown';
}

export function statusLabel(status) {
  return {
    operational: '可用',
    degraded: '可用但延迟高',
    failed: '错误/不可用',
    error: '错误/不可用',
    // Active targets without a fresh request keep their last usable route.
    // Unknown is therefore rendered as the normal usable baseline; the
    // monitoring data still retains the raw unknown state for decisions.
    unknown: '可用',
    disabled: '错误/不可用'
  }[normalizeStatus(status)];
}

export function statusClass(status) {
  const normalized = normalizeStatus(status);
  if (normalized === 'unknown') return 'status-operational';
  if (normalized === 'disabled') return 'status-failed';
  return `status-${normalized}`;
}

export function historyStatusClass(status) {
  const normalized = normalizeStatus(status);
  if (normalized === 'operational') return 'ok';
  if (normalized === 'degraded') return 'warn';
  if (normalized === 'unknown') return 'ok';
  return 'bad';
}

export function availabilityClass(value, status = '') {
  const normalized = normalizeStatus(status);
  if (normalized === 'failed' || normalized === 'error') return 'bad';
  if (normalized === 'degraded') return 'warn';
  if (value >= 99) return 'good';
  if (value >= 90) return 'warn';
  return 'bad';
}

export function formatMs(value) {
  if (value == null) return '—';
  const milliseconds = Math.max(0, Math.round(value));
  if (milliseconds < 60000) return `${milliseconds} ms`;
  const totalSeconds = Math.round(milliseconds / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}分${seconds}秒`;
}

export function formatMedianMs(stats) {
  return stats?.median_ms == null ? '—' : formatMs(stats.median_ms);
}

export function formatPct(value) {
  return `${Number(value || 0).toFixed(2)}%`;
}

export function formatTime(value) {
  if (!value) return '—';
  return new Date(value).toLocaleString('zh-CN', {
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit'
  });
}

export function formatTokens(value) {
  const amount = Number(value || 0);
  if (amount >= 1e9) return `${(amount / 1e9).toFixed(2)}B`;
  if (amount >= 1e6) return `${(amount / 1e6).toFixed(2)}M`;
  if (amount >= 1e3) return `${(amount / 1e3).toFixed(1)}K`;
  return Math.round(amount).toLocaleString('zh-CN');
}

export function formatUSD(value) {
  return `$${Number(value || 0).toFixed(4)}`;
}

export function formatCount(value) {
  return Number(value || 0).toLocaleString('zh-CN');
}

export function activateToggle(selector, activeButton) {
  document.querySelectorAll(selector).forEach((button) => {
    const active = button === activeButton;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', String(active));
  });
}

export function toast(message, recovered = false) {
  const node = document.createElement('div');
  node.className = `toast ${recovered ? 'recovered' : ''}`;
  node.textContent = message;
  $('#toastStack').appendChild(node);
  setTimeout(() => node.remove(), 5000);
}
