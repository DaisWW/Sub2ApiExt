export const $ = (selector) => document.querySelector(selector);

// Keep latency colors consistent across end-to-end and first-byte metrics.
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

const platformAliases = new Map([
  ['openai', 'openai'],
  ['openai_compatible', 'openai'],
  ['codex', 'openai'],
  ['grok', 'openai'],
  ['xai', 'openai'],
  ['anthropic', 'anthropic'],
  ['claude', 'anthropic'],
  ['arthropic', 'anthropic'],
  ['airthropic', 'anthropic']
]);

const platformNames = {
  openai: 'OpenAI',
  anthropic: 'Anthropic',
  mixed: '混合平台',
  unknown: '未知平台'
};

export function normalizePlatform(value) {
  const raw = String(value ?? '').trim().toLowerCase();
  return platformAliases.get(raw) || raw || 'unknown';
}

export function platformLabel(value) {
  const key = normalizePlatform(value);
  return platformNames[key] || String(value ?? '').trim() || platformNames.unknown;
}

export function platformTone(value) {
  const key = normalizePlatform(value);
  return key === 'openai' || key === 'anthropic' ? key : 'other';
}

export function platformMatches(value, filter) {
  return filter === 'all' || normalizePlatform(value) === filter;
}

export function renderPlatformFilters(container, values, activeKey) {
  if (!container) return activeKey;
  const options = new Map();
  options.set('openai', platformNames.openai);
  options.set('anthropic', platformNames.anthropic);
  for (const value of values || []) {
    const key = normalizePlatform(value);
    if (!options.has(key)) options.set(key, platformLabel(value));
  }
  const selected = options.has(activeKey)
    ? activeKey
    : options.has('openai') ? 'openai' : options.keys().next().value;
  container.innerHTML = [...options.entries()].map(([key, label]) => {
    const tone = platformTone(key);
    const active = key === selected;
    return `<button type="button" class="platform-filter platform-filter-${tone}${active ? ' active' : ''}"
      data-platform-filter="${escapeHTML(key)}" aria-pressed="${String(active)}">${escapeHTML(label)}</button>`;
  }).join('');
  return selected;
}

export function nullableNonNegativeNumber(value) {
  if (value == null || (typeof value === 'string' && value.trim() === '')) return null;
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : null;
}

export function priorityValue(value, fallback = null) {
  const priority = nullableNonNegativeNumber(value);
  return priority === null ? fallback : Math.trunc(priority);
}

const knownStatuses = new Set(['operational', 'degraded', 'failed', 'error', 'unknown', 'disabled']);

export function normalizeStatus(status) {
  return knownStatuses.has(status) ? status : 'unknown';
}

export function sourceLabel(source) {
  if (source === 'history') return '真实请求';
  if (source === 'aggregate') return '账户聚合';
  if (source === 'probe') return '主动探测';
  if (source === 'request_error') return '真实请求错误';
  if (source === 'cache') return '缓存证据';
  return '当前状态';
}

export function statusClass(status) {
  const normalized = normalizeStatus(status);
  if (normalized === 'disabled') return 'status-failed';
  return `status-${normalized}`;
}

export function formatMs(value) {
  if (value == null) return '—';
  const milliseconds = Number(value);
  if (!Number.isFinite(milliseconds)) return '—';
  const roundedMilliseconds = Math.max(0, Math.round(milliseconds));
  const totalSeconds = Math.round(roundedMilliseconds / 1000);
  if (totalSeconds < 60) {
    const seconds = roundedMilliseconds / 1000;
    const precision = seconds < 1 ? 3 : 2;
    return `${seconds.toFixed(precision).replace(/\.?0+$/, '')}s`;
  }
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) {
    return `${hours}h ${String(minutes).padStart(2, '0')}m ${String(seconds).padStart(2, '0')}s`;
  }
  return `${minutes}m ${String(seconds).padStart(2, '0')}s`;
}

export function formatMedianMs(stats) {
  return stats?.median_ms == null ? '—' : formatMs(stats.median_ms);
}

export function latencyMetricClass(value) {
  if (value == null || value === '') return '';
  const milliseconds = Number(value);
  if (!Number.isFinite(milliseconds)) return '';
  return milliseconds >= slowLatencyThresholdMs ? 'warn' : 'good';
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
