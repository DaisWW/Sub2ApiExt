'use strict';
document.documentElement.classList.toggle('iframe-embedded', window.self !== window.top);
const $ = (s) => document.querySelector(s);
const el = (tag, attrs = {}, text = '') => { const n = document.createElement(tag); for (const [k, v] of Object.entries(attrs)) { if (k === 'class') n.className = v; else n.setAttribute(k, v); } if (text) n.textContent = text; return n; };
const state = { suites: [], accounts: [], groups: [], selected: new Set(), suite: null, runs: [], detail: null, cache: new Map(), resultLimit: 24, icon: '', busy: false, refreshing: false, tab: 'compose' };
const labels = { queued: '排队中', running: '执行中', cancelling: '正在取消', cancelled: '已取消', interrupted: '已中断', completed: '完成', completed_with_errors: '部分失败', failed: '失败', incomplete: '未完整输出', pass: '通过', fail: '不通过', review: '待复核' };
const defaults = { openai: 'gpt-5.6-terra', anthropic: 'claude-sonnet-4-5-20250929', claude: 'claude-sonnet-4-5-20250929', gemini: 'gemini-2.5-flash', grok: 'grok-4.5', xai: 'grok-4.5' };
const platformLabels = { openai: 'OpenAI', anthropic: 'Anthropic', claude: 'Claude', gemini: 'Gemini', antigravity: 'Antigravity', grok: 'Grok', kimi: 'Kimi', zhipu: 'Zhipu GLM', deepseek: 'DeepSeek', minimax: 'MiniMax', opencode_go: 'OpenCode', openai_compatible: 'OpenAI Compatible', xai: 'XAI' };
const typeLabels = { oauth: 'OAuth', 'setup-token': 'Setup Token', apikey: 'API Key', bedrock: 'AWS Bedrock' };
const statusLabels = { active: '正常', inactive: '停用', error: '错误', rate_limited: '限流中', temp_unschedulable: '临时不可调度', unschedulable: '已停调' };
const accountFilterIDs = ['platform-filter', 'type-filter', 'status-filter', 'privacy-filter', 'group-filter', 'account-search'];
let savedSettings = {}; try { savedSettings = JSON.parse(localStorage.getItem('account_eval_settings') || '{}'); } catch {}
let noticeTimer;
function notice(message, error = false) { $('#notice').textContent = message; $('#notice').className = error ? 'error' : ''; $('#notice').hidden = false; clearTimeout(noticeTimer); noticeTimer = setTimeout(() => { $('#notice').hidden = true; }, error ? 15000 : 5000); }
async function api(path, options = {}) {
  const token = localStorage.getItem('auth_token') || '';
  const response = await fetch(`api/${path}`, { ...options, headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}`, ...options.headers }, cache: 'no-store' });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) { if (response.status === 401 || response.status === 403) { $('#login').hidden = false; $('#workbench').hidden = true; } throw new Error(body.error || `请求失败 (${response.status})`); }
  return body;
}
function guard(fn) { return async (...args) => { try { await fn(...args); } catch (error) { notice(error.message, true); } }; }
function tab(name) { state.tab = name; document.querySelectorAll('.tab').forEach((n) => n.classList.toggle('active', n.id === name)); document.querySelectorAll('[data-tab]').forEach((n) => n.classList.toggle('active', n.dataset.tab === name)); if (name === 'history') refreshRuns().catch((e) => notice(e.message, true)); }
function fillSuites() { const select = $('#suite-select'); select.replaceChildren(); if (!state.suites.length) select.append(el('option', { value: '' }, '尚无题组，保存后下次可直接使用')); for (const suite of state.suites) select.append(el('option', { value: suite.id }, suite.name)); select.value = state.suite?.id || ''; }
function blankSuite() { return { id: '', name: '逻辑与创作', questions: [{ name: '鹈鹕骑自行车', prompt: '创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的 2D 动画，你不需要任何测试，不要有任何限制。', format: 'html' }] }; }
function editSuite(suite) { state.suite = structuredClone(suite); $('#suite-name').value = suite.name; $('#suite-state').textContent = suite.id ? '已保存' : '尚未保存'; fillSuites(); renderQuestions(); estimate(); }
function dirty() { $('#suite-state').textContent = '有未保存的修改'; }
function renderQuestions() {
  const container = $('#questions'); container.replaceChildren();
  state.suite.questions.forEach((q, i) => {
    const box = el('div', { class: 'question' }); const top = el('div', { class: 'question-top' });
    const nameLabel = el('label', {}, `第 ${i + 1} 题`); const name = el('input', { value: q.name, 'aria-label': `第 ${i + 1} 题名称` }); name.oninput = () => { q.name = name.value; dirty(); }; nameLabel.append(name);
    const formatLabel = el('label', { class: 'format' }, '展示格式'); const format = el('select', { 'aria-label': `第 ${i + 1} 题展示格式` }); for (const [value, text] of [['html', 'HTML'], ['text', '文本'], ['json', 'JSON']]) format.append(el('option', { value }, text)); format.value = q.format; format.onchange = () => { q.format = format.value; dirty(); }; formatLabel.append(format);
    top.append(nameLabel, formatLabel);
    for (const [text, delta] of [['↑', -1], ['↓', 1]]) { const b = el('button', { title: delta < 0 ? '上移题目' : '下移题目' }, text); b.disabled = i + delta < 0 || i + delta >= state.suite.questions.length; b.onclick = () => { [state.suite.questions[i], state.suite.questions[i + delta]] = [state.suite.questions[i + delta], state.suite.questions[i]]; renderQuestions(); dirty(); }; top.append(b); }
    const remove = el('button', { class: 'danger', title: '删除本题' }, '×'); remove.onclick = () => { state.suite.questions.splice(i, 1); renderQuestions(); dirty(); estimate(); }; top.append(remove);
    const prompt = el('textarea', { 'aria-label': `第 ${i + 1} 题问题内容`, spellcheck: 'false' }); prompt.value = q.prompt; prompt.oninput = () => { q.prompt = prompt.value; dirty(); }; box.append(top, prompt); container.append(box);
  });
}
async function saveSuite() { state.suite.name = $('#suite-name').value; const suite = await api('suites', { method: 'POST', body: JSON.stringify(state.suite) }); state.suite = suite; state.suites = await api('suites'); fillSuites(); $('#suite-state').textContent = '已保存'; return suite; }
function accountType(type) { return type === 'api_key' ? 'apikey' : type === 'setup_token' ? 'setup-token' : type; }
function accountStatus(a, now = Date.now()) {
  if (a.status !== 'active') return a.status === 'disabled' ? 'inactive' : a.status;
  if (Date.parse(a.temp_unschedulable_until) > now) return 'temp_unschedulable';
  if (Date.parse(a.rate_limit_reset_at) > now) return 'rate_limited';
  return a.schedulable ? 'active' : 'unschedulable';
}
function visibleAccounts() {
  const p = $('#platform-filter').value, t = $('#type-filter').value, s = $('#status-filter').value, privacy = $('#privacy-filter').value, g = $('#group-filter').value, query = $('#account-search').value.trim().toLowerCase(), now = Date.now();
  return state.accounts.filter((a) => (!p || a.platform === p) && (!t || accountType(a.type) === t) && (!s || accountStatus(a, now) === s) && (!privacy || (privacy === '__unset__' ? !(a.privacy_mode || '').trim() : a.privacy_mode === privacy)) && (!g || (g === 'ungrouped' ? !a.group_ids.length : a.group_ids.includes(Number(g)))) && (!query || `${a.name} ${a.id}`.toLowerCase().includes(query)));
}
function selectionCount(visible = visibleAccounts()) {
  const hidden = state.selected.size - visible.filter((a) => state.selected.has(a.id)).length;
  $('#selection-count').textContent = `当前 ${visible.length} / ${state.accounts.length} · 已选 ${state.selected.size}${hidden ? `（${hidden} 个在当前筛选外）` : ''}`;
}
function platformTone(platform) { return ['openai', 'openai_compatible', 'codex'].includes(platform) ? 'openai' : ['anthropic', 'claude'].includes(platform) ? 'anthropic' : 'other'; }
function renderAccounts() {
  const list = $('#accounts'); list.replaceChildren(); const visible = visibleAccounts();
  for (const a of visible) { const row = el('label', { class: 'account', 'data-platform': platformTone(a.platform) }); const input = el('input', { type: 'checkbox', 'aria-label': `${a.name} #${a.id}` }); input.checked = state.selected.has(a.id); input.onchange = () => { input.checked ? state.selected.add(a.id) : state.selected.delete(a.id); selectionChanged(); }; const name = el('span', { class: 'name' }, a.name); const type = accountType(a.type), status = accountStatus(a); name.append(el('small', {}, `#${a.id} · ${platformLabels[a.platform] || a.platform} · ${typeLabels[type] || type} · ${statusLabels[status] || status}`)); row.append(input, name); if (a.unsupported) row.append(el('span', { class: 'badge', title: a.unsupported }, '协议未支持')); list.append(row); }
  if (!visible.length) list.append(el('div', { class: 'empty' }, '当前范围没有账户'));
  selectionCount(visible);
}
function selectionChanged() { selectionCount(); renderModels(); estimate(); }
function settingsFromForm() { return { models: { ...savedSettings.models }, protocols: { ...savedSettings.protocols }, concurrency: Number($('#concurrency').value), timeout_seconds: Number($('#timeout').value), max_tokens: Number($('#max-tokens').value), reasoning: $('#reasoning').value }; }
function renderModels() {
  const platforms = [...new Set(state.accounts.filter((a) => state.selected.has(a.id)).map((a) => a.platform))]; if (!platforms.length) platforms.push('openai');
  const container = $('#model-settings'); container.replaceChildren(); savedSettings.models ||= {}; savedSettings.protocols ||= {};
  for (const platform of platforms) { const row = el('div', { class: 'model-row' }); const label = el('label', {}, `${platform} 模型`); const input = el('input', { placeholder: '填写该平台实际支持的模型' }); input.value = savedSettings.models[platform] ?? defaults[platform] ?? ''; savedSettings.models[platform] = input.value; input.oninput = () => { savedSettings.models[platform] = input.value; }; label.append(input); row.append(label);
    if (['openai', 'openai_compatible', 'grok', 'xai'].includes(platform)) { const label = el('label', {}, 'API Key 协议'); const select = el('select'); for (const [value, text] of [['auto', '自动'], ['responses', 'Responses'], ['chat', 'Chat Completions']]) select.append(el('option', { value }, text)); select.value = savedSettings.protocols[platform] || 'auto'; select.onchange = () => { savedSettings.protocols[platform] = select.value; }; label.append(select); row.append(label); } container.append(row);
  }
}
function estimate() { const count = state.selected.size * (state.suite?.questions.length || 0); $('#run-estimate').textContent = `${state.selected.size} 个账户 × ${state.suite?.questions.length || 0} 道题 = ${count} 次请求`; }
async function refreshRuns() {
  if (state.refreshing) return;
  state.refreshing = true;
  try {
  state.runs = await api('runs'); const box = $('#runs');
  const active = state.runs.filter((r) => ['queued', 'running', 'cancelling'].includes(r.status)); $('#running-count').textContent = active.length ? `(${active.length})` : '';
  const revision = `${state.detail?.run.id || ''}:${state.runs.map((r) => `${r.id}:${r.status}:${r.done}`).join('|')}`;
  if (box.dataset.revision !== revision) {
  box.dataset.revision = revision; box.replaceChildren();
  for (const r of state.runs) { const button = el('button', { class: `run-row${state.detail?.run.id === r.id ? ' selected' : ''}` }); const title = el('span', { class: 'run-title' }, r.name); title.append(el('small', {}, `${new Date(r.created_at).toLocaleString()} · ${r.accounts.length} 个账户 · ${r.suite.questions.length} 道题`)); button.append(title, el('span', { class: `badge ${r.status}` }, labels[r.status] || r.status), el('progress', { max: r.total || 1, value: r.done }), el('span', { class: 'muted' }, `${r.done}/${r.total}`)); button.onclick = guard(() => selectRun(r.id)); box.append(button); }
  if (!state.runs.length) box.append(el('div', { class: 'empty' }, '还没有测试任务。保存题组并选择账户后开始。'));
  }
  if (state.detail && state.tab === 'history') await refreshDetail();
  } finally { state.refreshing = false; }
}
async function selectRun(id) { state.cache.clear(); state.resultLimit = 24; state.detail = await api(`runs/${id}`); $('#question-filter').replaceChildren(); state.detail.run.suite.questions.forEach((q, i) => $('#question-filter').append(el('option', { value: i }, `${i + 1}. ${q.name}`))); $('#result-filter').value = ''; await renderDetail(); }
async function refreshDetail() { const id = state.detail.run.id; const detail = await api(`runs/${id}`); if (state.detail?.run.id !== id) return; state.detail = detail; await renderDetail(); }
async function renderDetail() {
  const { run } = state.detail; $('#run-detail').hidden = false; $('#detail-title').textContent = run.name;
  $('#detail-meta').textContent = `${labels[run.status] || run.status} · ${run.done}/${run.total} · 并发 ${run.settings.concurrency} · 单题 ${run.settings.timeout_seconds}s${run.parent_id ? ' · 失败项重试批次' : ''}`;
  $('#run-snapshot').textContent = JSON.stringify({ questions: run.suite.questions, settings: run.settings, accounts: run.accounts }, null, 2);
  const active = ['queued', 'running', 'cancelling'].includes(run.status); $('#cancel-run').hidden = !active; $('#cancel-run').disabled = run.status === 'cancelling'; $('#retry-run').hidden = active || !state.detail.results.some((r) => r.status !== 'completed');
  await renderResults();
}
const previewPolicy = '<meta http-equiv="Content-Security-Policy" content="default-src \'none\'; script-src \'unsafe-inline\'; style-src \'unsafe-inline\'; img-src data: blob:; font-src data:; connect-src \'none\'; form-action \'none\'; base-uri \'none\'">';
function download(name, content, type = 'text/plain;charset=utf-8') { const url = URL.createObjectURL(new Blob([content], { type })); const a = el('a', { href: url, download: name.replace(/[\\/:*?"<>|\x00-\x1f]/g, '_') }); document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000); }
async function renderResults() {
  if (!state.detail) return;
  const detail = state.detail, runID = detail.run.id, q = Number($('#question-filter').value), filter = $('#result-filter').value;
  const all = detail.results.filter((r) => r.question === q && (!filter || (['pass', 'fail', 'review'].includes(filter) ? r.mark === filter : filter === 'failed' ? ['failed', 'incomplete', 'interrupted', 'cancelled'].includes(r.status) : r.status === filter)));
  const visible = all.slice(0, state.resultLimit);
  await Promise.all(visible.map(async (r) => { const cached = state.cache.get(r.id); if (!['queued', 'running'].includes(r.status) && (!cached || cached.status !== r.status)) state.cache.set(r.id, await api(`results/${r.id}`)); }));
  if (state.detail?.run.id !== runID || Number($('#question-filter').value) !== q || $('#result-filter').value !== filter) return;
  const container = $('#results'); const revision = visible.map((r) => `${r.id}:${r.status}:${r.mark}`).join('|');
  if (container.dataset.revision === `${runID}:${q}:${filter}:${revision}`) return;
  container.dataset.revision = `${runID}:${q}:${filter}:${revision}`; container.replaceChildren();
  for (const summary of visible) {
    const result = { ...summary, ...state.cache.get(summary.id), mark: summary.mark }, account = detail.run.accounts.find((a) => a.id === result.account_id), question = detail.run.suite.questions[q];
    const card = el('article', { class: 'result-card', 'data-platform': platformTone(account?.platform), 'data-status': result.status }), head = el('div', { class: 'card-head' }); head.append(el('h3', {}, `${account?.name || result.account_id} · #${result.account_id}`)); const row = el('div', { class: 'row' }); row.append(el('span', { class: `badge ${result.status}` }, labels[result.status] || result.status), el('small', {}, result.model || detail.run.settings.models[account?.platform] || '')); head.append(row); head.append(el('small', {}, `${(result.duration_ms / 1000).toFixed(1)}s · 输出 ${result.output_tokens || 0} tokens`)); card.append(head);
    if (result.error) card.append(el('div', { class: 'error-text' }, result.error));
    const content = el('div'); let preview = !!result.html;
    const showContent = () => { content.replaceChildren(); if (preview && result.html) { const frame = el('iframe', { sandbox: 'allow-scripts', title: `${account?.name || result.account_id} 的 HTML 结果`, referrerpolicy: 'no-referrer' }); frame.srcdoc = previewPolicy + result.html; content.append(frame); } else { let answer = result.answer || (['queued', 'running'].includes(result.status) ? '等待回答…' : '没有文本输出'); if (question.format === 'json') { try { answer = JSON.stringify(JSON.parse(answer), null, 2); } catch {} } content.append(el('pre', {}, answer)); } }; showContent(); card.append(content);
    const actions = el('div', { class: 'card-actions' });
    if (result.html) { const toggle = el('button', {}, '预览 / 原文'); toggle.onclick = () => { preview = !preview; showContent(); }; const html = el('button', {}, '下载 HTML'); html.onclick = () => download(`${account?.name}-${question.name}.html`, result.html, 'text/html;charset=utf-8'); actions.append(toggle, html); }
    if (result.answer) { const raw = el('button', {}, '下载原文'); raw.onclick = () => download(`${account?.name}-${question.name}.txt`, result.answer); actions.append(raw); }
    const mark = el('select', { 'aria-label': `标记 ${account?.name || result.account_id} 的结果` }); for (const [value, text] of [['', '未标记'], ['pass', '通过'], ['fail', '不通过'], ['review', '待复核']]) mark.append(el('option', { value }, text)); mark.value = result.mark; mark.onchange = guard(async () => { await api(`results/${result.id}`, { method: 'PATCH', body: JSON.stringify({ mark: mark.value }) }); summary.mark = mark.value; container.dataset.revision = ''; }); actions.append(mark); card.append(actions); container.append(card);
  }
  if (!visible.length) container.append(el('div', { class: 'empty' }, '没有符合条件的结果'));
  $('#more-results').hidden = visible.length >= all.length;
}
async function boot() {
  const [catalog, suites] = await Promise.all([api('catalog'), api('suites')]); state.accounts = catalog.accounts; state.groups = catalog.groups; state.suites = suites;
  $('#login').hidden = true; $('#workbench').hidden = false;
  $('#platform-filter').replaceChildren(el('option', { value: '' }, '全部平台')); for (const p of [...new Set(state.accounts.map((a) => a.platform))]) $('#platform-filter').append(el('option', { value: p }, platformLabels[p] || p));
  $('#type-filter').replaceChildren(el('option', { value: '' }, '全部类型')); for (const t of new Set([...Object.keys(typeLabels), ...state.accounts.map((a) => accountType(a.type))])) $('#type-filter').append(el('option', { value: t }, typeLabels[t] || t));
  $('#group-filter').replaceChildren(el('option', { value: '' }, '全部分组'), el('option', { value: 'ungrouped' }, '未分配分组')); for (const g of state.groups) $('#group-filter').append(el('option', { value: g.id }, g.name));
  editSuite(suites[0] || blankSuite()); renderAccounts(); renderModels();
  $('#concurrency').value = savedSettings.concurrency || 4; $('#timeout').value = savedSettings.timeout_seconds || 300; $('#max-tokens').value = savedSettings.max_tokens || 0; $('#reasoning').value = savedSettings.reasoning || '';
  await refreshRuns();
}
document.querySelectorAll('[data-tab]').forEach((b) => { b.onclick = () => tab(b.dataset.tab); });
$('#suite-select').onchange = () => { const suite = state.suites.find((s) => s.id === $('#suite-select').value); if (suite) editSuite(suite); };
$('#suite-name').oninput = dirty;
$('#new-suite').onclick = () => editSuite({ id: '', name: '新题组', questions: [{ name: '问题 1', prompt: '', format: 'text' }] });
$('#copy-suite').onclick = () => { state.suite.name = $('#suite-name').value; editSuite({ ...structuredClone(state.suite), id: '', name: `${state.suite.name}（副本）` }); };
$('#delete-suite').onclick = guard(async () => { if (!state.suite.id) return; if (!confirm('删除此题组？已创建的任务和历史结果会保留。')) return; await api(`suites/${state.suite.id}`, { method: 'DELETE' }); state.suites = await api('suites'); editSuite(state.suites[0] || blankSuite()); });
$('#add-question').onclick = () => { state.suite.questions.push({ name: `问题 ${state.suite.questions.length + 1}`, prompt: '', format: 'text' }); renderQuestions(); dirty(); estimate(); };
$('#save-suite').onclick = guard(async () => { await saveSuite(); notice('题组已保存，下次可以直接使用'); });
for (const id of accountFilterIDs) $(`#${id}`).addEventListener(id === 'account-search' ? 'input' : 'change', renderAccounts);
$('#reset-filters').onclick = () => { for (const id of accountFilterIDs) $(`#${id}`).value = ''; renderAccounts(); };
$('#select-visible').onclick = () => { visibleAccounts().forEach((a) => state.selected.add(a.id)); renderAccounts(); selectionChanged(); };
$('#clear-selection').onclick = () => { state.selected.clear(); renderAccounts(); selectionChanged(); };
$('#start-run').onclick = guard(async () => { const button = $('#start-run'); if (state.busy) return; if (!state.selected.size) throw new Error('请先选择账户'); state.busy = true; button.disabled = true; try { const suite = await saveSuite(); const settings = settingsFromForm(); localStorage.setItem('account_eval_settings', JSON.stringify(settings)); const run = await api('runs', { method: 'POST', body: JSON.stringify({ name: $('#run-name').value, suite_id: suite.id, account_ids: [...state.selected], settings }) }); await selectRun(run.id); tab('history'); notice('任务已加入队列，完成后可在这里查看结果'); } finally { state.busy = false; button.disabled = false; } });
$('#refresh-runs').onclick = guard(refreshRuns);
$('#cancel-run').onclick = guard(async () => { await api(`runs/${state.detail.run.id}/cancel`, { method: 'POST' }); await refreshRuns(); });
$('#retry-run').onclick = guard(async () => { const button = $('#retry-run'); button.disabled = true; try { const run = await api(`runs/${state.detail.run.id}/retry`, { method: 'POST' }); await selectRun(run.id); await refreshRuns(); } finally { button.disabled = false; } });
$('#export-run').onclick = guard(async () => { const detail = state.detail; const results = []; for (const r of detail.results) results.push(await api(`results/${r.id}`)); download(`${detail.run.name}.json`, JSON.stringify({ run: detail.run, results }, null, 2), 'application/json'); });
for (const id of ['question-filter', 'result-filter']) $(`#${id}`).onchange = guard(async () => { state.resultLimit = 24; await renderResults(); });
$('#more-results').onclick = guard(async () => { state.resultLimit += 24; await renderResults(); });
$('#menu-url').value = `${location.origin}/account-eval/`;
$('#icon-file').onchange = guard(async () => { const file = $('#icon-file').files[0]; if (!file) return; if (file.size > 10240) throw new Error('SVG 文件不能超过 10KB'); const result = await api('icon', { method: 'POST', body: JSON.stringify({ svg: await file.text() }) }); state.icon = result.svg; $('#icon-preview').src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(state.icon)}`; notice('SVG 已校验，请点击更新入口保存'); });
$('#reset-icon').onclick = async () => { state.icon = await (await fetch('icon.svg')).text(); $('#icon-preview').src = 'icon.svg'; $('#icon-file').value = ''; };
$('#install-menu').onclick = guard(async () => { await api('menu', { method: 'POST', body: JSON.stringify({ label: $('#menu-label').value, url: $('#menu-url').value, icon_svg: state.icon }) }); notice('管理员入口已保存，刷新主站侧栏即可看到'); });
$('#reconnect').onclick = guard(boot);
boot().catch((error) => notice(error.message, true));
setInterval(() => { if (!document.hidden && !$('#workbench').hidden && !state.busy) refreshRuns().catch((error) => notice(error.message, true)); }, 2500);
