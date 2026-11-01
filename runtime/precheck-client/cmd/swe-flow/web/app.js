'use strict';

// Unified pipeline: local tasks and personal prechecks are one queue. Stage
// ids come from the server (task.stage) or are derived here for prechecks that
// have no local task yet.
const STAGES = {
  ready: { label: '本地已完成', group: 'mine', step: 3, hint: '审核已完成、留在本地，平台开放时点击提交' },
  execute: { label: '待执行', group: 'mine', step: 1, hint: '在 Trae 执行冻结题面，回来补 Session 和轨迹' },
  prepare: { label: '待准备', group: 'mine', step: 0, hint: '预检已通过，准备独立源码工作区' },
  blocked: { label: '需核查', group: 'mine', step: -1, hint: '状态不明或材料冲突，先核对再继续' },
  review: { label: '验收中', group: 'wait', step: 2, hint: 'Codex 验收、取证并 dry-run，完成后转为待提交' },
  submitting: { label: '提交中', group: 'wait', step: 3, hint: '正在上传附件并创建平台提交' },
  submitted: { label: '质检中', group: 'wait', step: 4, hint: '已提交，等待平台质检' },
  precheck: { label: '预检中', group: 'wait', step: 0, hint: '题面在平台预检中，或需要返修' },
  rejected: { label: '质检退回', group: 'end', step: 4, hint: '平台质检未通过' },
  done: { label: '已通过', group: 'end', step: 5, hint: '平台质检通过' },
  dropped: { label: '已弃用', group: 'end', step: 5, hint: '不继续执行或提交平台，仅留存本地' },
};
const STAGE_ORDER = Object.keys(STAGES);
const STEPS = ['准备', '执行', '验收', '提交', '质检'];
const STATUS_LABEL = { initialized: '待绑定项目', project_selected: '待预检', precheck_draft: '预检草稿', precheck_submitted: '预检中', precheck_rejected: '预检未通过', precheck_passed: '预检已通过', trae_running: '待补执行材料', session_captured: 'Session 已登记', session_verified: 'Session 已核验', artifacts_verified: '材料已审核', dry_run_passed: 'dry-run 已通过', submitting: '提交中', submitted: '已提交', qc_pending: '质检中', qc_rejected: '质检未通过', qc_accepted: '质检通过', closed: '已关闭', local_only: '已弃用（仅本地）' };
const PLATFORM_LABEL = { PENDING_QC: '待质检', QC_RUNNING: '质检中', PENDING_FIX: '待修复', REJECTED: '已拒绝', QC_PASSED: '质检通过', PASSED: '已通过', ACCEPTED: '已采纳' };
const MATERIAL_STATUSES = ['precheck_passed', 'trae_running', 'session_captured', 'session_verified'];
const STATUS_RANK = ['initialized', 'project_selected', 'precheck_draft', 'precheck_submitted', 'precheck_rejected', 'precheck_passed', 'trae_running', 'session_captured', 'session_verified', 'artifacts_verified', 'dry_run_passed', 'submitting', 'submitted', 'qc_pending', 'qc_rejected', 'qc_accepted', 'closed', 'local_only'];

const app = {
  csrf: '', root: '', workRoot: '', tasks: [], prechecks: [], platform: null, accounts: null, accountShown: '', prechecksLoaded: false, sync: {},
  view: null, route: '', current: null, busy: false, generation: 0, form: null, submitTimer: 0, tickTimer: 0,
};
const $ = (id) => document.getElementById(id);
const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
const short = (hash) => hash ? String(hash).slice(0, 10) : '—';
const repoName = (url) => String(url || '').replace(/^https?:\/\/github\.com\//, '').replace(/\.git$/, '');
const statusLabel = (status) => STATUS_LABEL[status] || status || '未知';
const sessionStatusLabel = (t) => t?.session_verification_skipped && t.status === 'session_verified' ? 'Session 已保存（未校验）' : statusLabel(t.status);
const platformLabel = (status) => PLATFORM_LABEL[status] || status || '待查询';
const rank = (status) => STATUS_RANK.indexOf(status);
const storage = {
  get(key) { try { return localStorage.getItem(key); } catch { return null; } },
  set(key, value) { try { localStorage.setItem(key, value); } catch { /* per-viewer convenience only */ } },
};

// ---------- infrastructure ----------
let noticeTimer = 0;
function showNotice(message, isError = false) {
  const node = $('notice');
  clearTimeout(noticeTimer);
  node.hidden = !message;
  node.className = 'notice' + (isError ? ' error' : '');
  $('notice-text').textContent = message || '';
  if (message && !isError) noticeTimer = setTimeout(() => { node.hidden = true; }, 4200);
}
async function api(path, options = {}) {
  const headers = { ...(options.headers || {}) };
  if (options.body && !(options.body instanceof FormData)) headers['Content-Type'] = 'application/json';
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = app.csrf;
  let response;
  try { response = await fetch(path, { ...options, headers, cache: 'no-store' }); }
  catch { throw new Error('无法连接本地工作台，请确认服务仍在运行后重试。'); }
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `请求未完成（${response.status}），请刷新后重试。`);
  return data;
}
async function busy(button, action, label = '处理中…') {
  if (app.busy) return;
  app.busy = true;
  const old = button?.innerHTML;
  if (button) { button.disabled = true; button.textContent = label; }
  try { await action(); }
  catch (error) {
    showNotice(error.message, true);
    if ($('evidence-progress') && button?.id === 'save-evidence') $('evidence-progress').textContent = '未完成：' + error.message + '。输入已保留，修正后可重试。';
  } finally {
    app.busy = false;
    if (button?.isConnected) { button.disabled = false; button.innerHTML = old; }
  }
}
async function copyText(value, message) {
  try { await navigator.clipboard.writeText(value); showNotice(message); }
  catch { showNotice('复制失败，可展开预览后手动选中复制。', true); }
}
function dirty() { return !!document.querySelector('[data-editor]') || !!$('session-token')?.value || !!$('trace-file')?.files?.length || $('settings-page')?.dataset.dirty === 'true'; }
const confirmLeave = () => !dirty() || window.confirm('当前材料或字段尚未保存，离开会放弃这次输入。继续离开？');
function fmtTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
}
function elapsed(from) {
  const seconds = Math.max(0, Math.round((Date.now() - new Date(from).getTime()) / 1000));
  return seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`;
}

// ---------- data ----------
const platformClosed = () => !!(app.platform?.known && !app.platform.open);
const closedText = () => app.platform?.limited ? '平台本周期提交额度已满' : '平台暂停提交';
async function loadPlatform(refresh = false) {
  try { app.platform = await api('/api/platform' + (refresh ? '?refresh=1' : '')); }
  catch { /* display only; submit re-checks live */ }
  renderPlatform();
}
function renderLocalDone() {
  const n = items().filter((x) => !x.archived && x.stage === 'ready').length;
  const node = $('local-done');
  node.hidden = !n;
  node.innerHTML = `<b>${n}</b><span> 项本地已完成</span>`;
  node.setAttribute('aria-pressed', String(app.view.stages.length === 1 && app.view.stages[0] === 'ready'));
}
function renderAccountChip() {
  const node = $('account-chip');
  if (!node) return;
  const data = app.accounts;
  if (!data) { node.hidden = true; return; }
  const active = data.active || '';
  node.hidden = false;
  node.dataset.state = active ? 'on' : 'off';
  node.innerHTML = active ? `<span class="account-at" aria-hidden="true">@</span><b>${esc(active)}</b>` : '<b>未登录</b>';
  node.title = active ? `当前 SWE 用户 ${active}；点击切换` : '未登录 SWE 用户；点击设置账号';
  if (app.accountShown && active && app.accountShown !== active) {
    node.classList.remove('flash');
    void node.offsetWidth;
    node.classList.add('flash');
    window.setTimeout(() => node.classList.remove('flash'), 1000);
  }
  app.accountShown = active;
  renderAccountContext();
}
function renderAccountContext() {
  const node = $('overview-account');
  if (!node) return;
  const active = app.accounts?.active || '';
  node.hidden = !active;
  node.innerHTML = active ? `<span class="account-dot on" aria-hidden="true"></span>平台身份 <b>${esc(active)}</b><span>本地任务共享</span>` : '';
}
async function loadAccounts() {
  try { app.accounts = await api('/api/swe/accounts'); }
  catch { app.accounts = null; }
  renderAccountChip();
}
function scrollToSettingsAccount() {
  document.getElementById('settings-account')?.scrollIntoView({ block: 'start' });
}
function openAccountSettings() {
  if (location.hash === '#/settings') { scrollToSettingsAccount(); return; }
  navigate('/settings');
  let tries = 0;
  const attempt = () => {
    if (document.getElementById('settings-account')) scrollToSettingsAccount();
    else if (tries++ < 12) window.setTimeout(attempt, 100);
  };
  window.setTimeout(attempt, 100);
}
function renderPlatform() {
  const node = $('platform-state'), p = app.platform;
  node.hidden = !p?.known;
  if (!p?.known) return;
  node.dataset.state = p.open ? 'open' : 'closed';
  node.textContent = p.open ? '平台可提交' : (p.limited ? '提交额度已满' : '平台暂停提交');
  node.title = `${p.open ? '平台当前接受新提交' : closedText() + '；本地可继续验收到「待提交」，开放后再点提交'}。读取于 ${fmtTime(p.checked_at)}${p.error ? '（最近一次刷新失败）' : ''}。点击重新读取。`;
}

async function loadTasks() {
  const data = await api('/api/tasks');
  app.tasks = data.tasks || [];
}
async function loadPrechecks(refresh = false) {
  if (app.prechecksRequest) return app.prechecksRequest;
  app.prechecksRequest = (async () => {
    try {
      const data = await api('/api/prechecks' + (refresh ? '?refresh=1' : ''));
      app.prechecks = data.items || [];
      app.prechecksLoaded = true;
      app.sync = data;
    } catch (error) {
      app.sync = { ...app.sync, unavailable: true };
      throw error;
    } finally { renderSync(); }
  })();
  try { await app.prechecksRequest; } finally { app.prechecksRequest = null; }
}
function remoteStage(p) {
  if (p.status === 'PRECHECK_PASSED') return p.match_unverified || p.unverified_local_count || p.historical_evidence_count ? 'blocked' : 'prepare';
  return 'precheck';
}
function items() {
  const byId = new Map(app.prechecks.map((p) => [p.id, p]));
  const list = app.tasks.map((task) => {
    const remote = byId.get(task.precheck_id);
    return { id: 't:' + task.key, kind: 'task', route: '/tasks/' + task.key, stage: STAGES[task.stage] ? task.stage : 'blocked', name: task.display_name || task.task_id || '未命名任务', repo: task.repo_url, precheckId: task.precheck_id, difficulty: remote?.difficulty, archived: !!task.archived, task, remote };
  });
  for (const p of app.prechecks) {
    if (p.matches?.length) continue;
    const repo = repoName(p.repo_url).split('/').pop();
    list.push({ id: 'p:' + p.id, kind: 'remote', route: '/prechecks/' + p.id, stage: remoteStage(p), name: `${p.id}-${repo}`, repo: p.repo_url, precheckId: p.id, difficulty: p.difficulty, archived: !!p.archived, remote: p });
  }
  return list.sort((a, b) => STAGE_ORDER.indexOf(a.stage) - STAGE_ORDER.indexOf(b.stage) || (b.precheckId || 0) - (a.precheckId || 0));
}
function rowNote(item) {
  const t = item.task;
  if (item.kind === 'remote') {
    if (item.stage === 'blocked') return item.remote.match_unverified ? '平台详情未核实' : '发现历史执行材料';
    if (item.stage === 'prepare') return item.remote.design_recorded ? '可准备源码 · 出题设计已记录' : '可准备源码';
    return item.remote.reject_label || item.remote.status_label || item.remote.status;
  }
  switch (item.stage) {
    case 'ready': return platformClosed() ? '本地已完成 · 等平台额度' : '本地已完成 · 可提交';
    case 'execute': return !t.session_sha256 && !t.artifacts?.x_trae_trace ? '补 Session 与原生轨迹' : !t.session_sha256 ? '补 Session' : '补原生轨迹';
    case 'review': return t.status === 'dry_run_passed' ? (t.review_reason || '等待 Codex 审核记录') : `${sessionStatusLabel(t)} · Codex 验收中`;
    case 'submitting': return '正在提交…';
    case 'submitted': case 'done': case 'rejected': return `#${t.submission_id} · ${platformLabel(t.submission_status)}`;
    case 'blocked': return t.integrity_error ? '审计链待核查' : t.pending || t.status === 'submitting' ? '写请求结果未明' : t.next_step || '待核查';
    default: return t.next_step || sessionStatusLabel(t);
  }
}
function matches(item, query) {
  if (!query) return true;
  return [item.name, item.repo, item.precheckId, item.task?.task_id, item.remote?.prompt_excerpt, STAGES[item.stage].label].some((v) => String(v ?? '').toLowerCase().includes(query));
}
function chip(stage, text) { return `<span class="chip" data-stage="${stage}">${esc(text || STAGES[stage].label)}</span>`; }
// Sample type and duration markers come from the recorded platform fields
// (question_type, x_is_more_than_two_hours); nothing is inferred locally.
const SAMPLE_TYPE_TONES = { '有效轮数 > 100': 'long', '有效轮数 < 100 且 效果好': 'control', '有效轮数 < 100 且 效果差': 'weak' };
function rowTags(item) {
  const t = item.task;
  if (!t) return '';
  const tags = [];
  if (t.historical) tags.push(`<span class="tag" title="任务位于历史工作目录 ${esc(t.work_root || '')}；当前目录只接收新任务">历史目录</span>`);
  if (t.question_type) tags.push(`<span class="tag" data-type="${SAMPLE_TYPE_TONES[t.question_type] || 'other'}" title="提交字段 question_type">${esc(t.question_type)}</span>`);
  if (t.more_than_two_hours) {
    const over = t.more_than_two_hours === '是';
    tags.push(`<span class="tag" data-hours="${over ? 'over' : 'under'}" title="提交字段 是否超过两小时">${over ? '超 2h' : '未超 2h'}</span>`);
  }
  return tags.length ? `<span class="row-tags">${tags.join('')}</span>` : '';
}
// Shelf and unshelf share one endpoint, so the row button shows the direction
// it will take: an arrow into the tray, or back out of it.
const ARCHIVE_ICON = '<svg aria-hidden="true" width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2.2 4.6h11.6"/><path d="M3.4 4.6v7.6a.9.9 0 0 0 .9.9h7.4a.9.9 0 0 0 .9-.9V4.6"/><path d="M6.3 2.4h3.4a.7.7 0 0 1 .7.7v1.5H5.6V3.1a.7.7 0 0 1 .7-.7Z"/><path d="M8 7v3.4M6.7 9.1 8 10.4l1.3-1.3"/></svg>';
const RESTORE_ICON = '<svg aria-hidden="true" width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2.2 4.6h11.6"/><path d="M3.4 4.6v7.6a.9.9 0 0 0 .9.9h7.4a.9.9 0 0 0 .9-.9V4.6"/><path d="M6.3 2.4h3.4a.7.7 0 0 1 .7.7v1.5H5.6V3.1a.7.7 0 0 1 .7-.7Z"/><path d="M8 10.4V7M6.7 8.3 8 7l1.3 1.3"/></svg>';
function renderSync() {
  const s = app.sync || {};
  const node = $('sync-state');
  const at = s.synced_at ? new Date(s.synced_at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : '';
  node.dataset.state = s.unavailable || s.sync_error ? 'error' : s.refreshing ? 'busy' : s.stale ? 'stale' : 'ok';
  $('sync-text').textContent = s.unavailable ? '预检暂不可用' : s.refreshing ? '预检同步中' : at ? `预检 ${at}${s.sync_error ? ' · 同步失败' : ''}` : '读取中';
  node.title = '预检列表每 5 分钟后台同步；准备源码、复制题面与提交仍实时核验。点击立即刷新。';
}

// ---------- view: scope tabs, filters, sort ----------
const DEFAULT_VIEW = { scope: 'mine', stages: [], repos: [], difficulty: [], kind: [], rounds: '', sort: 'stage' };
const ROUNDS = [['', '不限'], ['gt100', '> 100 轮'], ['le100', '≤ 100 轮'], ['none', '未记录']];
const KINDS = [['task', '本地任务'], ['remote', '未建任务的预检']];
const SORTS = [['stage', '按阶段'], ['id-desc', '预检编号 新→旧'], ['id-asc', '预检编号 旧→新'], ['rounds-desc', '有效轮数 高→低']];
function loadView() {
  try { return { ...DEFAULT_VIEW, ...JSON.parse(storage.get('swe-view') || '{}') }; } catch { return { ...DEFAULT_VIEW }; }
}
function saveView() { storage.set('swe-view', JSON.stringify(app.view)); }
const repoKey = (item) => repoName(item.repo) || '—';
const difficultyKey = (item) => item.difficulty || '未标注';
// The archive is a shelf, not a stage: an archived item keeps its real stage
// and status, only the scope decides which shelf it is read from.
const shelved = () => app.view.scope === 'archive' && !app.view.stages.length;
function passes(item, { scope = true } = {}) {
  const v = app.view, rounds = item.task?.rounds;
  if (scope && (shelved() ? !item.archived : item.archived)) return false;
  if (scope && !shelved() && !v.stages.length && v.scope !== 'all' && STAGES[item.stage].group !== v.scope) return false;
  if (v.stages.length && !v.stages.includes(item.stage)) return false;
  if (v.repos.length && !v.repos.includes(repoKey(item))) return false;
  if (v.difficulty.length && !v.difficulty.includes(difficultyKey(item))) return false;
  if (v.kind.length && !v.kind.includes(item.kind)) return false;
  if (v.rounds === 'gt100' && !(rounds > 100)) return false;
  if (v.rounds === 'le100' && !(rounds > 0 && rounds <= 100)) return false;
  if (v.rounds === 'none' && rounds) return false;
  return true;
}
function filterCount() { const v = app.view; return v.stages.length + v.repos.length + v.difficulty.length + v.kind.length + (v.rounds ? 1 : 0); }
function visibleItems() {
  const query = $('search').value.trim().toLowerCase();
  const list = items().filter((item) => passes(item) && matches(item, query));
  const id = (x) => x.precheckId || 0;
  if (app.view.sort === 'id-desc') list.sort((a, b) => id(b) - id(a));
  if (app.view.sort === 'id-asc') list.sort((a, b) => id(a) - id(b));
  if (app.view.sort === 'rounds-desc') list.sort((a, b) => (b.task?.rounds || 0) - (a.task?.rounds || 0) || id(b) - id(a));
  return list;
}
function setView(patch) {
  Object.assign(app.view, patch);
  if (patch.stages?.length) app.view.scope = 'all';
  saveView();
  renderRail();
  renderList();
}
function setScope(scope) { setView({ scope, stages: [] }); }
function renderRail() {
  const query = $('search').value.trim().toLowerCase();
  const pool = items().filter((item) => passes(item, { scope: false }) && matches(item, query));
  const live = pool.filter((x) => !x.archived), archived = pool.filter((x) => x.archived);
  const count = (scope) => scope === 'archive' ? archived.length : scope === 'all' ? live.length : live.filter((x) => STAGES[x.stage].group === scope).length;
  const active = app.view.stages.length ? '' : app.view.scope;
  $('scopes').innerHTML = [['mine', '待处理'], ['wait', '等待中'], ['end', '已结束'], ['all', '全部'], ['archive', '归档']].map(([id, name]) => `<button type="button" role="tab" data-scope="${id}" aria-selected="${active === id}" class="${count(id) ? 'has' : ''}">${name}<b>${count(id)}</b></button>`).join('');
  $('scopes').querySelectorAll('[data-scope]').forEach((node) => node.addEventListener('click', () => setScope(node.dataset.scope)));
  const n = filterCount();
  $('filter-count').textContent = n || '';
  $('filter-toggle').classList.toggle('on', n > 0);
  $('sort').value = app.view.sort;
  renderActiveFilters();
  if (!$('filter-panel').hidden) renderFilterPanel();
  renderLocalDone();
  syncHeadHeight();
}
function syncHeadHeight() {
  const head = document.querySelector('.queue-head');
  const offset = matchMedia('(max-width: 900px)').matches ? 52 : 0;
  $('list').style.setProperty('--head-h', `${head.offsetHeight + offset}px`);
}
function renderActiveFilters() {
  const v = app.view, chips = [];
  v.stages.forEach((x) => chips.push(['stages', x, STAGES[x]?.label || x]));
  v.repos.forEach((x) => chips.push(['repos', x, x.split('/').pop()]));
  v.difficulty.forEach((x) => chips.push(['difficulty', x, x]));
  v.kind.forEach((x) => chips.push(['kind', x, KINDS.find(([id]) => id === x)?.[1] || x]));
  if (v.rounds) chips.push(['rounds', v.rounds, ROUNDS.find(([id]) => id === v.rounds)[1]]);
  const node = $('active-filters');
  node.hidden = !chips.length;
  node.innerHTML = chips.map(([key, value, name]) => `<button type="button" class="active-chip" data-key="${key}" data-value="${esc(value)}" title="移除筛选">${esc(name)}<span aria-hidden="true">×</span></button>`).join('') + (chips.length ? '<button type="button" class="clear-filters" id="clear-filters">清除</button>' : '');
  node.querySelectorAll('.active-chip').forEach((chipNode) => chipNode.addEventListener('click', () => {
    const { key, value } = chipNode.dataset;
    setView(key === 'rounds' ? { rounds: '' } : { [key]: app.view[key].filter((x) => x !== value) });
  }));
  $('clear-filters')?.addEventListener('click', () => setView({ stages: [], repos: [], difficulty: [], kind: [], rounds: '' }));
}
function renderFilterPanel() {
  const all = items(), v = app.view;
  const facet = (key, values, label) => `<fieldset><legend>${label}</legend><div class="facet">${values.map(([value, name, n]) => `<button type="button" class="facet-chip" data-key="${key}" data-value="${esc(value)}" aria-pressed="${key === 'rounds' ? v.rounds === value : v[key].includes(value)}"${key === 'stages' ? ` data-stage="${value}"` : ''}>${key === 'stages' ? '<i aria-hidden="true"></i>' : ''}${esc(name)}${n === undefined ? '' : `<b>${n}</b>`}</button>`).join('')}</div></fieldset>`;
  const tally = (fn) => { const map = new Map(); all.forEach((x) => { const k = fn(x); map.set(k, (map.get(k) || 0) + 1); }); return map; };
  const repos = [...tally(repoKey)].sort((a, b) => b[1] - a[1]).map(([k, n]) => [k, k.split('/').pop(), n]);
  const diffs = [...tally(difficultyKey)].map(([k, n]) => [k, k, n]);
  const stages = STAGE_ORDER.map((id) => [id, STAGES[id].label, all.filter((x) => x.stage === id).length]).filter(([, , n]) => n);
  const kinds = KINDS.map(([id, name]) => [id, name, all.filter((x) => x.kind === id).length]);
  $('filter-panel').innerHTML = facet('stages', stages, '阶段') + facet('repos', repos, '仓库') + `<div class="facet-row">${facet('difficulty', diffs, '难度')}${facet('kind', kinds, '来源')}</div>` + facet('rounds', ROUNDS.map(([id, name]) => [id, name]), '有效轮数') +
    `<div class="panel-foot"><button type="button" class="ghost small" id="panel-clear">清除全部</button><button type="button" class="primary small" id="panel-done">完成</button></div>`;
  $('filter-panel').querySelectorAll('.facet-chip').forEach((node) => node.addEventListener('click', () => {
    const { key, value } = node.dataset;
    if (key === 'rounds') { setView({ rounds: value }); return; }
    const current = app.view[key];
    setView({ [key]: current.includes(value) ? current.filter((x) => x !== value) : [...current, value] });
  }));
  $('panel-clear').addEventListener('click', () => setView({ stages: [], repos: [], difficulty: [], kind: [], rounds: '' }));
  $('panel-done').addEventListener('click', () => toggleFilterPanel(false));
}
function toggleFilterPanel(open = $('filter-panel').hidden) {
  $('filter-panel').hidden = !open;
  $('filter-toggle').setAttribute('aria-expanded', String(open));
  if (open) renderFilterPanel();
}
function renderList() {
  const visible = visibleItems();
  const total = items().length;
  $('result-count').textContent = visible.length === total ? `${total} 项` : `${visible.length} / ${total} 项`;
  const list = $('list');
  if (!visible.length) {
    const query = $('search').value.trim();
    const filtered = filterCount() > 0;
    list.innerHTML = `<div class="list-empty">${query || filtered ? `<p>没有符合条件的任务。</p><button type="button" class="ghost small" id="empty-reset">清空搜索和筛选</button>` : app.view.scope === 'archive' ? '<p>还没有归档的任务。</p><p class="minor">列表每行的归档按钮会把任务移到这里，任何状态都可以归档；点同一位置可恢复。</p>' : app.view.scope === 'mine' ? '<p>没有需要你处理的事项。</p><p class="minor">Codex 审核完成后，任务会回到这里等你提交。</p>' : !app.prechecksLoaded ? '<p>正在读取预检缓存…</p>' : '<p>这里暂时没有任务。</p>'}</div>`;
    $('empty-reset')?.addEventListener('click', () => { $('search').value = ''; setView({ stages: [], repos: [], difficulty: [], kind: [], rounds: '' }); });
    return;
  }
  const grouped = app.view.sort === 'stage';
  let html = '', last = '';
  for (const item of visible) {
    if (grouped && item.stage !== last) { last = item.stage; html += `<div class="list-section" data-stage="${item.stage}">${esc(STAGES[item.stage].label)}<span>${visible.filter((x) => x.stage === item.stage).length}</span></div>`; }
    const meta = [repoName(item.repo).split('/').pop(), item.difficulty, item.task?.rounds ? `${item.task.rounds} 轮` : ''].filter(Boolean).join(' · ');
    const label = item.archived ? '恢复' : '归档';
    html += `<div class="row-item" data-stage="${item.stage}" data-archived="${item.archived ? 'true' : 'false'}"><button type="button" class="row" role="option" data-route="${esc(item.route)}" data-stage="${item.stage}" aria-selected="${app.current?.id === item.id}"><span class="row-top"><b>${esc(item.name)}</b>${grouped ? '' : chip(item.stage)}</span><span class="row-note">${esc(rowNote(item))}</span><span class="row-meta">${esc(meta)}</span>${rowTags(item)}</button><button type="button" class="row-archive" data-archive="${esc(item.id)}" aria-label="${label} ${esc(item.name)}" title="${label}${item.archived ? '：回到原队列' : '：可在「归档」中恢复'}">${item.archived ? RESTORE_ICON : ARCHIVE_ICON}</button></div>`;
  }
  list.innerHTML = html;
  list.querySelectorAll('[data-route]').forEach((node) => node.addEventListener('click', () => navigate(node.dataset.route)));
  list.querySelectorAll('[data-archive]').forEach((node) => node.addEventListener('click', (event) => {
    event.stopPropagation();
    const item = items().find((x) => x.id === node.dataset.archive);
    if (item) toggleArchive(item, node);
  }));
}

// Shelving never changes a task's stage, so a local task is archived through
// its own revision-checked state file; a platform precheck that never became a
// task has no such file and keeps its shelf entry in the workbench settings.
async function toggleArchive(item, button) {
  const archived = !item.archived;
  await busy(button, async () => {
    if (item.kind === 'task') {
      await api(`/api/tasks/${item.task.key}/archive`, { method: 'POST', body: JSON.stringify({ archived }) });
      await loadTasks();
    } else {
      await api(`/api/prechecks/${item.precheckId}/archive`, { method: 'POST', body: JSON.stringify({ archived }) });
      await loadPrechecks();
    }
    renderRail();
    renderList();
    if (app.current?.id === item.id) await renderRoute();
    showNotice(archived ? `已归档「${item.name}」。可在「归档」中恢复。` : `已恢复「${item.name}」。`);
  });
}

// ---------- routing ----------
function navigate(route) {
  if (app.busy) { showNotice('当前操作仍在处理，请完成后再切换。'); return; }
  if (route === app.route) return;
  if (!confirmLeave()) return;
  history.pushState({ swe: true }, '', '#' + route);
  renderRoute();
}
function resolveRoute() {
  const route = location.hash.slice(1) || '/tasks';
  if (route === '/settings') return { route, settings: true };
  const match = /^\/(tasks|prechecks)(?:\/([a-zA-Z0-9]+))?$/.exec(route);
  if (!match) return { route: '/tasks', item: null };
  if (!match[2]) return { route, list: match[1], item: null };
  const id = match[1] === 'tasks' ? 't:' + match[2] : 'p:' + match[2];
  let item = items().find((x) => x.id === id) || null;
  // A precheck that was turned into a task keeps working through its old link.
  if (!item && match[1] === 'prechecks') item = items().find((x) => x.kind === 'task' && String(x.precheckId) === match[2] && x.remote?.matches?.length === 1) || null;
  return { route, item, missing: !item, kind: match[1] };
}
async function renderRoute() {
  const generation = ++app.generation;
  stopSubmitPolling();
  const resolved = resolveRoute();
  app.route = resolved.route;
  if (resolved.settings) {
    app.current = null;
    document.body.dataset.view = 'settings';
    renderRail();
    renderList();
    await renderSettings();
    return;
  }
  if (resolved.list === 'prechecks' && !resolved.item && app.lastList !== 'prechecks') { app.view = { ...app.view, scope: 'all', stages: ['prepare'] }; }
  app.lastList = resolved.list || app.lastList;
  app.current = resolved.item;
  document.body.dataset.view = resolved.item || resolved.missing ? 'detail' : 'list';
  renderRail();
  renderList();
  if (resolved.missing) {
    if (resolved.kind === 'prechecks' && !app.prechecksLoaded) { $('detail').innerHTML = '<div class="detail-empty"><p>正在读取预检缓存…</p></div>'; return; }
    $('detail').innerHTML = backBar() + '<div class="detail-empty"><h2>没有找到这项任务</h2><p>它可能已归档或预检列表尚未同步。点击右上角同步状态刷新。</p></div>';
    wireBack();
    return;
  }
  if (!resolved.item) { renderOverview(); return; }
  await renderDetail(resolved.item, generation);
}
function backBar() { return '<button type="button" class="back" id="back">← 返回列表</button>'; }
function wireBack() { $('back')?.addEventListener('click', () => navigate('/tasks')); }

// ---------- overview (nothing selected) ----------
function renderOverview() {
  const all = items().filter((x) => !x.archived);
  const line = (stage, text) => {
    const list = all.filter((x) => x.stage === stage);
    if (!list.length) return '';
    return `<button type="button" class="todo" data-stage="${stage}" data-route="${esc(list[0].route)}"><span class="todo-count">${list.length}</span><span><b>${esc(text)}</b><small>${esc(list.slice(0, 3).map((x) => x.name).join('、'))}${list.length > 3 ? ' 等' : ''}</small></span><span class="todo-go" aria-hidden="true">→</span></button>`;
  };
  const todo = line('ready', '本地已完成，平台开放时点击提交') + line('execute', '待在 Trae 执行或补材料') + line('blocked', '需要先核查') + line('prepare', '预检可以准备源码');
  const waiting = ['review', 'submitting', 'submitted'].map((s) => { const n = all.filter((x) => x.stage === s).length; return n ? `${STAGES[s].label} ${n}` : ''; }).filter(Boolean).join(' · ');
  const mine = all.filter((x) => STAGES[x.stage].group === 'mine').length;
  $('detail').innerHTML = `<div class="overview"><h2>${mine ? `${mine} 项需要你处理` : '暂时没有需要你处理的事项'}</h2><p id="overview-account" class="account-context" hidden></p>${waiting ? `<p class="minor">等待中：${esc(waiting)}</p>` : ''}${todo ? `<div class="todo-list">${todo}</div>` : ''}<details class="flow-legend"><summary>流程说明</summary><ol>${['预检通过后准备独立源码工作区', '在 Trae 输入冻结题面执行一次，回来补 Session 与原生轨迹', '复制提示词交给 Codex：验收、取证、dry-run，写入审核记录后停下', '任务进入「待提交」，确认后点击提交，工作台自动提交并回显平台回执'].map((t) => `<li>${esc(t)}</li>`).join('')}</ol><p class="hint">快捷键：/ 搜索，J / K 上下切换，Esc 返回。</p></details></div>`;
  renderAccountContext();
  $('detail').querySelectorAll('[data-route]').forEach((node) => node.addEventListener('click', () => { setScope('mine'); navigate(node.dataset.route); }));
}

// ---------- detail ----------
function pipeline(stage) {
  const step = STAGES[stage].step;
  return `<ol class="pipeline" data-stage="${stage}" aria-label="任务进度">${STEPS.map((name, i) => `<li class="${step < 0 ? '' : i < step ? 'done' : i === step ? 'now' : ''}"><span>${esc(name)}</span></li>`).join('')}</ol>`;
}
function detailHead(item) {
  const t = item.task, r = item.remote;
  const eyebrow = [item.precheckId ? `预检 #${item.precheckId}` : '本地任务', item.difficulty, t ? (t.execution_mode === 'manual' ? '手动执行' : '') : '尚未建任务'].filter(Boolean).join(' · ');
  const historyNote = t?.historical ? `<p class="minor history-note">该任务位于历史工作目录 <code class="path">${esc(t.work_root)}</code>；当前全局目录是 <code class="path">${esc(app.workRoot)}</code>。已有状态和材料仍按原目录读取，只有新任务写入当前目录。</p>` : '';
  const shelfNote = item.archived ? '<p class="minor">已归档：仍可正常处理，只是不在「待处理 / 等待中 / 已结束 / 全部」中出现。在「归档」里点恢复即可回到原队列。</p>' : '';
  const tags = `<div class="detail-tags">${item.archived ? '<span class="tag">已归档</span>' : ''}${chip(item.stage)}</div>`;
  return `${backBar()}<header class="detail-head"><div><p class="eyebrow">${esc(eyebrow)}</p><h2 class="title">${esc(item.name)}</h2>${item.repo ? `<a class="repo" href="${esc(item.repo)}" target="_blank" rel="noreferrer noopener">${esc(repoName(item.repo))} ↗</a>` : ''}</div>${tags}</header>${shelfNote}${historyNote}${pipeline(item.stage)}<div id="detail-update" class="update-bar" hidden>任务状态有更新。<button type="button" class="ghost small" id="apply-update">刷新详情</button></div>`;
}
const TASK_TABS = [['materials', '材料'], ['source', '题面与源码'], ['form', '提交字段'], ['design', '设计'], ['handoff', 'Codex 交接']];
const REMOTE_TABS = [['prompt', '冻结题面'], ['design', '出题设计']];
async function renderDetail(item, generation) {
  const tabs = item.kind === 'task' ? TASK_TABS : REMOTE_TABS;
  const remembered = history.state?.tab;
  const tab = tabs.some(([id]) => id === remembered) ? remembered : tabs[0][0];
  $('detail').innerHTML = detailHead(item) + `<section class="next" id="next" data-stage="${item.stage}"><p class="loading">读取中…</p></section>` +
    `<div class="tabs" role="tablist">${tabs.map(([id, name]) => `<button type="button" role="tab" data-tab="${id}" aria-selected="${id === tab}">${esc(name)}</button>`).join('')}</div><div class="tab-panel" id="tab-panel" role="tabpanel"></div>`;
  wireBack();
  $('apply-update')?.addEventListener('click', () => { if (dirty()) { showNotice('请先保存或取消当前输入。'); return; } renderRoute(); });
  $('detail').querySelectorAll('[data-tab]').forEach((node) => node.addEventListener('click', () => {
    if (!confirmLeave()) return;
    history.replaceState({ ...history.state, tab: node.dataset.tab }, '', location.href);
    $('detail').querySelectorAll('[data-tab]').forEach((n) => n.setAttribute('aria-selected', String(n === node)));
    renderTab(item, node.dataset.tab);
  }));
  renderTab(item, tab);
  if (item.kind === 'task') await renderTaskNext(item, generation);
  else await renderRemoteNext(item);
}
function renderTab(item, tab) {
  const panel = $('tab-panel');
  panel.innerHTML = '<p class="loading">读取中…</p>';
  const t = item.task;
  if (tab === 'materials') return renderMaterials(t);
  if (tab === 'source') return loadExecution(t);
  if (tab === 'form') return loadForm(t);
  if (tab === 'design') return item.kind === 'task' ? loadDesign(t) : loadRemoteDesign(item.remote);
  if (tab === 'handoff') return loadHandoff(t);
  if (tab === 'prompt') return loadRemotePrompt(item.remote);
}
function proof(name, done, detail, state) {
  return `<li class="proof" data-state="${state || (done ? 'ok' : 'wait')}"><i aria-hidden="true"></i><b>${esc(name)}</b><span>${esc(detail)}</span></li>`;
}
function fact(name, value, mono = false) { return `<div class="fact"><dt>${esc(name)}</dt><dd class="${mono ? 'mono' : ''}">${esc(value ?? '—')}</dd></div>`; }

// ---------- next step panels: tasks ----------
async function renderTaskNext(item, generation) {
  const t = item.task, next = $('next');
  const stage = item.stage;
  if (stage === 'execute') return renderExecuteNext(t);
  if (stage === 'review') return renderReviewNext(t);
  if (stage === 'ready' || stage === 'submitting') return renderReadyNext(t, generation);
  if (stage === 'submitted' || stage === 'done' || stage === 'rejected') return renderReceiptNext(t, stage);
  if (stage === 'blocked') {
    const job = t.submit_job;
    const message = t.integrity_error ? '任务审计链不完整，页面保持只读。交给 Codex 复核现有材料。'
      : t.pending || t.status === 'submitting' ? '平台写请求结果未明，禁止重试。交给 Codex 用平台真实回执 reconcile。'
        : t.next_step || '状态需要核对。';
    next.innerHTML = `<h3>先核查，再继续</h3><p class="warn-text">${esc(message)}</p>${job?.message ? `<p class="minor">最近一次提交：${esc(job.message)}</p>` : ''}<div class="actions"><button type="button" class="primary" id="copy-next-handoff">复制核查提示词</button></div>`;
    $('copy-next-handoff').addEventListener('click', () => copyHandoff(t));
    return;
  }
  if (stage === 'dropped') return renderDroppedNext(t, generation);
  next.innerHTML = `<h3>${esc(sessionStatusLabel(t))}</h3><p class="minor">${esc(t.next_step || STAGES[stage].hint)}</p>`;
}
// A task the collection policy will not take is closed on purpose: the platform
// would reject it, so nothing further is asked for. It stays editable.
async function renderDroppedNext(t, generation) {
  const next = $('next');
  const reason = t.review_reason || STAGES.dropped.hint;
  next.innerHTML = `<h3>已弃用，仅留存本地</h3><p class="warn-text">${esc(reason)}</p><p class="minor">不会再提交到平台，也不需要继续跑取证和 dry-run。需要重新处理时可以打开修改，把材料或轮数改正后再核验。</p><div class="actions"><button type="button" class="primary" id="reopen-local-only">打开并修改</button></div>`;
  $('reopen-local-only').addEventListener('click', async () => {
    try {
      await api(`/api/tasks/${t.key}/local-only`, { method: 'POST', body: JSON.stringify({ local: false }) });
      showNotice('任务已重新打开，可以继续修改材料；核验后若仍不满足收录条件会再次弃用。');
      await refreshAll(true);
    } catch (error) { showNotice(error.message, true); }
  });
}
// A task can be dropped by hand before it is ever submitted: not every task is
// worth finishing. It stays editable from the "已弃用" panel.
function abandonAction(t) {
  return `<div class="actions"><button type="button" class="ghost small" id="abandon-task">这个任务不继续，标记废弃</button></div>`;
}
function wireAbandon(t) {
  const button = $('abandon-task');
  if (!button) return;
  button.addEventListener('click', async () => {
    if (!window.confirm('标记废弃后不会再提示执行和提交，任务会留在「已结束」；之后可以打开并修改。确定？')) return;
    try {
      await api(`/api/tasks/${t.key}/local-only`, { method: 'POST', body: JSON.stringify({ local: true }) });
      showNotice('已标记为废弃，任务只留在本地。');
      await refreshAll(true);
    } catch (error) { showNotice(error.message, true); }
  });
}

// Per-viewer default for whether Codex may submit after review.
const submitPref = () => storage.get('swe-submit-after-review') === '1';
function submitSwitch(id) {
  return `<label class="switch" title="关闭时 Codex 审核完成后留在本地；开启时审核通过后直接提交到 SWE 平台（平台未开放则自动留在本地）"><input type="checkbox" id="${id}" ${submitPref() ? 'checked' : ''}><span class="track" aria-hidden="true"></span><span class="switch-text">审核通过后直接提交到 SWE 平台</span></label>`;
}
function wireSwitch(id, onChange) {
  $(id)?.addEventListener('change', () => { storage.set('swe-submit-after-review', $(id).checked ? '1' : '0'); onChange?.(); });
}
async function copyHandoff(t, submit = false) {
  try {
    const data = await api(`/api/tasks/${t.key}/handoff${submit ? '?submit=1' : ''}`);
    await copyText(data.codex_text, submit ? '提示词已复制：审核通过后 Codex 会直接提交到平台。' : '提示词已复制：Codex 审核完成后留在本地，不提交。');
  } catch (error) { showNotice(error.message, true); }
}
// The submit entry is always visible so the platform step is discoverable;
// it stays locked until every gate below is met.
function submitLocked(t) {
  const hasMaterials = !!t.session_sha256 && !!t.artifacts?.x_trae_trace;
  const dryRun = t.status === 'dry_run_passed';
  const gates = [['补充 Session 与原生轨迹', hasMaterials], ['Codex 完成材料审核与 dry-run', dryRun], ['Codex 写入与当前材料一致的审核记录', !!t.review_ready]];
  const next = gates.find(([, ok]) => !ok);
  return `<div class="submit-locked"><div class="submit-locked-head"><button type="button" class="submit" disabled aria-describedby="submit-gates">提交到 SWE 平台</button><span class="minor">${next ? `还差：${esc(next[0])}` : '已满足条件，刷新后可提交'}</span></div><ol class="gates" id="submit-gates">${gates.map(([name, ok]) => `<li data-ok="${ok}"><i aria-hidden="true"></i>${esc(name)}</li>`).join('')}<li data-ok="false"><i aria-hidden="true"></i>你确认后点击提交，自动上传并回显平台回执</li></ol>${platformClosed() ? `<p class="warn-text gate-note">${esc(closedText())}：本地可以先完成以上步骤，开放后再提交。</p>` : ''}</div>`;
}
async function renderExecuteNext(t) {
  const next = $('next');
  const canSupply = !t.integrity_error && !t.pending && MATERIAL_STATUSES.includes(t.status);
  const hasSession = !!t.session_sha256, hasTrace = !!t.artifacts?.x_trae_trace;
  next.innerHTML = `<h3>在 Trae 执行，然后补两项材料</h3><ol class="runbook">
    <li data-state="ok"><span class="num">1</span><div><b>用 Trae 打开模型源码目录</b><code id="exec-workspace" class="path">读取中…</code></div><button type="button" class="ghost small" id="copy-workspace" disabled>复制路径</button></li>
    <li data-state="ok"><span class="num">2</span><div><b>输入冻结题面，只输入这一次</b><small>不补充指令、不续聊；执行改动会保留在源码里。</small></div><button type="button" class="ghost small" id="copy-prompt" disabled>复制题面</button></li>
    <li data-state="${hasSession && hasTrace ? 'ok' : 'now'}"><span class="num">3</span><div class="grow"><b>补充同一会话的 Session 与原生轨迹</b>${canSupply ? evidenceForm(t) : '<small class="warn-text">当前阶段不能补充材料，请交给 Codex 核查。</small>'}</div></li>
    <li data-state="wait"><span class="num">4</span><div><b>交给 Codex 验收</b><small>两项材料保存后，任务自动进入「验收中」。</small></div></li></ol>${submitLocked(t)}${abandonAction(t)}`;
  if (canSupply) wireEvidence(t);
  wireAbandon(t);
  try {
    const data = await api(`/api/tasks/${t.key}/execution`);
    if (!$('exec-workspace')) return;
    $('exec-workspace').textContent = data.workspace;
    $('copy-workspace').disabled = $('copy-prompt').disabled = false;
    $('copy-workspace').onclick = () => copyText(data.workspace, '源码路径已复制');
    $('copy-prompt').onclick = () => copyText(data.prompt, '冻结题面已复制，到 Trae 粘贴执行。');
  } catch (error) { if ($('exec-workspace')) $('exec-workspace').textContent = error.message; }
}
// The saved original string and its log verification are separate facts: one
// click can save the string and still fail verification while Trae keeps
// writing its logs, so only a verified session may skip the session step.
const sessionVerified = (t) => rank(t.status) >= rank('session_verified');
function sessionProof(t) {
  if (t.session_verification_skipped) return proof('Session 日志核验', false, '未校验（临时）', 'skip');
  return proof('Session 日志核验', !!t.session_sha256, t.session_sha256 ? '已核验' : '待补');
}
function evidenceForm(t) {
  const hasSession = !!t.session_sha256, hasTrace = !!t.artifacts?.x_trae_trace;
  const skipped = !!t.session_verification_skipped;
  const verified = sessionVerified(t) && !skipped;
  const badge = skipped ? '<span class="tag warn">未校验</span>' : verified ? '<span class="tag ok">已核验</span>' : hasSession ? '<span class="tag">待核验</span>' : '<span class="tag">必填</span>';
  const lockedSkip = skipped && sessionVerified(t);
  const skipBox = lockedSkip ? '' : `<label class="checkline"><input type="checkbox" id="session-skip"><span>暂时跳过日志核验，只保存原串</span></label><p class="hint">只影响工作台；Codex 验收仍会看原始日志。跳过只对本题生效。</p>`;
  const body = !hasSession ? `<textarea id="session-token" rows="2" maxlength="1024" autocomplete="off" spellcheck="false" placeholder="双击 Trae 首轮 Agent 头像，粘贴复制到的完整原串"></textarea><p class="hint">只用于本地日志核验，不会回显，也不进入交接文本。</p>${skipBox}`
    : lockedSkip ? '<p class="warn-text">已按临时设置保存原串，未做日志核验。恢复核验需要重新采集本题。</p>'
      : verified ? '<p class="minor">完整原串已私密保存，无需重复填写。</p>'
        : `<p class="warn-text">原串已私密保存，但还没通过日志核验。Trae 仍在写日志时核验会失败，请先停止该会话，再点下方按钮重新核验，不必重新粘贴。</p>${skipBox}`;
  const label = lockedSkip ? '保存轨迹' : hasSession && !verified ? '保存 Session' : hasSession || hasTrace ? '核验并保存' : '核验并保存两项材料';
  return `<form id="evidence-form" class="evidence">
    <div class="evidence-item"><label for="session-token">Session ID ${badge}</label>${body}</div>
    <div class="evidence-item"><span class="label">Trae 原生 Markdown ${hasTrace ? '<span class="tag ok">已归档</span>' : '<span class="tag">必填</span>'}</span>${hasTrace ? '<p class="minor">同一会话的导出已归档。</p>' : `<div class="segmented" role="radiogroup" aria-label="轨迹来源"><label><input type="radio" name="trace-mode" value="upload" checked><span>上传文件</span></label><label><input type="radio" name="trace-mode" value="directory"><span>从任务目录检测</span></label></div>
      <div id="trace-upload"><label class="file"><input id="trace-file" type="file" accept=".md,text/markdown"><span id="trace-file-name">选择 .md 文件（最大 32 MiB）</span></label></div>
      <div id="trace-directory" hidden><div class="copy-line"><code id="trace-drop-path" class="path">读取目录…</code><button type="button" class="ghost small" id="copy-trace-dir">复制目录</button></div><button type="button" class="ghost small" id="scan-traces">检测 Markdown 文件</button><div id="trace-candidates" class="candidates"></div></div>`}</div>
    <div class="actions"><button type="submit" class="primary" id="save-evidence">${label}</button><span id="evidence-progress" class="minor" role="status"></span></div></form>`;
}
function wireEvidence(t) {
  document.querySelectorAll('[name="trace-mode"]').forEach((radio) => radio.addEventListener('change', () => {
    const directory = radio.value === 'directory' && radio.checked;
    $('trace-directory').hidden = !directory; $('trace-upload').hidden = directory;
    if (directory) loadTraceDirectory(t, false);
  }));
  $('trace-file')?.addEventListener('change', () => { const file = $('trace-file').files[0]; $('trace-file-name').textContent = file ? `${file.name} · ${(file.size / 1024).toFixed(1)} KiB` : '选择 .md 文件（最大 32 MiB）'; });
  $('session-skip')?.addEventListener('change', (event) => {
    const button = $('save-evidence');
    if (!button) return;
    if (event.target.checked) button.textContent = '保存（跳过核验）';
    else button.textContent = t.session_sha256 && !sessionVerified(t) ? '保存 Session' : (t.artifacts?.x_trae_trace ? '核验并保存' : '核验并保存两项材料');
  });
  $('scan-traces')?.addEventListener('click', () => busy($('scan-traces'), () => loadTraceDirectory(t, true), '检测中…'));
  $('copy-trace-dir')?.addEventListener('click', () => copyText($('trace-drop-path').textContent, '轨迹目录已复制'));
  $('evidence-form').addEventListener('submit', (event) => {
    event.preventDefault();
    const file = $('trace-file')?.files[0];
    let token = $('session-token')?.value.trim();
    const fromDirectory = document.querySelector('[name="trace-mode"]:checked')?.value === 'directory';
    const candidate = document.querySelector('[name="trace-candidate"]:checked');
    const skip = !!$('session-skip')?.checked;
    if (!t.session_sha256 && !token) { showNotice('请粘贴完整 Session 原串。', true); return; }
    if (!t.artifacts?.x_trae_trace && !fromDirectory && !file) { showNotice('请选择本题的 Trae 原生 Markdown。', true); return; }
    if (!t.artifacts?.x_trae_trace && fromDirectory && !candidate) { showNotice('请先检测目录并选择本题的 Markdown。', true); return; }
    if (file && file.size > 32 * 1024 * 1024) { showNotice('轨迹超过 32 MiB，请确认导出的文件。', true); return; }
    busy($('save-evidence'), async () => {
      if (!sessionVerified(t)) {
        $('evidence-progress').textContent = skip ? '正在保存 Session（跳过日志核验）…' : '正在核对 Session 与目标工作区的真实日志…';
        await api(`/api/tasks/${t.key}/session`, { method: 'POST', body: JSON.stringify({ token, skip_verify: skip }) });
        token = '';
        t.session_sha256 = 'saved';
        t.session_verification_skipped = skip;
        t.status = 'session_verified';
        if ($('session-token')) $('session-token').value = '';
        const item = document.querySelector('#evidence-form .evidence-item');
        if (item) item.innerHTML = skip
          ? '<label>Session ID <span class="tag warn">未校验</span></label><p class="warn-text">原串已私密保存，未做日志核验（临时）。</p>'
          : '<label>Session ID <span class="tag ok">已核验</span></label><p class="minor">完整原串已私密保存并通过日志核验。</p>';
      }
      if (!t.artifacts?.x_trae_trace) {
        $('evidence-progress').textContent = '正在核对轨迹原文并归档…';
        if (fromDirectory) await api(`/api/tasks/${t.key}/trace-import`, { method: 'POST', body: JSON.stringify({ name: candidate.value, sha256: candidate.dataset.hash, attested: true }) });
        else { const body = new FormData(); body.append('file', file); body.append('attested', 'true'); await api(`/api/tasks/${t.key}/trace`, { method: 'POST', body }); }
      }
      if ($('trace-file')) $('trace-file').value = '';
      await refreshAll();
      showNotice(t.session_verification_skipped ? '轨迹已保存。Session 仍为未校验（临时），Codex 验收时会复核原始日志。' : 'Session 和轨迹已核验保存。复制提示词交给 Codex 验收。');
    }, '核验中…');
  });
}
async function loadTraceDirectory(t, scan) {
  try {
    const data = await api(`/api/tasks/${t.key}/trace-files`);
    if (!$('trace-drop-path')) return;
    $('trace-drop-path').textContent = data.directory;
    if (!scan) return;
    $('trace-candidates').innerHTML = data.files.length ? data.files.map((file) => `<label class="candidate"><input type="radio" name="trace-candidate" value="${esc(file.name)}" data-hash="${esc(file.sha256)}" ${file.problem ? 'disabled' : ''}><span><b>${esc(file.name)}</b><small>${esc(file.problem || `${(file.bytes / 1024).toFixed(1)} KiB · ${fmtTime(file.modified_at)}`)}</small></span></label>`).join('') : '<p class="minor">目录中还没有 .md 文件。把 Trae 原生导出放进去后再检测，或改用上传。</p>';
  } catch (error) { showNotice(error.message, true); }
}
function renderReviewNext(t) {
  const reached = (status) => rank(t.status) >= rank(status);
  $('next').innerHTML = `<h3>交给 Codex 验收</h3><p class="minor">Codex 复核日志与源码、按冻结 Rubric 验收取证并 dry-run，写入审核记录。开关关闭时留在本地（标为「本地已完成」，之后由你点击提交）；开启时审核通过后直接提交，平台未开放则留在本地。</p>
    <ul class="proofs">${sessionProof(t)}${proof('原生轨迹', !!t.artifacts?.x_trae_trace, t.artifacts?.x_trae_trace ? '已归档' : '待补')}${proof('终端验收截图', !!t.artifacts?.x_artifact_shot, t.artifacts?.x_artifact_shot ? '已绑定' : 'Codex 生成')}${proof('材料审核', reached('artifacts_verified'), reached('artifacts_verified') ? '已通过' : 'Codex 执行')}${proof('dry-run', t.status === 'dry_run_passed', t.status === 'dry_run_passed' ? '已通过' : 'Codex 执行')}${proof('Codex 审核记录', false, t.status === 'dry_run_passed' ? (t.review_reason || '等待写入') : '最后一步', t.status === 'dry_run_passed' && /不一致|不可读|变化/.test(t.review_reason || '') ? 'bad' : 'wait')}</ul>
    <div class="actions"><button type="button" class="primary" id="copy-review-handoff">复制验收提示词</button>${submitSwitch('review-submit-switch')}</div>${submitLocked(t)}${abandonAction(t)}`;
  $('copy-review-handoff').addEventListener('click', () => copyHandoff(t, $('review-submit-switch').checked));
  wireSwitch('review-submit-switch');
  wireAbandon(t);
}

// ---------- the release gate ----------
async function renderReadyNext(t, generation) {
  let data;
  try { data = await api(`/api/tasks/${t.key}/submit`); }
  catch (error) { $('next').innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; return; }
  if (generation !== app.generation || !$('next')) return;
  const job = data.job;
  if (data.platform) { app.platform = data.platform; renderPlatform(); }
  if (job?.state === 'running') return renderSubmitting(t, job);
  const closed = platformClosed();
  const review = data.gate.review || {};
  const failed = job && (job.state === 'failed' || job.state === 'uncertain') ? job : null;
  $('next').innerHTML = `<div class="gate"><div class="gate-mark" aria-hidden="true"></div><div><h3>本地已完成 · 未提交</h3><p class="minor">Codex 已完成验收与 dry-run，材料留在本地。平台开放时点击提交，工作台会自动上传附件、创建提交并回显平台结果。</p></div></div>
    ${failed ? `<div class="result" data-result="${failed.state}"><b>${failed.state === 'uncertain' ? '上次提交结果未明' : '上次提交未完成'}</b><p>${esc(failed.message)}</p><small>${esc(fmtTime(failed.finished_at || failed.started_at))}</small></div>` : ''}
    <ul class="proofs">${proof('Session 日志已复核', review.session_log_reviewed, review.session_log_reviewed ? '是' : '否')}${proof('只有一条人工题面', review.one_human_prompt, review.one_human_prompt ? '是' : '否')}${proof('源码差异已复核', review.source_diff_reviewed, review.source_diff_reviewed ? '是' : '否')}${proof('Rubric 逐项证据已复核', review.rubric_evidence_reviewed, review.rubric_evidence_reviewed ? '是' : '否')}</ul>
    ${review.notes ? `<details class="notes" open><summary>Codex 审核说明</summary><p>${esc(review.notes)}</p></details>` : ''}
    <dl class="facts compact">${fact('有效轮数', t.rounds ?? '—')}${fact('计划', short(data.plan_sha256), true)}${fact('题面', short(data.prompt_sha256), true)}${fact('审核报告', short(data.audit_sha256), true)}</dl>
    ${closed ? `<div class="result" data-result="uncertain"><b>${esc(closedText())}</b><p>本地审核已完成并保留。平台开放后回到这里点击提交即可，不需要重新验收。</p><small>平台状态读取于 ${esc(fmtTime(app.platform.checked_at))}</small></div>` : ''}
    <div class="actions submit-actions" id="submit-actions"><button type="button" class="submit" id="submit-start" ${closed ? 'disabled' : ''}>${closed ? '等待平台开放' : '提交到平台'}</button>${closed ? '<button type="button" class="ghost small" id="recheck-platform">重新检查平台</button>' : '<span class="minor">提交前会再次实时核对 schema、预检与 Session</span>'}</div>`;
  $('recheck-platform')?.addEventListener('click', () => busy($('recheck-platform'), async () => { await loadPlatform(true); await renderReadyNext(t, app.generation); showNotice(platformClosed() ? closedText() + '，稍后再试。' : '平台已开放，可以提交。'); }, '检查中…'));
  $('submit-start').addEventListener('click', () => {
    if (failed?.state === 'uncertain' && !window.confirm('上次提交结果未明。确认平台上没有对应提交后再继续，否则可能重复。继续？')) return;
    $('submit-actions').innerHTML = `<button type="button" class="submit" id="submit-confirm">确认提交 ${esc(t.display_name)}</button><button type="button" class="ghost" id="submit-cancel">取消</button><span class="warn-text">将创建正式平台提交，无法撤回。</span>`;
    $('submit-confirm').focus();
    $('submit-cancel').addEventListener('click', () => renderReadyNext(t, app.generation));
    $('submit-confirm').addEventListener('click', () => busy($('submit-confirm'), async () => {
      const result = await api(`/api/tasks/${t.key}/submit`, { method: 'POST', body: JSON.stringify({ confirm: true, plan_sha256: data.plan_sha256 }) });
      t.stage = 'submitting'; t.submit_job = result.job;
      renderSubmitting(t, result.job);
      loadTasks().then(() => { renderRail(); renderList(); }).catch(() => {});
    }, '提交中…'));
  });
}
function renderSubmitting(t, job) {
  const next = $('next');
  next.dataset.stage = 'submitting';
  const headChip = document.querySelector('.detail-head .chip');
  if (headChip) { headChip.dataset.stage = 'submitting'; headChip.textContent = STAGES.submitting.label; }
  next.innerHTML = `<div class="gate running"><div class="spinner" aria-hidden="true"></div><div><h3>正在提交</h3><p class="minor">${esc(job.phase || '正式审核、上传附件并创建提交')} · 已用 <span id="submit-elapsed">${elapsed(job.started_at)}</span></p></div></div><p class="minor">可以离开此页，结果会保存在任务里。请勿在别处重复提交。</p>`;
  clearInterval(app.tickTimer);
  app.tickTimer = setInterval(() => { if ($('submit-elapsed')) $('submit-elapsed').textContent = elapsed(job.started_at); else clearInterval(app.tickTimer); }, 1000);
  stopSubmitPolling(false);
  const key = t.key, generation = app.generation;
  app.submitTimer = setInterval(async () => {
    if (generation !== app.generation) { stopSubmitPolling(); return; }
    let data;
    try { data = await api(`/api/tasks/${key}/submit`); } catch { return; }
    if (data.job?.state === 'running') return;
    stopSubmitPolling();
    const done = data.job;
    if (done?.state === 'succeeded') showNotice(`已提交到平台：#${done.submission_id} · ${platformLabel(done.submission_status)}`);
    else if (done) showNotice(done.state === 'uncertain' ? '提交结果未明，需要按平台回执核对。' : '提交未完成：' + done.message, true);
    await refreshAll(true);
  }, 2500);
}
function stopSubmitPolling(all = true) {
  clearInterval(app.submitTimer); app.submitTimer = 0;
  if (all) { clearInterval(app.tickTimer); app.tickTimer = 0; }
}
function renderReceiptNext(t, stage) {
  const job = t.submit_job;
  const fresh = job?.state === 'succeeded' && job.submission_id === t.submission_id && Date.now() - new Date(job.finished_at).getTime() < 30 * 60 * 1000;
  $('next').innerHTML = `${fresh ? `<div class="result" data-result="succeeded"><b>提交成功</b><p>平台已创建提交 #${t.submission_id}，回执状态 ${esc(platformLabel(job.submission_status))}${job.message ? `；${esc(job.message)}` : ''}。</p><small>${esc(fmtTime(job.finished_at))}</small></div>` : ''}
    <section id="receipt-evidence" class="evidence-panel"><p class="loading">读取回执与本地自评…</p></section>
    <div class="actions"><button type="button" class="ghost" id="refresh-receipt">刷新回执</button>${stage === 'rejected' ? '<button type="button" class="ghost" id="copy-receipt-handoff">复制核查提示词</button>' : ''}</div>
    <p class="hint">回执只读查询，不会重复创建。</p>`;
  $('refresh-receipt').addEventListener('click', () => busy($('refresh-receipt'), async () => {
    await api(`/api/tasks/${t.key}/submission-refresh`, { method: 'POST', body: '{}' });
    await refreshAll(true);
    showNotice('回执已刷新。');
  }, '查询中…'));
  $('copy-receipt-handoff')?.addEventListener('click', () => copyHandoff(t));
  loadReceiptEvidence(t);
}

// The receipt is not one file: it is a family of local records with different
// shapes. Each file keeps its own kind; an unreadable or unrecognized file is
// shown as unknown instead of being folded into a pass/fail sentence.
async function loadReceiptEvidence(t) {
  const host = $('receipt-evidence');
  if (!host) return;
  try {
    const data = await api(`/api/tasks/${t.key}/receipts`);
    if (!host.isConnected) return;
    const items = data.items || [];
    const kinds = Object.entries(data.kinds || {}).map(([kind, count]) => `<span class="tag">${esc(kind)}<b>${count}</b></span>`).join('');
    const files = items.map((item) => `
      <details class="receipt-file">
        <summary><span class="tag">${esc(item.kind)}</span><b title="${esc(item.name)}">${esc(item.name)}</b><span class="file-meta">${esc(item.read_state)}${item.bytes ? ` · ${item.bytes} B` : ''}</span></summary>
        ${item.reason ? `<p class="receipt-reason">${esc(item.reason)}</p>` : '<p class="minor">这份回执没有可读的原因字段。</p>'}
        ${item.note ? `<p class="minor">备注：${esc(item.note)}</p>` : ''}
        ${item.checks?.length ? `<ul class="checks">${item.checks.map((check) => `<li data-ok="${check.conclusion === 'ok' || check.conclusion === 'pass'}"><b>${esc(check.name || check.key || '检查项')}</b><em>${esc(check.category || '')} · ${esc(check.conclusion || 'unknown')}</em><small>${esc(check.summary || check.detail || '')}</small></li>`).join('')}</ul>` : ''}
        <code class="path">${esc(item.path)}</code>
      </details>`).join('');
    const local = data.local?.available
      ? `<div class="receipt-local"><div class="section-head"><span class="kicker">LOCAL</span><h4>提交自评</h4><span class="section-count">${data.local.passed}/${data.local.total} 通过</span></div>${(data.local.lines || []).some((line) => !line.ok) ? `<ul class="local-failures">${(data.local.lines || []).filter((line) => !line.ok).slice(0, 6).map((line) => `<li><span>${line.id}</span>${esc(line.text || '未通过')}</li>`).join('')}</ul>` : '<p class="minor">全部通过。</p>'}</div>`
      : `<div class="receipt-local"><div class="section-head"><span class="kicker">LOCAL</span><h4>提交自评</h4></div><p class="minor">${esc(data.local?.note || '没有本地逐条自评')}</p></div>`;
    const conflicts = (data.conflicts || []).map((conflict) => `<div class="receipt-conflict"><div class="section-head"><span class="kicker">CONFLICT</span><h4>结论冲突</h4></div><div class="conflict-grid"><div><span>本地</span><p>${esc(conflict.local)}</p></div><div><span>平台</span><p>${esc(conflict.platform)}</p></div></div><p class="conflict-note">${esc(conflict.note)}</p></div>`).join('');
    const platform = data.platform_reason
      ? `<div class="receipt-verdict"><div class="section-head"><span class="kicker">PLATFORM</span><h4>平台原因</h4></div><p class="receipt-reason">${esc(data.platform_reason)}</p></div>`
      : `<div class="receipt-verdict"><div class="section-head"><span class="kicker">PLATFORM</span><h4>平台原因</h4></div><p class="receipt-unknown">未记录 <span class="tag">unknown</span></p><p class="hint">本地没有可读的退回原因；不要把 unknown 当成未通过。</p></div>`;
    host.innerHTML = `<div class="receipt-sheet"><div class="receipt-main"><div class="receipt-meta"><div><span class="eyebrow">平台提交</span><strong class="receipt-id">#${esc(t.submission_id)}</strong></div><div><span class="eyebrow">回执状态</span><strong>${esc(platformLabel(t.submission_status))}</strong></div><div><span class="eyebrow">有效轮数</span><strong>${esc(t.rounds ?? '—')}</strong></div></div>${platform}${conflicts}${local}</div><aside class="receipt-rail"><div class="section-head"><span class="kicker">FILES</span><h4>回执</h4><span class="section-count">${data.total}</span></div><div class="rail-kinds">${kinds || '<span class="minor">无</span>'}</div>${files || '<p class="minor">任务目录下没有回执 JSON。</p>'}</aside></div>`;
  } catch (error) {
    if (host.isConnected) host.innerHTML = `<p class="bad-text">回执读取失败：${esc(error.message)}</p>`;
  }
}

// ---------- next step panels: prechecks ----------
async function renderRemoteNext(item) {
  const p = item.remote, next = $('next');
  if (item.stage === 'prepare') {
    if (!app.workRoot) {
      next.innerHTML = '<h3>先设置全局工作目录</h3><p class="minor">源码和材料会放在评审仓库之外的独立目录。</p><div class="actions"><button type="button" class="primary" id="remote-settings">设置工作目录</button></div>';
      $('remote-settings').addEventListener('click', openSettingsDialog);
      return;
    }
    next.innerHTML = `<h3>准备独立源码工作区</h3><p class="minor">从原始仓库克隆${p.design_recorded ? '出题设计固定的基线' : '当前默认分支 HEAD'}，记录干净状态并绑定本预检。之后在 Trae 执行冻结题面。</p><div class="actions"><button type="button" class="primary" id="start-remote">准备工作区</button><button type="button" class="ghost" id="import-remote">接入已有干净源码</button></div>`;
    $('import-remote').addEventListener('click', () => openTaskDialog(p));
    $('start-remote').addEventListener('click', () => busy($('start-remote'), async () => {
      const data = await api(`/api/prechecks/${p.id}/start`, { method: 'POST', body: JSON.stringify({ confirm: true }) });
      await loadTasks();
      app.prechecksLoaded = false;
      loadPrechecks().catch(() => {});
      app.busy = false;
      setScope('mine');
      navigate('/tasks/' + data.task.key);
      showNotice(data.archived ? `源码已准备；上次中断留下的目录已归档到 ${data.archived}。` : '源码已准备。复制路径和题面到 Trae 执行。');
    }, '克隆中…'));
    return;
  }
  if (item.stage === 'blocked') {
    next.innerHTML = `<h3>${p.match_unverified ? '平台详情未核实' : '发现历史执行材料'}</h3><p class="warn-text">${p.match_unverified ? '本地任务与平台题面尚未完成匹配，稍后同步再看。' : '同编号已有历史任务或文档，不能一键新跑。先核对现有执行能否交付。'}</p>`;
    return;
  }
  next.innerHTML = `<h3>${esc(p.status_label || p.status)}</h3>${p.reject_label ? `<p class="bad-text">${esc(p.reject_label)}</p>` : ''}${p.reject_detail ? `<p class="minor">${esc(p.reject_detail)}</p>` : '<p class="minor">预检完成后会自动出现在「待准备」。</p>'}`;
}

// ---------- tabs ----------
function renderMaterials(t) {
  // In review the next-step panel already lists these proofs.
  const proofs = t.stage === 'review' ? '' : `<ul class="proofs grid">${t.session_verification_skipped ? proof('Session', false, '未校验（临时）', 'skip') : proof('Session', !!t.session_sha256, t.session_sha256 ? '日志已核验' : '需要你补充')}${proof('原生轨迹', !!t.artifacts?.x_trae_trace, t.artifacts?.x_trae_trace ? '已归档' : '需要你补充')}${proof('验收截图', !!t.artifacts?.x_artifact_shot, t.artifacts?.x_artifact_shot ? '已绑定' : 'Codex 生成')}${proof('审核记录', t.review_ready || t.submission_id > 0, t.submission_id > 0 ? '已用于提交' : t.review_ready ? '与当前材料一致' : 'Codex 写入')}</ul>`;
  $('tab-panel').innerHTML = `${proofs}<dl class="facts${proofs ? '' : ' flush'}">${fact('状态', sessionStatusLabel(t))}${fact('有效轮数', t.rounds ?? '日志核验后填写')}${fact('样类型', t.question_type ?? '未记录')}${fact('超过两小时', t.more_than_two_hours ? (t.more_than_two_hours === '是' ? '是' : '否') : '未记录')}${fact('执行方式', t.execution_mode === 'manual' ? '手动执行' : '历史记录')}${fact('执行开始', fmtTime(t.execution_started_at))}${fact('平台提交', t.submission_id ? '#' + t.submission_id : '未提交')}${fact('固定基线', short(t.baseline), true)}${fact('题面哈希', short(t.prompt_sha256), true)}${fact('状态版本', 'rev ' + t.revision)}</dl><section class="evidence-blocks"><div id="journal-block" class="evidence-block"><p class="loading">读取记录链…</p></div><div id="trace-block" class="evidence-block"><p class="loading">读取轨迹账本…</p></div></section>`;
  loadEvidenceBlocks(t);
}

async function loadEvidenceBlocks(t) {
  const journalHost = $('journal-block'), traceHost = $('trace-block');
  if (!journalHost || !traceHost) return;
  api(`/api/tasks/${t.key}/journal`).then((data) => {
    if (!journalHost.isConnected) return;
    const chain = data.chain || {};
    const last = (data.events || []).slice(-1)[0];
    const verified = !!chain.verified;
    journalHost.innerHTML = `<div class="section-head"><span class="kicker">JOURNAL</span><h4>记录链</h4><button type="button" class="ghost small" id="export-bundle">导出校验包</button></div>
      <div class="chain" data-state="${verified ? 'ok' : 'bad'}" role="img" aria-label="${verified ? '记录链完整' : `记录链在 ${chain.broken_at || '未知位置'} 断开`}"><i aria-hidden="true"></i><span aria-hidden="true"></span><b>${verified ? chain.count : `断在 ${chain.broken_at || '?'}`}</b><span aria-hidden="true"></span><i aria-hidden="true"></i></div>
      <div class="chain-meta"><span>${chain.head_matches ? '已对齐快照' : '与快照不一致'}</span><span>最后事件 ${last ? `${esc(fmtTime(last.at))} · ${esc(last.action || last.to)}` : '—'}</span></div>
      ${chain.reason ? `<p class="bad-text chain-error">${esc(chain.reason)}</p>` : ''}`;
    $('export-bundle')?.addEventListener('click', () => exportBundle(t));
  }).catch((error) => { if (journalHost.isConnected) journalHost.innerHTML = `<p class="bad-text">记录链读取失败：${esc(error.message)}</p>`; });
  api(`/api/tasks/${t.key}/execution`).then((data) => {
    if (!traceHost.isConnected) return;
    traceHost.innerHTML = traceLedgerMarkup(data.trace_ledger);
  }).catch((error) => { if (traceHost.isConnected) traceHost.innerHTML = `<p class="bad-text">轨迹读取失败：${esc(error.message)}</p>`; });
}

function traceLedgerMarkup(ledger) {
  if (!ledger?.available) {
    return `<div class="section-head"><span class="kicker">TRACE</span><h4>轨迹账本</h4></div><div class="empty-inline"><b>没有归档轨迹</b><p class="hint">${esc(ledger?.error || '材料里没有可读的 Trae Markdown。')}</p></div>`;
  }
  const note = (ledger.notes || [])[0] || '由归档 Markdown 计数；轮数来自 Trae，不能由 Markdown 推导。';
  return `<div class="section-head"><span class="kicker">TRACE</span><h4>轨迹账本</h4></div><dl class="ledger"><div><dt>人工题面</dt><dd>${ledger.user_turns}</dd></div><div><dt>助手分段</dt><dd>${ledger.assistant_sections}</dd></div><div><dt>有效轮数</dt><dd>${ledger.effective_rounds || '—'}</dd></div><div><dt>Markdown</dt><dd>${ledger.lines}<small>行</small></dd></div></dl><p class="ledger-note">${esc(note)}</p>`;
}

async function exportBundle(t) {
  const button = $('export-bundle');
  await busy(button, async () => {
    const data = await api(`/api/tasks/${t.key}/journal?full=1`);
    data.exported_at = new Date().toISOString();
    data.export_schema = 'swe-workbench-bundle/v1';
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `bundle-${t.task_id || t.key}.json`;
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    showNotice('校验包已导出。');
  }, '导出中…');
}

async function loadExecution(t) {
  const panel = $('tab-panel');
  try {
    const data = await api(`/api/tasks/${t.key}/execution`);
    if (!panel.isConnected) return;
    panel.innerHTML = `<div class="split"><div><span class="caption">模型源码工作区</span><div class="copy-line"><code class="path">${esc(data.workspace)}</code><button type="button" class="ghost small" id="tab-copy-workspace">复制</button></div><span class="caption">固定基线</span><code class="path">${esc(data.baseline)}</code>${data.model ? `<span class="caption">记录模型（按实际证据复核）</span><p>${esc(data.model)}</p>` : '<p class="hint">实际模型和版本由 Codex 根据日志与轨迹核实。</p>'}</div><div><div class="caption-row"><span class="caption">冻结题面</span><button type="button" class="ghost small" id="tab-copy-prompt">复制 Trae 原文</button></div><div class="prompt">${esc(data.prompt)}</div></div></div>${traceLedgerMarkup(data.trace_ledger)}`;
    $('tab-copy-workspace').addEventListener('click', () => copyText(data.workspace, '源码路径已复制'));
    $('tab-copy-prompt').addEventListener('click', () => copyText(data.prompt, '冻结题面已复制'));
  } catch (error) { if (panel.isConnected) panel.innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; }
}
async function loadHandoff(t) {
  const panel = $('tab-panel');
  try {
    const submit = submitPref() && !t.submission_id;
    const data = await api(`/api/tasks/${t.key}/handoff${submit ? '?submit=1' : ''}`);
    if (!panel.isConnected) return;
    panel.innerHTML = `<p class="minor">${submit ? 'Codex 沿用现有执行：复核日志、逐项验收、终端取证、补齐字段并 dry-run，写入审核记录后直接提交到 SWE 平台；平台未开放时留在本地。' : 'Codex 沿用现有执行：复核日志、逐项验收、终端取证、补齐字段并 dry-run，写入审核记录后留在本地，不提交。'}</p><div class="actions"><button type="button" class="primary" id="tab-copy-handoff">复制提示词</button>${t.submission_id ? '' : submitSwitch('tab-submit-switch')}</div><details class="notes"><summary>查看将复制的内容</summary><pre>${esc(data.codex_text)}</pre></details>${data.review_template ? `<details class="notes"><summary>审核记录模板（workbench-review.json）</summary><pre>${esc(JSON.stringify({ ...data.review_template, session_log_reviewed: false, one_human_prompt: false, source_diff_reviewed: false, rubric_evidence_reviewed: false, notes: '' }, null, 2))}</pre></details>` : ''}`;
    $('tab-copy-handoff').addEventListener('click', () => copyText(data.codex_text, submit ? '提示词已复制：审核通过后 Codex 会直接提交到平台。' : '提示词已复制：Codex 审核完成后留在本地，不提交。'));
    wireSwitch('tab-submit-switch', () => loadHandoff(t));
  } catch (error) { if (panel.isConnected) panel.innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; }
}
function rubricBar(pass, fail, unbound, total) {
  let x = 0;
  const rects = [['pass', pass], ['fail', fail], ['unbound', unbound]].map(([kind, count]) => {
    const width = total ? (count / total) * 100 : 0;
    const rect = width > 0 ? `<rect class="${kind}" x="${x.toFixed(3)}" y="0" width="${width.toFixed(3)}" height="4"></rect>` : '';
    x += width;
    return rect;
  }).join('');
  return `<svg class="rubric-bar" viewBox="0 0 100 4" preserveAspectRatio="none" role="img" aria-label="${pass} 条通过，${fail} 条未通过，${unbound} 条未绑定">${rects}</svg>`;
}

async function loadDesign(t) {
  const panel = $('tab-panel');
  try {
    const data = await api(`/api/tasks/${t.key}/design`);
    if (!panel.isConnected) return;
    const names = { verify_rubric: 'Rubric', authenticity_note: '真实性与难度说明', involved_modules: '可能涉及模块', task_type: '任务类型' };
    const state = data.frozen ? 'ok' : data.missing.length ? 'wait' : 'bad';
    const rubric = data.rubric || {};
    const labels = { PASS: '通过', FAIL: '未通过', UNBOUND: '未绑定', MISSING: '缺证据', UNKNOWN: '无法判定' };
    const total = rubric.total || 0, pass = rubric.passed || 0, fail = rubric.failed || 0;
    const unbound = (rubric.unbound || 0) + (rubric.missing || 0);
    const bar = total ? rubricBar(pass, fail, unbound, total) : '';
    const items = (data.rubric_items || []).map((item) => {
      const ok = item.conclusion === 'PASS' ? 'true' : item.conclusion === 'FAIL' ? 'false' : 'unknown';
      return `<li class="rubric-row" data-ok="${ok}"><span class="rubric-id">${item.id}</span><span class="rubric-text">${esc(item.text)}</span><span class="status" data-status="${esc(item.conclusion)}">${esc(labels[item.conclusion] || item.conclusion)}</span>${item.evidence ? `<small>${esc(item.evidence)}</small>` : '<small class="none">无证据记录</small>'}</li>`;
    }).join('');
    const rubricBlock = (data.rubric_items || []).length
      ? `<section class="rubric-evidence"><div class="section-head"><span class="kicker">RUBRIC</span><h4>判据与证据</h4><span class="section-count">${pass}/${total}</span></div>${bar}<div class="rubric-table"><div class="rubric-head"><span>#</span><span>判据</span><span>结论</span></div><ol class="rubric-list">${items}</ol></div>${rubric.error ? `<p class="warn-text">${esc(rubric.error)}</p>` : ''}${rubric.script_tests?.length ? `<p class="hint">验收脚本提及 ${rubric.script_tests.length} 个测试函数；绑定仍以结果文件为准。</p>` : ''}</section>`
      : `<p class="warn-text">没有可读的 rubric.yaml；这道题的判据无法核对。</p>`;
    panel.innerHTML = `<div class="design-sheet"><div class="design-head"><ul class="proofs flush">${proof('四项设计材料', data.frozen, data.frozen ? (data.origin === 'before_execution' ? '出题时设计，已冻结' : '已冻结') : data.missing.length ? '缺少' + data.missing.map((k) => names[k] || k).join('、') : '已填写，来源待核查', state)}</ul><div class="design-path"><span class="kicker">DIR</span><code class="path">${esc(data.directory)}</code></div></div>${rubricBlock}<p class="hint">rubric.yaml 保存验收条件，materials.json 保存四项材料、来源和基线绑定。${esc(data.semantic_review || '')}</p></div>`;
  } catch (error) { if (panel.isConnected) panel.innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; }
}
function fieldValue(field) {
  if (field.field_key === 'x_trae_trace') return field.configured ? '已归档原生轨迹' : '在「待执行」中补充';
  if (field.type === 'attachment' || field.type === 'file') return field.configured ? '已绑定材料' : '由 Codex 生成并绑定';
  if (field.field_key === 'trae_session_id') return field.configured ? '已保存完整原串（隐藏）' : '在「待执行」中补充';
  if (field.field_key === 'trae_session_id_2' || field.field_key === 'seed_rounds') return '按规则留空';
  return field.value === undefined || field.value === null || field.value === '' ? '待 Codex 补齐' : String(field.value);
}
function fieldReviewMarkup(review) {
  if (!review) return '';
  const status = { blocked: '需补齐或修正', review_required: '需证据复核', passed: '自动规则通过' }[review.status] || '待核对';
  return `<details class="rules"><summary data-review="${esc(review.status)}">${esc(status)}</summary><ul>${(review.rules || []).map((rule) => `<li>${esc(rule)}</li>`).join('')}</ul>${(review.problems || []).map((problem) => `<p class="bad-text">${esc(problem)}</p>`).join('')}</details>`;
}
async function loadForm(t) {
  const panel = $('tab-panel');
  try {
    const form = await api(`/api/tasks/${t.key}/form`);
    if (!panel.isConnected) return;
    app.form = form;
    if (!form.plan_ready) { panel.innerHTML = '<p class="minor">提交计划尚未建立；补充 Session 时会自动准备。</p>'; return; }
    const fields = form.fields.filter((f) => !f.auto && f.type !== 'file');
    panel.innerHTML = (form.schema_changed ? '<p class="bad-text">平台规则已变化，字段暂时只读，由 Codex 核查计划。</p>' : '<p class="minor">字段默认锁定。可修正字段双击（或选中后按 Enter）确认解锁；证据绑定项不可直接修改。</p>') +
      `<div class="fields">${fields.map((field, index) => `<div class="field ${field.type === 'textarea' ? 'wide' : ''}"><span class="field-title">${esc(field.label || field.field_key)}${field.required ? '<i>*</i>' : ''}<small>${field.editable && form.editable ? '双击修改' : '证据绑定'}</small></span>${field.editable && form.editable ? `<button type="button" class="field-view" data-unlock="${index}" aria-label="解锁修改 ${esc(field.label || field.field_key)}">${esc(fieldValue(field))}</button>` : `<div class="field-view fixed">${esc(fieldValue(field))}</div>`}${fieldReviewMarkup(field.review)}</div>`).join('')}</div>`;
    panel.querySelectorAll('[data-unlock]').forEach((node) => {
      const invoke = () => openUnlock(t, fields[Number(node.dataset.unlock)], node);
      node.addEventListener('dblclick', invoke);
      node.addEventListener('keydown', (event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); invoke(); } });
    });
  } catch (error) { if (panel.isConnected) panel.innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; }
}
function openUnlock(t, field, node) {
  if (app.busy || dirty()) { showNotice('请先保存或取消当前字段修改。'); return; }
  $('unlock-label').textContent = field.label || field.field_key;
  $('unlock-dialog').showModal();
  $('confirm-unlock').onclick = () => busy($('confirm-unlock'), async () => {
    const grant = await api(`/api/tasks/${t.key}/form/unlock`, { method: 'POST', body: JSON.stringify({ field: field.field_key, confirm: true, plan_sha256: app.form.plan_sha256 }) });
    $('unlock-dialog').close();
    if (!node.isConnected) return;
    const value = esc(field.value ?? '');
    let control = `<input id="field-editor" data-editor type="${field.type === 'number' ? 'number' : field.type === 'date' ? 'date' : 'text'}" value="${value}">`;
    if (field.type === 'textarea') control = `<textarea id="field-editor" data-editor rows="6">${value}</textarea>`;
    if (field.type === 'select') control = `<select id="field-editor" data-editor><option value="">请选择</option>${(field.options || []).map((o) => `<option value="${esc(o)}" ${o === field.value ? 'selected' : ''}>${esc(o)}</option>`).join('')}</select>`;
    node.outerHTML = `<div class="field-editor"><label class="sr-only" for="field-editor">${esc(field.label || field.field_key)}</label>${control}<div class="actions"><button id="save-field" type="button" class="primary small">保存并锁定</button><button id="cancel-field" type="button" class="ghost small">取消</button></div></div>`;
    $('field-editor').focus();
    $('cancel-field').onclick = () => loadForm(t);
    $('save-field').onclick = () => busy($('save-field'), async () => {
      let next = $('field-editor').value;
      if (field.type === 'number') { if (next === '') throw new Error('请输入数字。'); next = Number(next); }
      await api(`/api/tasks/${t.key}/form`, { method: 'PUT', body: JSON.stringify({ values: { [field.field_key]: next }, unlock_tokens: { [field.field_key]: grant.unlock_token }, plan_sha256: app.form.plan_sha256 }) });
      await loadForm(t);
      await loadTasks(); renderRail(); renderList();
      showNotice('字段已保存并重新锁定；材料变化后需要 Codex 重新审核。');
    });
  });
}
async function loadRemotePrompt(p) {
  const panel = $('tab-panel');
  try {
    const data = await api(`/api/prechecks/${p.id}/handoff?mode=manual`);
    if (!panel.isConnected) return;
    panel.innerHTML = `<div class="caption-row"><span class="caption">交给 Trae 的冻结题面</span><button type="button" class="ghost small" id="copy-remote-prompt">复制 Trae 原文</button></div><div class="prompt">${esc(data.prompt)}</div>`;
    $('copy-remote-prompt').addEventListener('click', () => copyText(data.prompt, '冻结题面已复制'));
  } catch (error) { if (panel.isConnected) panel.innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; }
}
async function loadRemoteDesign(p) {
  const panel = $('tab-panel');
  try {
    const data = await api(`/api/prechecks/${p.id}/design`);
    if (!panel.isConnected) return;
    if (data.recorded) {
      const names = { task_type: '任务类型', authenticity_note: '真实性与难度说明', involved_modules: '可能涉及模块', verify_rubric: 'Verify Rubric' };
      panel.innerHTML = `<ul class="proofs">${proof('出题设计', true, data.approved ? '已绑定预检，准备任务时自动复用' : '已记录，预检通过后自动绑定')}</ul><dl class="facts">${fact('设计基线', short(data.baseline), true)}${fact('主要语言', data.language)}${fact('记录时间', fmtTime(data.recorded_at))}</dl>${Object.entries(names).map(([key, name]) => `<details class="notes"><summary>${esc(name)}</summary><pre>${esc(data.fields[key])}</pre></details>`).join('')}<p class="hint">研究依据：${esc(data.basis)}</p>`;
      return;
    }
    panel.innerHTML = '<p class="minor">这条预检尚无配套设计记录。出题时一起保存四项材料，通过后自动绑定。</p>' + (data.can_record ? '<details class="notes"><summary>导入已经设计好的材料</summary><p class="hint">仅用于尚未执行的题目。JSON 包含 language 与 materials，内容和研究证据须来自本题出题过程。</p><label class="file"><input id="precheck-design-file" type="file" accept=".json,application/json"><span>选择设计 JSON</span></label><div class="actions"><button type="button" class="ghost small" id="save-precheck-design">校验并记录</button></div></details>' : '<p class="hint">本题已有执行或当前状态不允许登记新设计。</p>');
    $('save-precheck-design')?.addEventListener('click', () => busy($('save-precheck-design'), async () => {
      const file = $('precheck-design-file').files[0];
      if (!file) throw new Error('请选择本题的设计 JSON。');
      if (file.size > 64 * 1024) throw new Error('设计 JSON 超过 64 KiB。');
      let design;
      try { design = JSON.parse(await file.text()); } catch { throw new Error('JSON 格式不正确。'); }
      if (!window.confirm('确认这是本题出题时设计的材料？记录后不能覆盖，已执行任务不得冒充执行前设计。')) return;
      await api(`/api/prechecks/${p.id}/design`, { method: 'POST', body: JSON.stringify(design) });
      p.design_recorded = true;
      await loadRemoteDesign(p);
      showNotice('出题设计已校验记录。');
    }));
  } catch (error) { if (panel.isConnected) panel.innerHTML = `<p class="bad-text">${esc(error.message)}</p>`; }
}

// ---------- settings ----------
function proxyFormURL(quiet = false) {
  const scheme = $('proxy-scheme')?.value || 'http';
  const host = ($('proxy-host')?.value || '').trim();
  const port = Number($('proxy-port')?.value);
  const fail = (message) => { if (!quiet) showNotice(message, true); return ''; };
  if (!host) return fail('请填写代理主机。');
  if (!Number.isInteger(port) || port < 1 || port > 65535) return fail('代理端口必须在 1–65535 之间。');
  const normalized = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
  return `${scheme}://${normalized}:${port}`;
}
function renderNetworkResult(data) {
  const leg = (label, r) => `<div class="net-leg ${r.ok ? '' : 'bad'}"><span class="net-label">${label}</span><b class="net-value">${r.ok ? `${r.ms}<small>ms</small>` : '失败'}</b><span class="${r.ok ? 'net-status' : 'net-error'}">${r.ok ? `HTTP ${r.status}` : esc(r.error || '连接失败')}</span></div>`;
  const when = data.tested_at ? ` · ${esc(fmtTime(data.tested_at))}` : '';
  const proxy = data.proxy_url ? ` · ${esc(data.proxy_url)}` : '';
  $('network-result').innerHTML = `<div class="net-legs">${leg('直连', data.direct)}${leg('代理', data.proxy)}</div><p class="net-meta">${esc(data.target)}${when}${proxy}</p>`;
}
function accountBlock(data) {
  const env = !!data?.env_override;
  const accounts = data?.accounts || [];
  const error = data?.error || '';
  const rows = accounts.map((account) => {
    const actions = env ? '' : `${account.active ? '<span class="tag">当前</span>' : `<button type="button" class="ghost small" data-account-switch="${esc(account.username)}">切换</button>`}<button type="button" class="link-button account-remove" data-account-remove="${esc(account.username)}">移除</button>`;
    return `<li class="account-row ${account.active ? 'active' : ''}"><span class="account-dot" aria-hidden="true"></span><div class="account-who"><b>${esc(account.username)}</b><small>${esc(account.real_name || account.role || '未记录姓名')}</small></div>${actions}</li>`;
  }).join('');
  if (error) return `<p class="account-env">账号配置不可读：${esc(error)}</p>`;
  const form = env
    ? ''
    : `<form id="account-form" class="account-form"><label>账号<input id="account-username" autocomplete="username" maxlength="64" spellcheck="false" required></label><label>密码<input id="account-password" type="password" autocomplete="current-password" maxlength="256" required></label><button type="submit" class="ghost small" id="account-add">登录并保存</button></form><p class="account-hint">密码只写入本机私有配置（0600），不回显，也不进入任务材料或提交文本。</p>`;
  return `<ul class="account-list">${rows || '<li class="account-empty">还没有保存的账号。登录一个账号后即可切换。</li>'}</ul>${form}${env ? '<p class="account-env">当前启动环境用 SWE_* 提供平台凭据，账号切换不会生效。移除此环境变量并重启工作台后，才能在这里管理账号。</p>' : ''}`;
}
async function refreshAccountBlock() {
  const data = await api('/api/swe/accounts');
  app.accounts = data;
  renderAccountChip();
  const host = $('account-block');
  if (host) {
    host.innerHTML = accountBlock(data);
    wireAccountBlock(data);
  }
  return data;
}
async function refreshAfterAccountChange() {
  app.prechecksRequest = null;
  await Promise.allSettled([loadPrechecks(true), loadPlatform(true)]);
  renderRail();
  renderList();
}
async function accountSwitch(username, button) {
  await busy(button, async () => {
    await api('/api/swe/accounts/switch', { method: 'POST', body: JSON.stringify({ username }) });
    const data = await refreshAccountBlock();
    await refreshAfterAccountChange();
    showNotice(`已切换到 ${data.active || username}。`);
  }, '切换中…');
}
async function accountRemove(username, button) {
  if (!window.confirm(`移除 SWE 账号 ${username}？`)) return;
  await busy(button, async () => {
    const data = await api('/api/swe/accounts/remove', { method: 'POST', body: JSON.stringify({ username }) });
    await refreshAccountBlock();
    await refreshAfterAccountChange();
    showNotice(data.active ? `账号已移除，当前身份 ${data.active}。` : '账号已移除，当前没有平台身份。');
  }, '移除中…');
}
async function accountAdd() {
  const username = $('account-username').value.trim();
  const password = $('account-password').value;
  if (!username || !password) { showNotice('请填写账号和密码。', true); return; }
  await busy($('account-add'), async () => {
    await api('/api/swe/accounts', { method: 'POST', body: JSON.stringify({ username, password }) });
    await refreshAccountBlock();
    await refreshAfterAccountChange();
    showNotice(`已添加并切换到 ${username}。`);
  }, '登录中…');
}
function wireAccountBlock(data) {
  document.querySelectorAll('[data-account-switch]').forEach((button) => button.addEventListener('click', () => accountSwitch(button.dataset.accountSwitch, button)));
  document.querySelectorAll('[data-account-remove]').forEach((button) => button.addEventListener('click', () => accountRemove(button.dataset.accountRemove, button)));
  $('account-form')?.addEventListener('submit', (event) => { event.preventDefault(); accountAdd(); });
}
async function renderSettings() {
  let s, accounts;
  try { s = await api('/api/settings'); }
  catch (error) { $('detail').innerHTML = backBar() + `<div class="detail-empty"><h2>设置读取失败</h2><p>${esc(error.message)}</p></div>`; wireBack(); return; }
  try { accounts = await api('/api/swe/accounts'); app.accounts = accounts; renderAccountChip(); }
  catch (error) { accounts = { accounts: [], active: '', env_override: false, error: error.message }; }
  const enabled = !!s.proxy_enabled;
  const host = s.proxy_host || '127.0.0.1';
  const port = Number(s.proxy_port) || 7890;
  $('detail').innerHTML = `<div class="settings-page" id="settings-page">
    <div class="settings-top">${backBar()}<div class="settings-top-actions"><span id="settings-dirty" class="settings-dirty" hidden>未保存</span><button type="button" class="primary settings-save" id="save-settings">保存更改</button></div></div>
    <div class="settings-layout">
      <aside class="settings-aside">
        <p class="kicker">PREFERENCES</p>
        <h2 class="title">设置</h2>
        <nav class="settings-nav" aria-label="设置分区">
          <a href="#settings-proxy" data-target="settings-proxy" class="active"><span>01</span>项目拉取</a>
          <a href="#settings-directory" data-target="settings-directory"><span>02</span>工作目录</a>
          <a href="#settings-network" data-target="settings-network"><span>03</span>网络诊断</a>
          <a href="#settings-account" data-target="settings-account"><span>04</span>SWE 用户</a>
        </nav>
        <p class="settings-note">代理只用于 GitHub 拉取；账号切换只影响平台身份。</p>
      </aside>
      <div class="settings-pane">
        <section id="settings-proxy" class="settings-section">
          <header class="settings-section-head"><span class="settings-num">01</span><div><h3>项目拉取</h3><p>让 git clone / fetch 走本地代理。</p></div><label class="switch" title="启用代理"><input type="checkbox" id="proxy-enabled" ${enabled ? 'checked' : ''}><span class="switch-track" aria-hidden="true"></span><span class="sr-only">启用代理</span></label></header>
          <div class="settings-row">
            <div class="settings-label"><label for="proxy-host">代理地址</label><p>Loon 默认 127.0.0.1:7890</p></div>
            <div class="settings-control">
              <div class="proxy-fields">
                <select id="proxy-scheme" aria-label="代理类型"><option value="http">HTTP</option><option value="https">HTTPS</option><option value="socks5">SOCKS5</option></select>
                <input id="proxy-host" aria-label="代理主机" value="${esc(host)}" autocomplete="off" spellcheck="false">
                <span class="proxy-sep" aria-hidden="true">:</span>
                <input id="proxy-port" aria-label="代理端口" type="number" min="1" max="65535" inputmode="numeric" value="${esc(String(port))}">
              </div>
              <div class="proxy-tools"><p class="settings-effective">当前 <code id="proxy-preview">${esc(s.proxy_url || '')}</code><span id="proxy-state" class="settings-state ${enabled ? 'on' : ''}">${enabled ? '已启用' : '未启用'}</span></p><button type="button" class="link-button" id="proxy-defaults">恢复 Loon 默认</button></div>
            </div>
          </div>
        </section>
        <section id="settings-directory" class="settings-section">
          <header class="settings-section-head"><span class="settings-num">02</span><div><h3>工作目录</h3><p>新任务的源码与工作流写在这里。</p></div></header>
          <div class="settings-row">
            <div class="settings-label"><label for="work-root-input">绝对路径</label><p>换目录只影响新任务</p></div>
            <div class="settings-control"><input id="work-root-input" class="settings-path" value="${esc(s.work_root || '')}" required autocomplete="off" spellcheck="false"><p class="settings-inline-hint"><code>sources/&lt;题目&gt;</code> 放模型源码，<code>workflow/&lt;题目&gt;</code> 放题面、状态和验收材料。旧目录里已有的任务仍会保留。</p></div>
          </div>
        </section>
        <section id="settings-network" class="settings-section">
          <header class="settings-section-head"><span class="settings-num">03</span><div><h3>GitHub 延迟</h3><p>对比直连与代理访问 github.com 的耗时。</p></div><button type="button" class="ghost small" id="test-network">测试</button></header>
          <div class="settings-net" id="network-result" aria-live="polite">
            <div class="net-legs"><div class="net-leg"><span class="net-label">直连</span><b class="net-value">—</b><span class="net-status">未测试</span></div><div class="net-leg"><span class="net-label">代理</span><b class="net-value">—</b><span class="net-status">未测试</span></div></div>
            <p class="net-meta">目标 https://github.com · 测试使用当前表单，不需要先保存</p>
          </div>
        </section>
        <section id="settings-account" class="settings-section">
          <header class="settings-section-head"><span class="settings-num">04</span><div><h3>SWE 用户</h3><p>切换平台身份；本地任务不随账号复制。</p></div></header>
          <div id="account-block">${accountBlock(accounts)}</div>
        </section>
      </div>
    </div>
  </div>`;
  $('detail').scrollTop = 0;
  window.scrollTo(0, 0);
  wireBack();
  $('proxy-scheme').value = s.proxy_scheme || 'http';
  const markDirty = () => { const page = $('settings-page'); if (page) page.dataset.dirty = 'true'; const badge = $('settings-dirty'); if (badge) badge.hidden = false; };
  const updateProxyPreview = () => {
    const url = proxyFormURL(true);
    if (url) $('proxy-preview').textContent = url;
    const on = $('proxy-enabled').checked;
    const state = $('proxy-state');
    state.textContent = on ? '已启用' : '未启用';
    state.classList.toggle('on', on);
  };
  ['proxy-enabled', 'proxy-scheme', 'proxy-host', 'proxy-port', 'work-root-input'].forEach((id) => {
    const el = $(id);
    if (!el) return;
    const changed = () => { markDirty(); if (id.startsWith('proxy')) updateProxyPreview(); };
    el.addEventListener('input', changed);
    el.addEventListener('change', changed);
  });
  $('proxy-defaults').addEventListener('click', () => {
    $('proxy-scheme').value = 'http';
    $('proxy-host').value = '127.0.0.1';
    $('proxy-port').value = '7890';
    markDirty();
    updateProxyPreview();
  });
  document.querySelectorAll('.settings-nav a').forEach((link) => link.addEventListener('click', (event) => {
    event.preventDefault();
    const target = document.getElementById(link.dataset.target);
    if (!target) return;
    document.querySelectorAll('.settings-nav a').forEach((item) => item.classList.toggle('active', item === link));
    target.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }));
  $('save-settings').addEventListener('click', () => busy($('save-settings'), async () => {
    const portValue = Number($('proxy-port').value);
    if (!Number.isInteger(portValue) || portValue < 1 || portValue > 65535) { showNotice('代理端口必须在 1–65535 之间。', true); return; }
    const workRoot = $('work-root-input').value.trim();
    if (!workRoot) { showNotice('请填写全局工作目录。', true); return; }
    const data = await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        work_root: workRoot,
        proxy_enabled: $('proxy-enabled').checked,
        proxy_scheme: $('proxy-scheme').value,
        proxy_host: $('proxy-host').value.trim(),
        proxy_port: portValue,
      }),
    });
    app.workRoot = data.work_root;
    const page = $('settings-page'); if (page) page.dataset.dirty = 'false';
    const badge = $('settings-dirty'); if (badge) badge.hidden = true;
    showNotice('设置已保存。');
    await refreshAll(true);
  }, '保存中…'));
  $('test-network').addEventListener('click', async () => {
    const url = proxyFormURL();
    if (!url) return;
    await busy($('test-network'), async () => {
      const data = await api('/api/settings/test-network', { method: 'POST', body: JSON.stringify({ proxy_url: url }) });
      renderNetworkResult(data);
    }, '测试中…');
    if ($('test-network')) $('test-network').textContent = '重新测试';
  });
  wireAccountBlock(accounts);
}

// ---------- dialogs ----------
function openSettingsDialog() { navigate('/settings'); }
function openTaskDialog(remote = null) {
  app.importing = remote?.id || 0;
  const form = $('task-form');
  form.reset();
  $('dialog-title').textContent = remote ? `接入预检 #${remote.id}` : '接入已有干净源码';
  $('dialog-context').textContent = remote?.repo_url || '';
  if (remote) {
    const name = `${remote.id}-${repoName(remote.repo_url).split('/').pop()}`;
    form.elements.task_id.value = name; form.elements.task_id.readOnly = true; form.elements.task_dir.value = 'tasks/' + name;
  } else form.elements.task_id.readOnly = false;
  $('repo-field').hidden = !!remote; $('prompt-field').hidden = !!remote;
  $('task-dialog').showModal();
}
async function submitTaskDialog(event) {
  event.preventDefault();
  await busy($('task-save'), async () => {
    const input = Object.fromEntries(new FormData($('task-form')).entries());
    const task = await api(app.importing ? `/api/prechecks/${app.importing}/import` : '/api/tasks', { method: 'POST', body: JSON.stringify(input) });
    $('task-dialog').close();
    await loadTasks();
    app.busy = false;
    navigate('/tasks/' + task.key);
    showNotice('本地任务已建立，已记录手动模式。');
  });
}

// ---------- refresh ----------
async function refreshAll(force = false) {
  await loadTasks();
  const before = app.current;
  const resolved = resolveRoute();
  const after = resolved.item;
  renderRail();
  renderList();
  const changed = !before || !after || before.id !== after.id || before.stage !== after.stage || before.task?.revision !== after.task?.revision || before.task?.submit_job?.state !== after.task?.submit_job?.state;
  if (force && !dirty()) { await renderRoute(); return; }
  if (changed && after && $('detail-update')) {
    if (!dirty() && !app.busy) { app.current = after; await renderRoute(); }
    else $('detail-update').hidden = false;
  } else if (after) app.current = after;
  else if (!resolved.missing && document.body.dataset.view === 'list' && !dirty()) renderOverview();
}
function moveSelection(delta) {
  const rows = Array.from($('list').querySelectorAll('[data-route]'));
  if (!rows.length) return;
  const index = rows.findIndex((row) => row.getAttribute('aria-selected') === 'true');
  const next = rows[Math.min(rows.length - 1, Math.max(0, index < 0 ? 0 : index + delta))];
  next.scrollIntoView({ block: 'nearest' });
  navigate(next.dataset.route);
}

async function init() {
  const theme = storage.get('swe-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  document.documentElement.dataset.theme = theme;
  app.view = loadView();
  app.view.stages = app.view.stages.filter((x) => STAGES[x]);
  const closeMenu = () => { $('more-menu').hidden = true; $('more-button').setAttribute('aria-expanded', 'false'); };
  $('more-button').addEventListener('click', (event) => { event.stopPropagation(); const open = $('more-menu').hidden; $('more-menu').hidden = !open; $('more-button').setAttribute('aria-expanded', String(open)); });
  $('theme-toggle').addEventListener('click', () => { closeMenu(); const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'; document.documentElement.dataset.theme = next; storage.set('swe-theme', next); });
  $('filter-toggle').addEventListener('click', (event) => { event.stopPropagation(); toggleFilterPanel(); });
  $('sort').addEventListener('change', () => setView({ sort: $('sort').value }));
  $('sort').innerHTML = SORTS.map(([id, name]) => `<option value="${id}">${name}</option>`).join('');
  $('filter-panel').addEventListener('click', (event) => event.stopPropagation());
  document.addEventListener('click', (event) => {
    if (!$('more-menu').hidden && !event.target.closest('#more-menu')) closeMenu();
    if (!$('filter-panel').hidden && !event.target.closest('#filter-panel, #filter-toggle')) toggleFilterPanel(false);
  });
  $('notice-close').addEventListener('click', () => { $('notice').hidden = true; });
  $('search').addEventListener('input', () => { renderRail(); renderList(); });
  $('local-done').addEventListener('click', () => { const on = $('local-done').getAttribute('aria-pressed') === 'true'; on ? setScope('mine') : setView({ stages: ['ready'] }); if (!on) navigate('/tasks'); });
  $('platform-state').addEventListener('click', () => loadPlatform(true).then(() => { renderList(); showNotice(platformClosed() ? closedText() + '。' : '平台当前可提交。'); }));
  $('sync-state').addEventListener('click', () => busy(null, async () => {
    $('sync-text').textContent = '同步中…';
    await Promise.allSettled([loadPrechecks(true), loadTasks()]);
    renderRail(); renderList();
    if (!app.current) renderOverview();
    showNotice('已刷新本地任务和预检。');
  }));
  $('account-chip').addEventListener('click', openAccountSettings);
  $('settings-button').addEventListener('click', () => { closeMenu(); navigate('/settings'); });
  $('new-task').addEventListener('click', () => { closeMenu(); openTaskDialog(); });
  for (const id of ['close-dialog', 'cancel-dialog']) $(id).addEventListener('click', () => $('task-dialog').close());
  $('cancel-unlock').addEventListener('click', () => $('unlock-dialog').close());
  $('task-form').addEventListener('submit', submitTaskDialog);
  document.addEventListener('keydown', (event) => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName) || document.querySelector('dialog[open]');
    if (event.key === 'Escape' && !$('filter-panel').hidden) { toggleFilterPanel(false); return; }
    if (event.key === 'Escape' && !$('more-menu').hidden) { closeMenu(); return; }
    if (event.key === 'Escape' && document.activeElement === $('search')) { $('search').value = ''; renderRail(); renderList(); $('search').blur(); return; }
    if (typing || event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.key === '/') { event.preventDefault(); $('search').focus(); }
    else if (event.key === 'j' || event.key === 'ArrowDown' && document.activeElement?.closest('#list')) { event.preventDefault(); moveSelection(1); }
    else if (event.key === 'k' || event.key === 'ArrowUp' && document.activeElement?.closest('#list')) { event.preventDefault(); moveSelection(-1); }
    else if (event.key === 'Escape' && app.current) navigate('/tasks');
  });
  window.addEventListener('popstate', () => {
    if (app.busy) { history.pushState({ swe: true }, '', '#' + app.route); showNotice('当前操作仍在处理，请稍后切换。'); return; }
    if (!confirmLeave()) { history.pushState({ swe: true }, '', '#' + app.route); return; }
    renderRoute();
  });
  window.addEventListener('resize', syncHeadHeight, { passive: true });
  window.addEventListener('beforeunload', (event) => { if (dirty() || app.busy) { event.preventDefault(); event.returnValue = ''; } });
  if (!location.hash) history.replaceState({ swe: true }, '', '#/tasks');
  try {
    const data = await api('/api/bootstrap');
    app.csrf = data.csrf; app.root = data.root; app.workRoot = data.work_root || '';
    await loadTasks();
    loadAccounts();
    loadPlatform().then(renderList);
    const prechecks = loadPrechecks().then(() => { renderRail(); renderList(); if (!app.current && location.hash !== '#/settings') renderRoute(); }).catch(() => { renderList(); });
    if (location.hash.startsWith('#/prechecks/')) await prechecks;
    await renderRoute();
  } catch (error) { showNotice(error.message, true); }
  setInterval(() => {
    if (app.busy || document.hidden) return;
    refreshAll().catch(() => {});
    loadPrechecks().then(() => { renderRail(); renderList(); }).catch(() => {});
    loadPlatform();
  }, 20000);
}
init();
