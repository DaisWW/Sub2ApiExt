import { $, LatestRequest, api, escapeHTML, formatTime, normalizeStatus, toast } from './shared.js';

const storageKey = 'sub2api-monitor-alerts';

export class AlertCenter {
  #requests = new LatestRequest();
  #seenAlerts = loadSeenAlerts();
  #initialized = false;
  #drawer;
  #backdrop;
  #returnFocus;

  constructor() {
    this.#drawer = $('#alertDrawer');
    this.#backdrop = $('#alertBackdrop');
    $('#notifyButton').addEventListener('click', () => this.open());
    $('#closeAlerts').addEventListener('click', () => this.close());
    this.#backdrop.addEventListener('click', () => this.close());
    document.addEventListener('keydown', (event) => {
      if (!this.#drawer.classList.contains('open')) return;
      if (event.key === 'Escape') {
        event.preventDefault();
        this.close();
        return;
      }
      if (event.key !== 'Tab') return;
      const focusable = [...this.#drawer.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')]
        .filter((element) => !element.disabled && element.offsetParent !== null);
      if (!focusable.length) {
        event.preventDefault();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && (document.activeElement === first || !this.#drawer.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !this.#drawer.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    });
  }

  open() {
    if (!this.#drawer.classList.contains('open')) this.#returnFocus = document.activeElement;
    this.#setDrawer(true);
    void this.load();
    if ('Notification' in window && Notification.permission === 'default') {
      void Notification.requestPermission();
    }
  }

  close() {
    const returnFocus = this.#returnFocus;
    this.#returnFocus = null;
    this.#setDrawer(false);
    if (returnFocus && returnFocus !== document.body && returnFocus.isConnected && typeof returnFocus.focus === 'function') returnFocus.focus();
    else $('#notifyButton').focus();
  }

  async load() {
    const requestId = this.#requests.begin();
    try {
      const data = await api('/api/v1/monitor/alerts?limit=30');
      if (!this.#requests.isCurrent(requestId)) return;
      const items = data.items || [];
      const visibleItems = foldAlerts(items);
      this.#render(visibleItems, items.length - visibleItems.length);
      this.#notifyNew(visibleItems);
      this.#remember(items);
      this.#initialized = true;
      this.#setStatus(`${formatTime(new Date())} 更新`);
    } catch {
      if (!this.#requests.isCurrent(requestId)) return;
      this.#setStatus(this.#initialized ? '更新失败 · 保留旧数据' : '读取失败', true);
    }
  }

  #setDrawer(open) {
    this.#drawer.classList.toggle('open', open);
    this.#backdrop.classList.toggle('open', open);
    this.#drawer.inert = !open;
    this.#drawer.setAttribute('aria-hidden', String(!open));
    const pageContent = $('#pageContent');
    pageContent.inert = open;
    pageContent.setAttribute('aria-hidden', String(open));
    document.body.classList.toggle('drawer-open', open);
    $('#notifyButton').setAttribute('aria-expanded', String(open));
    if (open) $('#closeAlerts').focus();
  }

  #setStatus(message, stale = false) {
    const status = $('#alertMeta');
    status.textContent = message;
    status.classList.toggle('stale', stale);
  }

  #render(items, foldedCount) {
    if (!items.length) {
      $('#alertList').innerHTML = '<div class="empty-state">暂无告警</div>';
      return;
    }
    const folded = foldedCount > 0
      ? `<div class="alert-folded">已按对象折叠 ${foldedCount} 条重复告警</div>`
      : '';
    $('#alertList').innerHTML = `${items.map(renderAlert).join('')}${folded}`;
  }

  #notifyNew(items) {
    if (!this.#initialized) return;
    for (const item of items) {
      if (this.#seenAlerts.has(item.id)) continue;
      const recovered = normalizeStatus(item.status) === 'operational';
      toast(`${item.title} · ${item.target_name}`, recovered);
      notifyBrowser(item);
    }
  }

  #remember(items) {
    for (const item of items) this.#seenAlerts.add(item.id);
    const recent = [...this.#seenAlerts].slice(-100);
    this.#seenAlerts = new Set(recent);
    localStorage.setItem(storageKey, JSON.stringify(recent));
  }
}

function loadSeenAlerts() {
  try {
    const values = JSON.parse(localStorage.getItem(storageKey) || '[]');
    return new Set(Array.isArray(values) ? values : []);
  } catch (_) {
    return new Set();
  }
}

function foldAlerts(items) {
  const latestByTarget = new Map();
  for (const item of items) {
    const key = item.target_key || item.target_name || String(item.id);
    const existing = latestByTarget.get(key);
    if (existing) existing.duplicateCount += 1;
    else latestByTarget.set(key, { ...item, duplicateCount: 0 });
  }
  return [...latestByTarget.values()];
}

function renderAlert(item) {
  const recovered = normalizeStatus(item.status) === 'operational' ? 'recovered' : '';
  const duplicate = item.duplicateCount ? ` · 已折叠 ${item.duplicateCount} 条` : '';
  const scope = item.kind === 'group' ? '分组' : '账户';
  return `<article class="alert-item ${recovered}">
    <div class="alert-title"><span>${escapeHTML(item.title)}</span><span class="alert-scope">${scope}</span></div>
    <div class="alert-message">${escapeHTML(item.message)}</div>
    <div class="alert-time">${formatTime(item.created_at)}${duplicate}</div>
  </article>`;
}

function notifyBrowser(item) {
  if (!('Notification' in window) || Notification.permission !== 'granted') return;
  new Notification(item.title, { body: `${item.target_name}: ${item.message}` });
}
