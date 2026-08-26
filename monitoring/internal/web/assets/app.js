(() => {
  const state = { dashboard: null, filter: 'all', windowDays: 7, token: localStorage.getItem('sub2api-monitor-token') || '', seenAlerts: new Set(JSON.parse(localStorage.getItem('sub2api-monitor-alerts') || '[]')) };
  const $ = (selector) => document.querySelector(selector);
  const escapeHTML = (value) => String(value ?? '').replace(/[&<>'"]/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' })[char]);
  const api = async (path, options = {}) => {
    const headers = { ...(options.headers || {}) };
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const response = await fetch(path, { ...options, headers });
    if (response.status === 401) {
      state.token = window.prompt('请输入监控 API Token') || '';
      if (state.token) { localStorage.setItem('sub2api-monitor-token', state.token); return api(path, options); }
    }
    if (!response.ok) throw new Error((await response.json().catch(() => ({}))).error || `HTTP ${response.status}`);
    return response.json();
  };
  const formatMs = (value) => value == null ? '—' : `${Math.round(value)} ms`;
  const formatPct = (value) => `${Number(value || 0).toFixed(2)}%`;
  const formatTime = (value) => value ? new Date(value).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '尚未探测';
  const statusLabel = (status) => ({ operational: '正常', degraded: '降级', failed: '失败', error: '错误', unknown: '等待探测', disabled: '已暂停' }[status] || status);
  const statusClass = (status) => `status-${status || 'unknown'}`;
  const availabilityClass = (value) => value >= 99 ? 'good' : value >= 90 ? 'warn' : 'bad';
  const metricValue = (stats) => stats && stats.median_ms != null ? formatMs(stats.median_ms) : '—';

  async function loadDashboard(silent = false) {
    if (!silent) $('#refreshButton').classList.add('loading');
    try {
      state.dashboard = await api(`/api/v1/monitor/dashboard?window=${state.windowDays}`);
      renderDashboard();
    } catch (error) {
      $('#targetGrid').innerHTML = `<div class="empty-state">${escapeHTML(error.message)}</div>`;
      toast(error.message, false);
    } finally {
      $('#refreshButton').classList.remove('loading');
    }
  }

  function renderDashboard() {
    const dashboard = state.dashboard;
    if (!dashboard) return;
    const summary = dashboard.summary || {};
	const probeButton = $('#probeButton');
	probeButton.disabled = Boolean(dashboard.probe_running);
	probeButton.innerHTML = dashboard.probe_running ? '<span>⌁</span> 探测中' : '<span>⌁</span> 立即探测';
    const hasFailure = summary.failed > 0;
    const hasDegraded = summary.degraded > 0;
    $('#overallTitle').textContent = hasFailure ? '服务需要关注' : hasDegraded ? '服务部分降级' : summary.targets ? '全部服务正常' : '等待探测数据';
    $('#overallMeta').textContent = `${summary.targets || 0} 个对象 · 最近更新 ${formatTime(dashboard.generated_at)}`;
    $('#kpiGrid').innerHTML = [
      ['综合可用率', formatPct(summary.availability), `${dashboard.window_days} 天窗口`, summary.availability >= 99 ? 'good' : summary.availability >= 90 ? 'warn' : 'bad'],
      ['正常对象', `${summary.operational || 0}`, `共 ${summary.targets || 0} 个`, 'good'],
      ['需要关注', `${(summary.degraded || 0) + (summary.failed || 0)}`, `${summary.failed || 0} 个失败`, (summary.failed || 0) ? 'bad' : 'warn'],
      ['探测周期', `${dashboard.interval_seconds || 60}`, '秒自动探测', '']
    ].map(([label, value, note, color]) => `<article class="kpi"><div class="kpi-label">${label}</div><div class="kpi-value ${color}">${value}${label === '探测周期' ? '<small>sec</small>' : ''}</div><div class="kpi-note">${note}</div></article>`).join('');
    const targets = (dashboard.targets || []).filter((item) => state.filter === 'all' || item.kind === state.filter);
    $('#sectionMeta').textContent = `${targets.length} / ${(dashboard.targets || []).length} 个对象`;
    $('#targetGrid').innerHTML = targets.length ? targets.map(renderTarget).join('') : '<div class="empty-state">当前筛选没有对象</div>';
    document.querySelectorAll('.target-card').forEach((card) => card.addEventListener('click', () => openHistory(card.dataset.target, card.dataset.name)));
  }

  function renderTarget(item) {
    const stats = item.stats || {};
    const latency = stats.latency || {};
    const status = item.status || 'unknown';
    const source = item.latest_source === 'history' ? '真实请求' : item.latest_source ? '主动探测' : '暂无来源';
    return `<article class="target-card" data-target="${escapeHTML(item.key)}" data-name="${escapeHTML(item.name)}">
      <div class="target-head"><div><div class="target-kind">${item.kind === 'group' ? 'GROUP' : 'ACCOUNT'}</div><div class="target-name" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</div><div class="target-platform">${escapeHTML(item.platform || 'mixed')}${item.stale ? '<span class="stale-label">● 数据滞后</span>' : ''}</div></div><span class="status-badge ${statusClass(status)}">${statusLabel(status)}</span></div>
      <div class="availability"><span class="availability-label">${state.windowDays} 天可用率 · ${stats.samples || 0} 次样本</span><strong class="availability-value ${availabilityClass(stats.availability)}">${formatPct(stats.availability)}</strong></div>
      <div class="metrics"><div><div class="metric-label">首字/首字节</div><div class="metric-value">${formatMs(item.latest_first_byte_ms)}</div></div><div><div class="metric-label">最快</div><div class="metric-value">${formatMs(latency.fastest_ms)}</div></div><div><div class="metric-label">中位数</div><div class="metric-value">${metricValue(latency)}</div></div><div><div class="metric-label">最慢</div><div class="metric-value">${formatMs(latency.slowest_ms)}</div></div></div>
      <div class="card-foot"><div class="availability-track"><i style="width:${Math.max(0, Math.min(100, Number(stats.availability || 0)))}%"></i></div><span>${source} · ${formatTime(item.last_checked_at)}</span></div>
    </article>`;
  }

  async function openHistory(target, name) {
    $('#dialogTitle').textContent = name;
    $('#historyBody').innerHTML = '<tr><td colspan="5">读取历史中...</td></tr>';
    $('#historySummary').textContent = '';
    $('#historyDialog').showModal();
    try {
      const data = await api(`/api/v1/monitor/history?target=${encodeURIComponent(target)}&days=${state.windowDays}&limit=240`);
      const items = data.items || [];
      const successful = items.filter((item) => item.status === 'operational' || item.status === 'degraded').length;
      $('#historySummary').innerHTML = `<span>样本 ${items.length}</span><span>可用率 ${formatPct(items.length ? successful * 100 / items.length : 0)}</span><span>真实请求为首字，主动探测为首字节近似值</span>`;
      $('#historyBody').innerHTML = items.length ? items.map((item) => `<tr><td>${formatTime(item.checked_at)}</td><td class="table-status ${item.status === 'operational' ? 'ok' : item.status === 'degraded' ? 'warn' : 'bad'}">${statusLabel(item.status)}</td><td>${formatMs(item.first_byte_ms)}</td><td>${formatMs(item.latency_ms)}</td><td title="${escapeHTML(item.message || '')}">${item.source === 'history' ? '真实请求' : '主动探测'} · ${escapeHTML(item.message || '—').slice(0, 38)}</td></tr>`).join('') : '<tr><td colspan="5">该窗口没有历史记录</td></tr>';
    } catch (error) {
      $('#historyBody').innerHTML = `<tr><td colspan="5">${escapeHTML(error.message)}</td></tr>`;
    }
  }

  async function loadAlerts() {
    try {
      const data = await api('/api/v1/monitor/alerts?unacknowledged=true&limit=30');
      const items = data.items || [];
      $('#alertList').innerHTML = items.length ? items.map((item) => `<article class="alert-item ${item.status === 'operational' ? 'recovered' : ''}"><div class="alert-title">${escapeHTML(item.title)}</div><div class="alert-message">${escapeHTML(item.message)}</div><div class="alert-time">${formatTime(item.created_at)}</div><button class="alert-ack" data-alert="${item.id}">标记已读</button></article>`).join('') : '<div class="empty-state">暂无未处理告警</div>';
      document.querySelectorAll('.alert-ack').forEach((button) => button.addEventListener('click', async (event) => { event.stopPropagation(); await api(`/api/v1/monitor/alerts/${button.dataset.alert}/ack`, { method: 'POST' }); loadAlerts(); }));
      for (const item of items) {
        if (!state.seenAlerts.has(item.id)) { state.seenAlerts.add(item.id); toast(`${item.title} · ${item.target_name}`, item.status === 'operational'); notify(item); }
      }
      localStorage.setItem('sub2api-monitor-alerts', JSON.stringify([...state.seenAlerts].slice(-100)));
    } catch (_) { /* dashboard remains useful when alert polling is temporarily unavailable */ }
  }

  function notify(item) {
    if (!('Notification' in window)) return;
    if (Notification.permission === 'granted') new Notification(item.title, { body: `${item.target_name}: ${item.message}` });
  }
  function toast(message, recovered) {
    const node = document.createElement('div'); node.className = `toast ${recovered ? 'recovered' : ''}`; node.textContent = message; $('#toastStack').appendChild(node); setTimeout(() => node.remove(), 5000);
  }

  document.querySelectorAll('[data-filter]').forEach((button) => button.addEventListener('click', () => { document.querySelectorAll('[data-filter]').forEach((item) => item.classList.remove('active')); button.classList.add('active'); state.filter = button.dataset.filter; renderDashboard(); }));
  $('#windowSelect').addEventListener('change', (event) => { state.windowDays = Number(event.target.value); loadDashboard(); });
  $('#refreshButton').addEventListener('click', () => loadDashboard());
  $('#probeButton').addEventListener('click', async () => {
    const button = $('#probeButton'); button.disabled = true; button.innerHTML = '<span>⌁</span> 探测中';
    try {
      await api('/api/v1/monitor/probe', { method: 'POST' });
      toast('主动探测已开始', true);
      for (let attempt = 0; attempt < 90; attempt += 1) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        await loadDashboard(true);
        if (!state.dashboard?.probe_running) break;
      }
      await loadAlerts();
    } catch (error) { toast(error.message, false); }
    finally { button.disabled = false; button.innerHTML = '<span>⌁</span> 立即探测'; }
  });
  $('#notifyButton').addEventListener('click', async () => { if ('Notification' in window && Notification.permission === 'default') await Notification.requestPermission(); $('#alertDrawer').classList.add('open'); loadAlerts(); });
  $('#closeAlerts').addEventListener('click', () => $('#alertDrawer').classList.remove('open'));
  $('#closeDialog').addEventListener('click', () => $('#historyDialog').close());
  $('#historyDialog').addEventListener('click', (event) => { if (event.target === $('#historyDialog')) $('#historyDialog').close(); });
  loadDashboard(); loadAlerts(); setInterval(() => loadDashboard(true), 30000); setInterval(loadAlerts, 30000);
})();
