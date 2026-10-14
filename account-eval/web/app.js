'use strict';
document.documentElement.classList.toggle('iframe-embedded', window.self !== window.top);
const $ = (s) => document.querySelector(s);
const el = (tag, attrs = {}, text = '') => { const n = document.createElement(tag); for (const [k, v] of Object.entries(attrs)) { if (k === 'class') n.className = v; else n.setAttribute(k, v); } if (text) n.textContent = text; return n; };
const state = { suites: [], accounts: [], groups: [], selected: new Set(), suite: null, testSuiteID: '', runs: [], selectedRuns: { compose: localStorage.getItem('account_eval_current_run') || '', history: '' }, detail: null, detailView: '', cache: new Map(), resultLimit: 24, icon: '', busy: false, refreshing: false, tab: 'compose' };
const labels = { queued: '排队中', running: '执行中', cancelling: '正在取消', cancelled: '已取消', interrupted: '已中断', completed: '完成', completed_with_errors: '部分失败', failed: '失败', incomplete: '未完整输出', pass: '通过', fail: '不通过', review: '待复核' };
const reasoningLabels = { none: '不推理', minimal: '最低', low: '低', medium: '中', high: '高', xhigh: '更高', max: '最高' };
const platformLabels = { openai: 'OpenAI', anthropic: 'Anthropic', claude: 'Claude', gemini: 'Gemini', antigravity: 'Antigravity', grok: 'Grok', kimi: 'Kimi', zhipu: 'Zhipu GLM', deepseek: 'DeepSeek', minimax: 'MiniMax', opencode_go: 'OpenCode', openai_compatible: 'OpenAI Compatible', xai: 'XAI' };
const typeLabels = { oauth: 'OAuth', 'setup-token': 'Setup Token', apikey: 'API Key', bedrock: 'AWS Bedrock' };
const statusLabels = { active: '正常', inactive: '停用', error: '错误', rate_limited: '限流中', temp_unschedulable: '临时不可调度', unschedulable: '已停调' };
const accountFilterIDs = ['platform-filter', 'type-filter', 'status-filter', 'privacy-filter', 'group-filter', 'account-search'];
let savedSettings = {}; try { savedSettings = JSON.parse(localStorage.getItem('account_eval_settings') || '{}'); } catch {}
const manualModels = new Set();
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
function tab(name) {
  state.tab = name; document.querySelectorAll('.tab').forEach((n) => n.classList.toggle('active', n.id === name)); document.querySelectorAll('[data-tab]').forEach((n) => n.classList.toggle('active', n.dataset.tab === name));
  $('#run-detail').hidden = true;
  if (name === 'compose' || name === 'history') {
    $(`#${name === 'compose' ? 'live' : 'history'}-detail`).append($('#run-detail'));
    const id = state.selectedRuns[name];
    (id ? selectRun(id, name).then(refreshRuns) : refreshRuns()).catch((e) => notice(e.message, true));
  }
}
function testSuite() { return state.suites.find((s) => s.id === state.testSuiteID); }
function fillSuites() {
  for (const id of ['suite-select', 'test-suite']) { const select = $(`#${id}`); select.replaceChildren(); if (!state.suites.length) select.append(el('option', { value: '' }, '先到题库保存一个题组')); for (const suite of state.suites) select.append(el('option', { value: suite.id }, `${suite.name}${id === 'test-suite' ? ` · ${suite.questions.length} 道题` : ''}`)); }
  $('#suite-select').value = state.suite?.id || '';
  if (!testSuite()) state.testSuiteID = state.suites[0]?.id || '';
  $('#test-suite').value = state.testSuiteID; renderTestSuite();
}
function renderTestSuite() {
  const suite = testSuite(); $('#test-suite-summary').textContent = suite ? `${suite.questions.length} 道题 · 查看问题内容` : '选择一个已保存的题组';
  $('#test-suite-preview').replaceChildren(); for (const [i, q] of (suite?.questions || []).entries()) { const item = el('div', { class: 'preview-question' }); item.append(el('strong', {}, `${i + 1}. ${q.name}`), el('pre', {}, q.prompt)); $('#test-suite-preview').append(item); }
  $('#edit-test-suite').disabled = !suite; estimate();
}
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
function settingsFromForm() { return { models: { ...savedSettings.models }, protocols: { ...savedSettings.protocols }, concurrency: Number($('#concurrency').value), timeout_seconds: Number($('#timeout').value), max_tokens: Number($('#max-tokens').value), reasoning: $('#reasoning-label').hidden ? '' : $('#reasoning').value }; }
function selectedAccounts() { return state.accounts.filter((a) => state.selected.has(a.id)); }
function modelOptions(platform) {
  const accounts = selectedAccounts().filter((a) => a.platform === platform), groupIDs = new Set(accounts.flatMap((a) => a.group_ids));
  let groups = state.groups.filter((g) => groupIDs.has(g.id)); if (!groups.length) groups = state.groups;
  const channel = new Set(groups.flatMap((g) => g.models?.[platform] || [])), account = new Set(accounts.flatMap((a) => a.models || []));
  if (!accounts.length) for (const a of state.accounts.filter((a) => a.platform === platform)) for (const model of a.models || []) account.add(model);
  return { channel: [...channel].sort(), account: [...account].filter((id) => !channel.has(id)).sort() };
}
function latestTextModel(models) {
  const version = (id) => (id.match(/\d+(?:\.\d+)*(?:-\d{1,2}(?=$|[-_.]))*/)?.[0] || '').split(/[.-]/).map(Number);
  return models.filter((id) => !/(^|[-_/.])(image|audio|realtime|tts|whisper|dall[-_]?e|embeddings?|moderation|video)($|[-_/.])/i.test(id)).sort((a, b) => {
    const av = version(a), bv = version(b);
    for (let i = 0; i < Math.max(av.length, bv.length); i++) { const diff = (bv[i] || 0) - (av[i] || 0); if (diff) return diff; }
    return b.localeCompare(a, undefined, { numeric: true });
  })[0] || '';
}
function renderModels() {
  const platforms = [...new Set(selectedAccounts().map((a) => a.platform))]; if (!platforms.length) platforms.push('openai');
  const container = $('#model-settings'); container.replaceChildren(); savedSettings.models ||= {}; savedSettings.protocols ||= {};
  $('#protocol-settings').replaceChildren();
  for (const platform of platforms) {
    const box = el('div', { class: 'model-field' }), label = el('label', {}, `${platformLabels[platform] || platform} 模型`), select = el('select', { 'data-model-platform': platform });
    const options = modelOptions(platform), all = [...options.channel, ...options.account], saved = savedSettings.models[platform] || '', model = manualModels.has(platform) ? saved : latestTextModel(options.channel) || latestTextModel(options.account) || saved;
    for (const [kind, ids] of [['渠道配置', options.channel], ['账户配置', options.account]]) { if (!ids.length) continue; const group = el('optgroup', { label: kind }); for (const id of ids) group.append(el('option', { value: id }, id)); select.append(group); }
    if (model && !all.includes(model)) select.append(el('option', { value: model }, `${model}（已保存）`));
    if (!select.options.length) select.append(el('option', { value: '' }, '暂无模型目录，请自定义'));
    select.append(el('option', { value: '__custom__' }, '自定义模型…')); select.value = model; savedSettings.models[platform] = select.value;
    const custom = el('input', { placeholder: '输入账户实际支持的模型', 'aria-label': `${platformLabels[platform] || platform} 自定义模型`, hidden: '' });
    select.onchange = () => { manualModels.add(platform); custom.hidden = select.value !== '__custom__'; savedSettings.models[platform] = custom.hidden ? select.value : custom.value.trim(); if (!custom.hidden) custom.focus(); renderReasoning(); };
    custom.oninput = () => { manualModels.add(platform); savedSettings.models[platform] = custom.value.trim(); renderReasoning(); };
    label.append(select); box.append(label, custom, el('p', { class: 'hint model-hint' }, all.length ? `读取主站渠道与账户配置 · ${all.length} 个候选模型 · 默认选最新文本模型` : '主站尚未配置模型目录，可填写自定义模型。')); container.append(box);
    if (['openai', 'openai_compatible', 'grok', 'xai'].includes(platform)) { const label = el('label', {}, `${platformLabels[platform] || platform} API Key 协议`), select = el('select'); for (const [value, text] of [['auto', '自动'], ['responses', 'Responses'], ['chat', 'Chat Completions']]) select.append(el('option', { value }, text)); select.value = savedSettings.protocols[platform] || 'auto'; select.onchange = () => { savedSettings.protocols[platform] = select.value; }; label.append(select); $('#protocol-settings').append(label); }
  }
  renderReasoning();
}
function renderReasoning() {
  const select = $('#reasoning'), current = savedSettings.reasoning || '', accounts = selectedAccounts().filter((a) => ['openai', 'openai_compatible'].includes(a.platform));
  $('#reasoning-label').hidden = state.selected.size > 0 && !accounts.length; $('#reasoning-hint').hidden = $('#reasoning-label').hidden;
  let levels = Object.keys(reasoningLabels), known = 0;
  for (const a of accounts) { const info = a.model_metadata?.[savedSettings.models[a.platform]]; if (!info) continue; const supported = info.reasoning === false ? [] : info.supported_reasoning_levels?.length ? info.supported_reasoning_levels : null; if (!supported) continue; levels = known ? levels.filter((level) => supported.includes(level)) : supported; known++; }
  select.replaceChildren(el('option', { value: '' }, '模型默认')); for (const level of levels) select.append(el('option', { value: level }, `${level}${reasoningLabels[level] ? `（${reasoningLabels[level]}）` : ''}`));
  select.value = levels.includes(current) ? current : ''; savedSettings.reasoning = select.value;
  $('#reasoning-hint').textContent = known ? `已按 ${known} 个账户的模型能力筛选推理等级。${current && !levels.includes(current) ? '原等级不适用于此范围，已切回模型默认。' : ''}` : '包含 max；主站未提供模型能力时，以账户实际支持的等级为准。';
}
function estimate() { const count = state.selected.size * (testSuite()?.questions.length || 0); $('#run-estimate').textContent = `${state.selected.size} 个账户 × ${testSuite()?.questions.length || 0} 道题 = ${count} 次请求`; $('#start-run').disabled = state.busy || !state.selected.size || !testSuite(); }
async function refreshRuns() {
  if (state.refreshing) return;
  state.refreshing = true;
  try {
    state.runs = await api('runs');
    const active = state.runs.filter((r) => ['queued', 'running', 'cancelling'].includes(r.status)); $('#running-count').textContent = active.length ? `(${active.length})` : '';
    for (const view of ['compose', 'history']) {
      if (!state.runs.some((r) => r.id === state.selectedRuns[view])) state.selectedRuns[view] = (view === 'history' ? (state.tab === 'history' ? state.runs[0] : null) : active[0])?.id || '';
    }
    renderRunList($('#runs'), state.runs, 'history');
    const other = active.filter((r) => r.id !== state.selectedRuns.compose); $('#active-runs').hidden = !other.length; renderRunList($('#active-runs'), other, 'compose');
    if (state.tab !== 'compose' && state.tab !== 'history') return;
    const view = state.tab, id = state.selectedRuns[view];
    $('#live-empty').hidden = !!state.selectedRuns.compose; $('#history-empty').hidden = !!state.selectedRuns.history;
    if (!id) { $('#run-detail').hidden = true; $('#live-empty').textContent = '选择题组和账户后开始测试，结果会显示在这里。'; $('#history-empty').textContent = '选择一个历史批次查看结果。'; return; }
    if (state.detail?.run.id !== id || state.detailView !== view) await selectRun(id, view); else await refreshDetail();
  } finally { state.refreshing = false; }
}
function renderRunList(box, runs, view) {
  const revision = `${state.selectedRuns[view]}:${runs.map((r) => `${r.id}:${r.status}:${r.done}`).join('|')}`;
  if (box.dataset.revision === revision) return;
  box.dataset.revision = revision; box.replaceChildren();
  if (view === 'compose' && runs.length) box.append(el('p', { class: 'hint' }, '其他正在执行或等待的批次'));
  for (const r of runs) { const button = el('button', { class: `run-row${state.selectedRuns[view] === r.id ? ' selected' : ''}` }); const title = el('span', { class: 'run-title' }, r.name); title.append(el('small', {}, `${new Date(r.created_at).toLocaleString()} · ${r.accounts.length} 个账户 · ${r.suite.questions.length} 道题`)); button.append(title, el('span', { class: `badge ${r.status}` }, labels[r.status] || r.status), el('progress', { max: r.total || 1, value: r.done }), el('span', { class: 'muted' }, `${r.done}/${r.total}`)); button.onclick = guard(async () => { await selectRun(r.id, view); await refreshRuns(); }); box.append(button); }
  if (view === 'history' && !runs.length) box.append(el('div', { class: 'empty' }, '还没有历史记录。开始测试后会自动保存。'));
}
async function selectRun(id, view = state.tab) {
  state.selectedRuns[view] = id;
  if (view === 'compose') localStorage.setItem('account_eval_current_run', id);
  if (state.tab === view) { $('#run-detail').hidden = true; const empty = $(`#${view === 'compose' ? 'live' : 'history'}-empty`); empty.textContent = '正在加载批次结果…'; empty.hidden = false; }
  const detail = await api(`runs/${id}`);
  if (state.tab !== view || state.selectedRuns[view] !== id) return;
  state.cache.clear(); state.resultLimit = 24; state.detail = detail; state.detailView = view;
  $(`#${view === 'compose' ? 'live' : 'history'}-detail`).append($('#run-detail'));
  $('#question-filter').replaceChildren(); detail.run.suite.questions.forEach((q, i) => $('#question-filter').append(el('option', { value: i }, `${i + 1}. ${q.name}`))); $('#result-filter').value = '';
  $('#live-empty').hidden = !!state.selectedRuns.compose; $('#history-empty').hidden = !!state.selectedRuns.history;
  await renderDetail();
}
async function refreshDetail() { const view = state.tab, id = state.detail.run.id; const detail = await api(`runs/${id}`); if (state.tab !== view || state.detailView !== view || state.detail?.run.id !== id || state.selectedRuns[view] !== id) return; state.detail = detail; await renderDetail(); }
async function renderDetail() {
  if (state.detailView !== state.tab || state.selectedRuns[state.tab] !== state.detail.run.id) return;
  const { run } = state.detail; $('#run-detail').hidden = false; $('#detail-title').textContent = run.name;
  $('#detail-meta').textContent = `${labels[run.status] || run.status} · ${run.done}/${run.total} · 并发 ${run.settings.concurrency} · 单题 ${run.settings.timeout_seconds}s${run.parent_id ? ' · 失败项重试批次' : ''}`;
  $('#run-snapshot').textContent = JSON.stringify({ questions: run.suite.questions, settings: run.settings, accounts: run.accounts }, null, 2);
  const active = ['queued', 'running', 'cancelling'].includes(run.status); $('#cancel-run').hidden = !active; $('#cancel-run').disabled = run.status === 'cancelling'; $('#retry-run').hidden = active || !state.detail.results.some((r) => r.status !== 'completed');
  $('#detail-progress').max = run.total || 1; $('#detail-progress').value = run.done; $('#detail-progress-text').textContent = `${run.done} / ${run.total} 完成`;
  const counts = { queued: 0, running: 0, completed: 0, failed: 0 }; for (const r of state.detail.results) counts[r.status in counts ? r.status : 'failed']++;
  $('#result-counts').replaceChildren(); for (const [status, label] of [['queued', '等待'], ['running', '执行中'], ['completed', '完成'], ['failed', '失败 / 中断']]) { const count = el('div', { class: `count ${status}` }); count.append(el('strong', {}, String(counts[status])), el('span', {}, label)); $('#result-counts').append(count); }
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
  if (state.detail !== detail || state.detailView !== state.tab || state.selectedRuns[state.tab] !== runID || Number($('#question-filter').value) !== q || $('#result-filter').value !== filter) return;
  const container = $('#results'); const revision = `${detail.run.status}:${visible.map((r) => `${r.id}:${r.status}:${r.mark}`).join('|')}`;
  if (container.dataset.revision === `${runID}:${q}:${filter}:${revision}`) return;
  container.dataset.revision = `${runID}:${q}:${filter}:${revision}`; container.replaceChildren();
  for (const summary of visible) {
    const result = { ...summary, ...state.cache.get(summary.id), mark: summary.mark }, account = detail.run.accounts.find((a) => a.id === result.account_id), question = detail.run.suite.questions[q];
    const card = el('article', { class: 'result-card', 'data-platform': platformTone(account?.platform), 'data-status': result.status }), head = el('div', { class: 'card-head' }); head.append(el('h3', {}, `${account?.name || result.account_id} · #${result.account_id}`)); const row = el('div', { class: 'row' }); row.append(el('span', { class: `badge ${result.status}` }, labels[result.status] || result.status), el('small', {}, result.model || detail.run.settings.models[account?.platform] || '')); head.append(row); head.append(el('small', {}, `${(result.duration_ms / 1000).toFixed(1)}s · 输出 ${result.output_tokens || 0} tokens`)); card.append(head);
    if (result.error) card.append(el('div', { class: 'error-text' }, result.error));
    const content = el('div'); let preview = !!result.html;
    const showContent = () => { content.replaceChildren(); if (preview && result.html) { const frame = el('iframe', { sandbox: 'allow-scripts', title: `${account?.name || result.account_id} 的 HTML 结果`, referrerpolicy: 'no-referrer' }); frame.srcdoc = previewPolicy + result.html; content.append(frame); } else if (['queued', 'running'].includes(result.status)) { content.append(el('div', { class: `result-waiting ${result.status}` }, result.status === 'running' ? '正在请求上游，回答完成后显示在这里…' : detail.run.status === 'queued' ? '等待前面的批次结束后开始…' : '等待空闲并发，或该账户的上一题完成…')); } else { let answer = result.answer || '没有文本输出'; if (question.format === 'json') { try { answer = JSON.stringify(JSON.parse(answer), null, 2); } catch {} } content.append(el('pre', {}, answer)); } }; showContent(); card.append(content);
    const actions = el('div', { class: 'card-actions' });
    if (result.html) { const toggle = el('button', {}, '预览 / 原文'); toggle.onclick = () => { preview = !preview; showContent(); }; const html = el('button', {}, '下载 HTML'); html.onclick = () => download(`${account?.name}-${question.name}.html`, result.html, 'text/html;charset=utf-8'); actions.append(toggle, html); }
    if (result.answer) { const raw = el('button', {}, '下载原文'); raw.onclick = () => download(`${account?.name}-${question.name}.txt`, result.answer); actions.append(raw); }
    const mark = el('select', { 'aria-label': `标记 ${account?.name || result.account_id} 的结果` }); for (const [value, text] of [['', '未标记'], ['pass', '通过'], ['fail', '不通过'], ['review', '待复核']]) mark.append(el('option', { value }, text)); mark.value = result.mark; mark.disabled = ['queued', 'running'].includes(result.status); mark.onchange = guard(async () => { await api(`results/${result.id}`, { method: 'PATCH', body: JSON.stringify({ mark: mark.value }) }); summary.mark = mark.value; container.dataset.revision = ''; }); actions.append(mark); card.append(actions); container.append(card);
  }
  if (!visible.length) container.append(el('div', { class: 'empty' }, '没有符合条件的结果'));
  $('#more-results').hidden = visible.length >= all.length;
}
function fillCatalogFilters() {
  const previous = Object.fromEntries(['platform-filter', 'type-filter', 'group-filter'].map((id) => [id, $(`#${id}`).value]));
  $('#platform-filter').replaceChildren(el('option', { value: '' }, '全部平台')); for (const p of [...new Set(state.accounts.map((a) => a.platform))]) $('#platform-filter').append(el('option', { value: p }, platformLabels[p] || p));
  $('#type-filter').replaceChildren(el('option', { value: '' }, '全部类型')); for (const t of new Set([...Object.keys(typeLabels), ...state.accounts.map((a) => accountType(a.type))])) $('#type-filter').append(el('option', { value: t }, typeLabels[t] || t));
  $('#group-filter').replaceChildren(el('option', { value: '' }, '全部分组'), el('option', { value: 'ungrouped' }, '未分配分组')); for (const g of state.groups) $('#group-filter').append(el('option', { value: g.id }, g.name));
  for (const [id, value] of Object.entries(previous)) { const select = $(`#${id}`); select.value = [...select.options].some((o) => o.value === value) ? value : ''; }
}
async function boot() {
  const [catalog, suites] = await Promise.all([api('catalog'), api('suites')]); state.accounts = catalog.accounts; state.groups = catalog.groups; state.suites = suites;
  $('#login').hidden = true; $('#workbench').hidden = false; fillCatalogFilters();
  editSuite(suites[0] || blankSuite()); renderAccounts(); renderModels();
  $('#concurrency').value = savedSettings.concurrency || 4; $('#timeout').value = savedSettings.timeout_seconds || 300; $('#max-tokens').value = savedSettings.max_tokens || 0;
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
$('#use-suite').onclick = guard(async () => { const suite = await saveSuite(); state.testSuiteID = suite.id; fillSuites(); tab('compose'); notice('题组已保存，选择账户后即可开始测试'); });
$('#test-suite').onchange = () => { state.testSuiteID = $('#test-suite').value; renderTestSuite(); };
$('#reasoning').onchange = () => { savedSettings.reasoning = $('#reasoning').value; };
$('#open-library').onclick = () => tab('library');
$('#edit-test-suite').onclick = () => { if (testSuite()) editSuite(testSuite()); tab('library'); };
$('#back-to-test').onclick = () => tab('compose');
$('#open-history').onclick = () => tab('history');
$('#refresh-models').onclick = guard(async () => { const catalog = await api('catalog'); state.accounts = catalog.accounts; state.groups = catalog.groups; state.selected = new Set([...state.selected].filter((id) => state.accounts.some((a) => a.id === id))); fillCatalogFilters(); renderAccounts(); selectionChanged(); notice('已刷新主站渠道与账户模型配置'); });
for (const id of accountFilterIDs) $(`#${id}`).addEventListener(id === 'account-search' ? 'input' : 'change', renderAccounts);
$('#reset-filters').onclick = () => { for (const id of accountFilterIDs) $(`#${id}`).value = ''; renderAccounts(); };
$('#select-visible').onclick = () => { visibleAccounts().forEach((a) => state.selected.add(a.id)); renderAccounts(); selectionChanged(); };
$('#clear-selection').onclick = () => { state.selected.clear(); renderAccounts(); selectionChanged(); };
$('#start-run').onclick = guard(async () => { if (state.busy) return; if (!state.selected.size) throw new Error('请先选择账户'); const suite = testSuite(); if (!suite) throw new Error('请先到题库保存并选择一个题组'); state.busy = true; estimate(); try { const settings = settingsFromForm(); const run = await api('runs', { method: 'POST', body: JSON.stringify({ name: $('#run-name').value, suite_id: suite.id, account_ids: [...state.selected], settings }) }); localStorage.setItem('account_eval_settings', JSON.stringify(settings)); tab('compose'); await selectRun(run.id, 'compose'); await refreshRuns(); $('#live-section').scrollIntoView({ behavior: 'smooth', block: 'start' }); notice('任务已加入队列，下方会自动更新进度和结果'); } finally { state.busy = false; estimate(); } });
$('#refresh-runs').onclick = guard(refreshRuns);
$('#cancel-run').onclick = guard(async () => { await api(`runs/${state.detail.run.id}/cancel`, { method: 'POST' }); await refreshRuns(); });
$('#retry-run').onclick = guard(async () => { const button = $('#retry-run'); button.disabled = true; try { const run = await api(`runs/${state.detail.run.id}/retry`, { method: 'POST' }); tab('compose'); await selectRun(run.id, 'compose'); await refreshRuns(); $('#live-section').scrollIntoView({ behavior: 'smooth', block: 'start' }); } finally { button.disabled = false; } });
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
