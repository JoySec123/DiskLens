const $ = selector => document.querySelector(selector);
const root = $('#root'), scanButton = $('#scan'), cancelButton = $('#cancel'), refreshButton = $('#refresh');
const titles = { overview: '概览', directories: '目录分析', types: '文件类型', files: '大文件', duplicates: '重复文件', history: '扫描记录', ledger: '空间账单', alerts: '提醒', settings: '巡检设置' };
const colors = ['#6398f7', '#53bfa8', '#ad92dc', '#e6b561', '#74b8d9', '#cf8f9f', '#8995a8', '#8fbd76'];
let result = null, busy = false, duplicateBusy = false, currentView = 'overview';
let roots = [], selectedRoots = [], excludes = ['@recycle', '.snapshot'], history = [];
let timer, duplicateTimer, filterTimer, pollErrors = 0, lastApplied = '';
let directoryPath = '', directoryPage = 1, filePage = 1, comparePage = 1, duplicatePage = 1;
let fileSequence = 0, dirSequence = 0, compareSequence = 0, pathsSequence = 0;
let ledger = [], ledgerSequence = 0, alertSequence = 0, settingsSequence = 0, alertPage = 1;

function bytes(value) {
  if (!value) return '0 B';
  const index = Math.max(0, Math.min(Math.floor(Math.log(Math.abs(value)) / Math.log(1024)), 5));
  const amount = value / 1024 ** index;
  return amount.toFixed(index && Math.abs(amount) < 100 ? 1 : 0) + ' ' + ['B', 'KB', 'MB', 'GB', 'TB', 'PB'][index];
}
function delta(value) { return (value > 0 ? '+' : '') + bytes(value); }
function duration(ms) {
  if (ms < 1000) return '< 1 秒';
  const seconds = Math.floor(ms / 1000);
  return seconds >= 60 ? Math.floor(seconds / 60) + ' 分 ' + seconds % 60 + ' 秒' : seconds + ' 秒';
}
function number(value) { return new Intl.NumberFormat('zh-CN').format(value || 0); }
function escape(value) { return String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }
function date(value) { return new Date(typeof value === 'number' ? value / 1e6 : value).toLocaleString('zh-CN', { hour12: false }); }
function basename(path) { return path.split('/').filter(Boolean).pop() || path; }
function icon(name) { return '<img src="./assets/' + name + '.svg" alt="">'; }
function hint(message, error = false) { $('#hint').textContent = message; $('#hint').classList.toggle('error', error); }
function state(message, kind = '') { $('#status').textContent = message; $('#status').dataset.state = kind; }
function empty(element, message = '暂无扫描数据', name = 'folder-open') { element.innerHTML = '<div class="empty">' + icon(name) + '<span>' + escape(message) + '</span></div>'; }
function copyButton(path) { return '<button class="icon-button compact" data-copy="' + escape(path) + '" title="复制路径" aria-label="复制路径">' + icon('copy') + '</button>'; }
function resultRoots(data = result) { return data?.roots?.length ? data.roots : data?.root ? [data.root] : []; }

function controls() {
  const locked = busy || duplicateBusy;
  root.disabled = locked || !roots.length;
  $('#scope').disabled = locked || !roots.length;
  scanButton.disabled = locked || !selectedRoots.length;
  scanButton.hidden = busy; cancelButton.hidden = !busy;
  refreshButton.disabled = locked; $('#progress').hidden = !locked;
  $('#duplicateStart').disabled = locked || !result?.id;
  $('#duplicateStart').hidden = duplicateBusy; $('#duplicateStop').hidden = !duplicateBusy;
  $('#duplicateMin').disabled = locked; $('#duplicateRate').disabled = locked;
  $('#export').disabled = !result?.id;
  $('#saveInspection').disabled = locked;
  $('#scopeLabel').textContent = selectedRoots.length > 1 ? selectedRoots.length + ' 个目录' : '扫描范围';
}
function forgetResults() {
  result = null; history = []; fileSequence++; dirSequence++; compareSequence++; pathsSequence++;
  ledger = []; ledgerSequence++; alertSequence++; settingsSequence++;
  render();
  for (const id of ['#history', '#changes', '#growth', '#duplicates', '#duplicatePaths']) empty($(id), '授权已失效');
  for (const id of ['#compareSummary', '#duplicateSummary', '#volumes', '#comparePager', '#duplicatePager']) $(id).textContent = '';
  $('#pathsDialog').close(); $('#copyDialog').close(); $('#copyValue').value = '';
  $('#compareBefore').replaceChildren(); $('#compareAfter').replaceChildren();
  for (const id of ['#ledgerScope', '#ledgerAfter', '#ledgerBefore', '#ledgerTotals', '#ledgerTrend', '#ledgerCoverage', '#ledgerCategories', '#ledgerCapacity', '#ledgerCapacityAt', '#ledgerLimit', '#alertSummary', '#alertPager', '#scheduleRoots', '#scheduleStatus']) $(id).replaceChildren();
  for (const id of ['#ledgerChanges', '#alerts']) empty($(id), '授权暂不可用');
  $('#scheduleExcludes').value = ''; $('#alertCount').hidden = true;
}
async function request(path, options = {}) {
  const controller = new AbortController(), timeout = setTimeout(() => controller.abort(), 30000);
  try {
    const headers = new Headers(options.headers);
    if (!['GET', 'HEAD', 'OPTIONS'].includes((options.method || 'GET').toUpperCase())) headers.set('X-DiskLens-Request', '1');
    const response = await fetch('./api/' + path, { ...options, headers, signal: controller.signal, cache: 'no-store' });
    const data = await response.json();
    if (!response.ok) {
      if (data.code === 'cross_site_request') { state('请求被拦截', 'error'); hint(data.error, true); }
      else if (response.status === 403 || response.status === 503) { forgetResults(); state('授权暂不可用', 'error'); hint(data.error || '请刷新授权后重试', true); }
      const error = new Error(data.error || '请求失败（' + response.status + '）'); error.status = response.status; error.code = data.code; throw error;
    }
    $('#connection').textContent = '已连接'; return data;
  } catch (error) {
    if (error.name === 'AbortError') throw new Error('连接超时，请重试');
    throw error;
  } finally { clearTimeout(timeout); }
}
function post(path, body) { return request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); }
function metrics(data) {
  $('#totalBytes').textContent = data ? bytes(data.totalBytes) : '--';
  $('#fileCount').textContent = data ? number(data.fileCount) : '--';
  $('#dirCount').textContent = data ? number(data.dirCount) : '--';
  $('#duration').textContent = data ? duration(data.durationMs) : '--';
}
function rootLabel(paths) {
  $('#rootSummary').textContent = paths.length > 1 ? paths.length + ' 个目录' : paths.length ? basename(paths[0]) : '未选择目录';
  $('#rootSummary').title = paths.join('\n');
}
function renderBars(selector, items, type, limit) {
  const element = $(selector);
  if (!result) { empty(element); return; }
  if (!items.length) { empty(element, type === 'directory' ? '没有子目录' : '没有文件'); return; }
  element.innerHTML = items.slice(0, limit).map((item, i) => {
    const label = type === 'directory' ? item.path : item.name;
    const percent = result.totalBytes ? item.bytes / result.totalBytes * 100 : 0;
    const action = type === 'directory' ? 'data-directory="' + escape(item.absolutePath || result.root + '/' + item.path) + '"' : 'data-category="' + escape(item.name) + '"';
    return '<button class="bar-item" ' + action + '><span class="bar-meta"><span class="bar-name" title="' + escape(label) + '">' + icon(type === 'directory' ? 'folder' : 'file') + '<span>' + escape(label) + '</span></span><span class="bar-values">' + bytes(item.bytes) + '<small>' + percent.toFixed(1) + '%</small></span></span><span class="bar-track"><span class="bar-fill" style="width:' + percent + '%;background:' + colors[i % colors.length] + '"></span></span></button>';
  }).join('');
}
function renderOverviewFiles() {
  if (!result) { empty($('#overview-files')); return; }
  if (!result.topFiles.length) { empty($('#overview-files'), '没有文件'); return; }
  $('#overview-files').innerHTML = '<table class="overview-table"><thead><tr><th>#</th><th>文件名称 / 路径</th><th>大小</th></tr></thead><tbody>' + result.topFiles.slice(0, 5).map((item, i) => '<tr><td>' + (i + 1) + '</td><td class="file-cell" title="' + escape(item.absolutePath || item.path) + '"><span class="file-name">' + escape(basename(item.path)) + '</span><span class="file-parent">' + escape(item.path) + '</span></td><td>' + bytes(item.bytes) + '</td></tr>').join('') + '</tbody></table>';
}
function render() {
  metrics(result); rootLabel(result ? resultRoots() : selectedRoots);
  renderBars('#overview-dirs', result?.topDirs || [], 'directory', 5);
  renderBars('#overview-types', result?.categories || [], 'type', 5);
  renderBars('#types', result?.categories || [], 'type', 20); renderOverviewFiles();
  $('#scanTime').textContent = result ? '扫描时间：' + date(result.scannedAt) : '暂无扫描记录';
  $('#skipped').textContent = result?.coverageVersion ? '排除 ' + number(result.excluded) + ' · 符号链接 ' + number(result.symlinks) + ' · 读取失败 ' + number(result.readErrors) : result?.skipped ? '已跳过 ' + number(result.skipped) + ' 项' : '';
  if (!result) {
    for (const id of ['#files', '#dirs']) empty($(id));
    for (const id of ['#filePager', '#dirPager', '#breadcrumbs', '#fileTotal']) $(id).textContent = '';
  }
  controls();
}
function adopt(data) {
  result = data; filePage = directoryPage = 1;
  directoryPath = resultRoots().length === 1 ? resultRoots()[0] : '';
  fileSequence++; dirSequence++; render();
}
function pager(selector, data, change) {
  const element = $(selector), pages = Math.max(1, Math.ceil(data.total / data.pageSize));
  element.innerHTML = '<span>共 ' + number(data.total) + ' 项</span><div><button class="icon-button compact" data-prev title="上一页" aria-label="上一页"' + (data.page <= 1 ? ' disabled' : '') + '>' + icon('chevron-left') + '</button><span>' + data.page + ' / ' + pages + '</span><button class="icon-button compact" data-next title="下一页" aria-label="下一页"' + (data.page >= pages ? ' disabled' : '') + '>' + icon('chevron-right') + '</button></div>';
  element.querySelector('[data-prev]').onclick = () => change(data.page - 1);
  element.querySelector('[data-next]').onclick = () => change(data.page + 1);
}
function filterParams() {
  const params = new URLSearchParams({ id: result.id });
  for (const [key, value] of new FormData($('#filters'))) {
    if (!value.trim()) continue;
    if (key === 'min' || key === 'max') {
      const amount = Number(value) * 1024 ** 2;
      if (!Number.isSafeInteger(Math.floor(amount)) || amount < 0) throw new Error('文件大小超出范围');
      params.set(key, String(Math.floor(amount)));
    } else if (key === 'since' || key === 'until') {
      const day = new Date(value + 'T00:00:00'); if (key === 'until') day.setDate(day.getDate() + 1);
      params.set(key, day.toISOString());
    } else params.set(key, value);
  }
  return params;
}
function entryTable(items, directories = false) {
  return '<table class="entry-table"><thead><tr><th>名称 / 路径</th><th>逻辑大小</th><th>' + (directories ? '文件数' : '类型') + '</th><th>修改时间</th><th></th></tr></thead><tbody>' + items.map(item => {
    const name = item.kind === 'directory' ? '<button class="directory-link" data-directory="' + escape(item.path) + '">' + icon('folder') + '<span>' + escape(item.name) + '</span></button>' : '<span class="file-name">' + escape(item.name) + '</span>';
    return '<tr><td class="file-cell" title="' + escape(item.path) + '">' + name + '<span class="file-parent">' + escape(item.path) + '</span><span class="mobile-meta">' + (directories ? number(item.files) + ' 个文件' : escape(item.category)) + '</span></td><td>' + bytes(item.bytes) + '</td><td>' + (directories ? number(item.files) : escape(item.category)) + '</td><td>' + date(item.modified) + '</td><td>' + copyButton(item.path) + '</td></tr>';
  }).join('') + '</tbody></table>';
}
async function loadFiles() {
  if (!result?.id) { empty($('#files')); return; }
  const sequence = ++fileSequence;
  try {
    const params = filterParams(); params.set('page', filePage); const data = await request('files?' + params);
    if (sequence !== fileSequence) return;
    if (data.items.length) $('#files').innerHTML = entryTable(data.items); else empty($('#files'), '没有匹配的文件', 'file-search');
    $('#fileTotal').textContent = number(data.total) + ' 个 · ' + bytes(data.totalBytes);
    pager('#filePager', data, page => { filePage = page; loadFiles(); });
  } catch (error) { if (sequence === fileSequence) { empty($('#files'), error.message); $('#filePager').textContent = ''; $('#fileTotal').textContent = ''; } }
}
function breadcrumbs() {
  const parts = [{ name: '扫描范围', path: '' }];
  const parentRoot = resultRoots().find(path => directoryPath === path || directoryPath.startsWith(path + '/'));
  if (parentRoot) {
    parts.push({ name: basename(parentRoot), path: parentRoot }); let path = parentRoot;
    for (const part of directoryPath.slice(parentRoot.length).split('/').filter(Boolean)) { path += '/' + part; parts.push({ name: part, path }); }
  }
  $('#breadcrumbs').innerHTML = parts.map(item => '<button class="text-button" data-directory="' + escape(item.path) + '" title="' + escape(item.path) + '">' + escape(item.name) + '</button>').join('<span>/</span>');
  $('#dirUp').disabled = !directoryPath;
}
async function loadDirectories() {
  if (!result?.id) { empty($('#dirs')); return; }
  const sequence = ++dirSequence; breadcrumbs();
  try {
    const data = await request('browse?' + new URLSearchParams({ id: result.id, path: directoryPath, page: directoryPage }));
    if (sequence !== dirSequence) return;
    if (data.items.length) $('#dirs').innerHTML = entryTable(data.items, true); else empty($('#dirs'), '目录为空');
    pager('#dirPager', data, page => { directoryPage = page; loadDirectories(); });
  } catch (error) { if (sequence === dirSequence) { empty($('#dirs'), error.message); $('#dirPager').textContent = ''; } }
}
async function loadCapacity() {
  try {
    const data = await request('capacity');
    $('#volumes').innerHTML = data.items.map(item => {
      const used = item.total - item.free, percent = item.total ? used / item.total * 100 : 0;
      return '<div class="volume-row"><div><span class="volume-name" title="' + escape(item.roots.join('\n')) + '">' + escape(item.roots.map(basename).join('、')) + '</span><span class="subtle">可用 ' + bytes(item.available) + ' / 总计 ' + bytes(item.total) + '</span></div><div class="bar-track"><span class="bar-fill" style="width:' + Math.min(100, percent) + '%;background:' + (percent > 90 ? '#d78871' : '#53bfa8') + '"></span></div></div>';
    }).join('');
    if (!data.items.length) empty($('#volumes'), data.unavailable?.length ? '暂时无法读取存储容量' : '暂无存储容量');
  } catch (error) { empty($('#volumes'), error.message); }
}
async function loadHistory() {
  try {
    history = await request('history');
    if (!history.length) empty($('#history'), '暂无扫描记录');
    else $('#history').innerHTML = '<table class="history-table"><thead><tr><th>扫描时间 / 范围</th><th>逻辑大小</th><th>文件数</th><th></th></tr></thead><tbody>' + history.map(item => '<tr><td title="' + escape(resultRoots(item).join('\n')) + '"><span class="file-name">' + date(item.scannedAt) + '</span><span class="file-parent">' + escape(resultRoots(item).join('、')) + '</span></td><td>' + bytes(item.totalBytes) + '</td><td>' + number(item.fileCount) + '</td><td><button class="icon-button compact" data-history="' + escape(item.id) + '" title="查看记录" aria-label="查看记录">' + icon('folder-open') + '</button></td></tr>').join('') + '</tbody></table>';
    const selected = $('#compareAfter').value;
    $('#compareAfter').replaceChildren(...history.map(item => new Option(date(item.scannedAt), item.id)));
    if (history.some(item => item.id === selected)) $('#compareAfter').value = selected;
    compareChoices();
  } catch (error) { empty($('#history'), error.message); }
}
function sameScope(a, b) {
  const normalized = values => JSON.stringify([...(values || [])].sort());
  return normalized(resultRoots(a)) === normalized(resultRoots(b)) && normalized(a.excludes) === normalized(b.excludes);
}
function compareChoices() {
  const after = history.find(item => item.id === $('#compareAfter').value), previous = $('#compareBefore').value;
  const candidates = history.filter(item => after && item.id !== after.id && sameScope(item, after));
  $('#compareBefore').replaceChildren(...candidates.map(item => new Option(date(item.scannedAt), item.id)));
  if (candidates.some(item => item.id === previous)) $('#compareBefore').value = previous;
  $('#compareButton').disabled = !candidates.length;
  compareSequence++; $('#compareSummary').textContent = ''; $('#growth').textContent = ''; $('#comparePager').textContent = '';
  empty($('#changes'), candidates.length ? '尚未选择对比结果' : '暂无同范围的另一份记录');
}
function changesTable(items) {
  const names = { added: '新增', removed: '消失', changed: '变化' };
  return '<table class="changes-table"><thead><tr><th>路径</th><th>状态</th><th>之前</th><th>之后</th><th>变化</th></tr></thead><tbody>' + items.map(item => '<tr><td title="' + escape(item.path) + '"><span class="file-name">' + escape(item.path) + '</span></td><td>' + names[item.change] + '</td><td>' + bytes(item.before) + '</td><td>' + bytes(item.after) + '</td><td class="' + (item.delta > 0 ? 'growth' : 'reduction') + '">' + delta(item.delta) + '</td></tr>').join('') + '</tbody></table>';
}
async function loadComparison() {
  const sequence = ++compareSequence; $('#compareButton').disabled = true;
  try {
    const data = await request('compare?' + new URLSearchParams({ before: $('#compareBefore').value, after: $('#compareAfter').value, page: comparePage }));
    if (sequence !== compareSequence) return;
    $('#compareSummary').textContent = '总量 ' + delta(data.deltaBytes) + ' · 新增 ' + number(data.added) + ' · 消失 ' + number(data.removed) + ' · 变化 ' + number(data.changed) + (data.partial ? ' · 含跳过项，结果可能不完整' : '');
    $('#growth').innerHTML = data.directories.length ? '<h3>一级目录变化</h3><div class="table-wrap">' + changesTable(data.directories) + '</div>' : '';
    if (data.items.length) $('#changes').innerHTML = changesTable(data.items); else empty($('#changes'), '没有文件变化');
    pager('#comparePager', data, page => { comparePage = page; loadComparison(); });
  } catch (error) { if (sequence === compareSequence) { empty($('#changes'), error.message); $('#compareSummary').textContent = ''; $('#growth').textContent = ''; $('#comparePager').textContent = ''; } }
  finally { $('#compareButton').disabled = !$('#compareBefore').value; }
}
function renderDuplicates(data) {
  const job = data?.job || data;
  duplicateBusy = job?.state === 'running' || job?.state === 'cancelling';
  $('#duplicateStop').disabled = job?.state === 'cancelling'; controls();
  $('#duplicatePager').textContent = ''; $('#duplicateSummary').textContent = '';
  if (!job || job.scanId !== result?.id) {
    $('#duplicateStatus').textContent = duplicateBusy ? '另一份扫描记录正在检测' : '当前记录暂无检测结果';
    empty($('#duplicates'), '暂无检测结果', 'files'); return;
  }
  if (duplicateBusy) {
    $('#duplicateStatus').textContent = (job.state === 'cancelling' ? '正在停止' : job.phase === 'verifying' ? '逐字节确认中' : '读取内容中') + ' · ' + number(job.processed) + ' / ' + number(job.candidates) + ' 个候选文件 · 已读 ' + bytes(job.readBytes) + (job.currentPath ? ' · ' + job.currentPath : '');
    empty($('#duplicates'), '检测中', 'files');
  } else if (job.state === 'completed') {
    $('#duplicateStatus').textContent = '检测完成 · 跳过 ' + number(job.skipped) + ' 个文件或硬链接别名';
    $('#duplicateSummary').textContent = number(job.groups) + ' 组 · 重复逻辑大小 ' + bytes(job.logicalDuplicateBytes) + '（非可回收空间）';
    if (data.groups?.length) $('#duplicates').innerHTML = data.groups.map(group => '<article class="duplicate-group"><header><strong>' + number(group.count) + ' 个相同文件</strong><span>' + bytes(group.bytes) + ' / 个</span><button class="icon-button compact" data-group="' + group.id + '" title="查看全部路径" aria-label="查看全部路径">' + icon('folder-open') + '</button></header><ul>' + group.paths.map(path => '<li><span title="' + escape(path) + '">' + escape(path) + '</span>' + copyButton(path) + '</li>').join('') + '</ul></article>').join('');
    else empty($('#duplicates'), '未发现符合条件的重复文件', 'files');
    pager('#duplicatePager', data, page => { duplicatePage = page; loadDuplicates(); });
  } else {
    $('#duplicateStatus').textContent = job.state === 'cancelled' ? '检测已停止，未发布结果' : job.error || '检测失败';
    empty($('#duplicates'), '暂无检测结果', 'files');
  }
}
async function loadDuplicates() {
  clearTimeout(duplicateTimer);
  try { renderDuplicates(await request('duplicates?' + new URLSearchParams({ page: duplicatePage, pageSize: 10 }))); }
  catch (error) { $('#duplicateStatus').textContent = error.message; if (error.status === 403 || error.status === 404) { duplicateBusy = false; controls(); } }
  if (duplicateBusy) duplicateTimer = setTimeout(loadDuplicates, 1000);
}
async function loadPaths(group, page = 1) {
  const sequence = ++pathsSequence;
  try {
    const data = await request('duplicates?' + new URLSearchParams({ group, page, pageSize: 25 }));
    if (sequence !== pathsSequence) return;
    if (!data?.paths) throw new Error('检测结果已更新');
    $('#duplicatePaths').innerHTML = data.paths.map(path => '<div class="path-row"><span>' + escape(path) + '</span>' + copyButton(path) + '</div>').join('');
    pager('#pathsPager', data, next => loadPaths(group, next));
    if (!$('#pathsDialog').open) $('#pathsDialog').showModal();
  } catch (error) { hint(error.message, true); }
}
async function startDuplicates() {
  if (!result?.id || busy || duplicateBusy) return;
  const minBytes = Math.floor(Number($('#duplicateMin').value) * 1024 ** 2);
  if (!Number.isSafeInteger(minBytes) || minBytes < 0) { $('#duplicateStatus').textContent = '最小大小无效'; return; }
  duplicateBusy = true; controls(); duplicatePage = 1;
  try { renderDuplicates(await post('duplicates', { id: result.id, minBytes, rateMiB: Number($('#duplicateRate').value) })); duplicateTimer = setTimeout(loadDuplicates, 300); }
  catch (error) { duplicateBusy = false; controls(); $('#duplicateStatus').textContent = error.message; await loadDuplicates(); }
}
async function navigate(view) {
  if (!titles[view]) return; currentView = view;
  document.querySelectorAll('[data-view]').forEach(button => {
    const active = button.dataset.view === view; button.classList.toggle('active', active);
    if (active) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current');
  });
  document.querySelectorAll('.view').forEach(element => { element.hidden = element.id !== 'view-' + view; });
  $('#viewTitle').textContent = titles[view];
  $('.scan-toolbar').hidden = view === 'settings';
  $('.summary').hidden = ['ledger', 'alerts', 'settings'].includes(view);
  if (view === 'files') await loadFiles(); if (view === 'directories') await loadDirectories();
  if (view === 'history') await loadHistory(); if (view === 'duplicates') await loadDuplicates();
  if (view === 'ledger') await loadLedger(); if (view === 'alerts') await loadAlerts(); if (view === 'settings') await loadInspection();
}
async function loadConfig() {
  refreshButton.disabled = scanButton.disabled = true;
  try {
    const config = await request('config'); roots = config.allowedRoots || [];
    if (!selectedRoots.length && config.lastScan) { selectedRoots = config.scanDefaults?.roots || []; excludes = config.scanDefaults?.excludes || []; }
    selectedRoots = selectedRoots.filter(path => roots.includes(path));
    if (!selectedRoots.length && roots.length) selectedRoots = [roots[0]];
    root.replaceChildren(...(roots.length ? roots.map(path => new Option(path, path)) : [new Option('尚未授权目录', '')]));
    root.value = selectedRoots[0] || ''; adopt(config.lastScan || null); lastApplied = config.lastScan?.id || '';
    hint(config.notice || (roots.length ? '已授权 ' + roots.length + ' 个目录' : '未发现授权目录，请在应用设置的访问权限中添加目录。'));
    state(result ? '扫描完成' : roots.length ? '准备就绪' : '等待授权', result ? 'completed' : '');
    await poll(); await loadDuplicates(); await loadCapacity(); await loadAlerts(); await navigate(currentView);
  } catch (error) {
    roots = []; selectedRoots = []; forgetResults(); root.replaceChildren(new Option('授权读取失败', '')); rootLabel([]);
    state('授权读取失败', 'error'); hint(error.message, true); $('#connection').textContent = '连接异常';
  } finally { controls(); }
}
function applyJob(job) {
  if (!job) return; busy = job.state === 'running' || job.state === 'cancelling'; cancelButton.disabled = job.state === 'cancelling';
  if (busy) {
    if (result) adopt(null); metrics(job.progress); rootLabel(job.roots || [job.root]);
    state(job.state === 'cancelling' ? '正在停止' : '扫描中', 'running');
    hint('已扫描 ' + number(job.progress.fileCount) + ' 个文件 · ' + bytes(job.progress.totalBytes) + (job.progress.currentPath ? ' · ' + job.progress.currentPath : ''));
  } else if (job.state === 'completed' && job.id !== lastApplied) {
    lastApplied = job.id; adopt(job.result); state('扫描完成', 'completed'); hint('已完成 · ' + number(result.fileCount) + ' 个文件' + (result.readErrors ? ' · 存在读取失败' : '')); navigate(currentView); loadCapacity(); loadAlerts();
  } else if (job.state === 'cancelled') {
    state('已停止'); hint('扫描已停止'); metrics(job.progress); $('#scanTime').textContent = '本次扫描未完成';
  } else if (job.state === 'error') { state('扫描失败', 'error'); hint(job.error || '扫描失败，请重试', true); }
  controls();
}
async function poll() {
  clearTimeout(timer);
  try { applyJob(await request('scan')); pollErrors = 0; }
  catch (error) {
    if (error.status === 403) { busy = false; roots = []; selectedRoots = []; state('授权已失效', 'error'); hint(error.message, true); controls(); return; }
    pollErrors++; hint('暂时无法获取扫描状态：' + error.message, true); $('#connection').textContent = '连接异常'; if (!busy) throw error;
  }
  if (!document.hidden) timer = setTimeout(() => poll().catch(error => hint(error.message, true)), busy ? (pollErrors ? 3000 : 700) : 15000);
}
async function startScan() {
  if (busy || duplicateBusy || !selectedRoots.length) return;
  busy = true; controls(); cancelButton.disabled = true; state('正在启动', 'running'); hint('正在检查授权目录');
  try {
    const job = await post('scan', { roots: selectedRoots, excludes }); lastApplied = ''; adopt(null); renderDuplicates(null); applyJob(job); timer = setTimeout(poll, 250);
  } catch (error) {
    busy = false; controls(); state(error.code === 'cross_site_request' ? '请求被拦截' : '启动失败', 'error'); hint(error.message, true);
    if (error.code === 'cross_site_request') return;
    try { await poll(); } catch { /* Status can be retried after connection recovery. */ }
  }
}
async function copyPath(value, button) {
  try {
    if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(value);
    else {
      const input = document.createElement('textarea'); input.value = value; input.className = 'clipboard-input'; document.body.append(input); input.select();
      const success = document.execCommand('copy'); input.remove(); if (!success) throw new Error('clipboard unavailable');
    }
    button.innerHTML = icon('check'); setTimeout(() => { if (button.isConnected) button.innerHTML = icon('copy'); }, 1500);
  } catch { $('#copyValue').value = value; $('#copyDialog').showModal(); $('#copyValue').select(); }
}

function ledgerTable(items, heading = '目录 / 类型') {
  return '<table class="ledger-table"><thead><tr><th>' + heading + '</th><th>基准</th><th>当前</th><th>变化</th></tr></thead><tbody>' + items.map(item => '<tr><td title="' + escape(item.path) + '"><span class="file-name">' + escape(item.path + (item.kind === 'root_files' ? '（根目录文件）' : '')) + '</span></td><td>' + bytes(item.before) + '</td><td>' + bytes(item.after) + '</td><td class="' + (item.delta > 0 ? 'growth' : 'reduction') + '">' + delta(item.delta) + '</td></tr>').join('') + '</tbody></table>';
}
function ledgerTotals(before, after) {
  $('#ledgerTotals').innerHTML = [['基准文件量', before ? bytes(before.totalBytes) : '--'], ['当前文件量', after ? bytes(after.totalBytes) : '--'], ['逻辑大小变化', before && after ? delta(after.totalBytes - before.totalBytes) : '--']].map(([name, value]) => '<div><strong>' + value + '</strong><span>' + name + '</span></div>').join('');
}
async function loadLedger() {
  const sequence = ++ledgerSequence;
  try {
    const items = await request('ledger'); if (sequence !== ledgerSequence) return;
    ledger = items;
    const previous = $('#ledgerScope').value, scopes = [...new Set(ledger.map(item => item.scope))];
    $('#ledgerScope').replaceChildren(...scopes.map(scope => {
      const item = ledger.find(item => item.scope === scope);
      return new Option(resultRoots(item).join('、') + ' · ' + item.excludes.length + ' 项排除', scope);
    }));
    if (scopes.includes(previous)) $('#ledgerScope').value = previous;
    if (!items.length) {
      $('#ledgerAfter').replaceChildren(); $('#ledgerBefore').replaceChildren(); ledgerTotals(null, null);
      for (const id of ['#ledgerChanges', '#ledgerCategories', '#ledgerCapacity']) empty($(id), '暂无账单');
      for (const id of ['#ledgerCoverage', '#ledgerCapacityAt', '#ledgerLimit', '#ledgerTrend']) $(id).textContent = '';
      return;
    }
    renderLedgerScope();
  } catch (error) { if (sequence === ledgerSequence) empty($('#ledgerChanges'), error.message); }
}
function renderLedgerScope() {
  const previous = $('#ledgerAfter').value, items = ledger.filter(item => item.scope === $('#ledgerScope').value);
  $('#ledgerAfter').replaceChildren(...items.map(item => new Option(date(item.scannedAt), item.id)));
  if (items.some(item => item.id === previous)) $('#ledgerAfter').value = previous;
  renderLedgerAfter();
}
function renderLedgerAfter() {
  ledgerSequence++;
  const items = ledger.filter(item => item.scope === $('#ledgerScope').value), after = items.find(item => item.id === $('#ledgerAfter').value);
  const candidates = items.slice(items.indexOf(after) + 1), previous = $('#ledgerBefore').value;
  $('#ledgerBefore').replaceChildren(...candidates.map(item => new Option(date(item.scannedAt), item.id)));
  if (candidates.some(item => item.id === previous)) $('#ledgerBefore').value = previous;
  if (!after) return;
  ledgerTotals(null, after);
  $('#ledgerCoverage').textContent = resultRoots(after).join('、'); $('#ledgerCoverage').title = '排除规则：' + after.excludes.join('、');
  $('#ledgerLimit').textContent = '';
  const trend = items.slice(items.indexOf(after), items.indexOf(after) + 8).reverse(), maximum = Math.max(1, ...trend.map(item => item.totalBytes));
  $('#ledgerTrend').innerHTML = trend.map(item => '<div class="trend-column" title="' + escape(date(item.scannedAt) + ' · ' + bytes(item.totalBytes)) + '"><span style="height:' + Math.max(2, item.totalBytes / maximum * 100) + '%"></span><small>' + new Date(item.scannedAt).toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' }) + '</small></div>').join('');
  $('#ledgerCapacityAt').textContent = date(after.capacityAt);
  $('#ledgerCapacity').innerHTML = (after.volumes || []).map(item => '<div class="volume-row"><div><span class="volume-name" title="' + escape(item.roots.join('、')) + '">' + escape(item.roots.map(basename).join('、')) + '</span><span class="subtle">可用 ' + bytes(item.available) + ' / 总计 ' + bytes(item.total) + '</span></div></div>').join('');
  if (after.unavailable?.length) $('#ledgerCapacity').innerHTML += '<p class="hint error">容量读取失败：' + escape(after.unavailable.join('、')) + '</p>';
  if (!after.volumes?.length && !after.unavailable?.length) empty($('#ledgerCapacity'), '暂无容量记录');
  if (!candidates.length) {
    for (const id of ['#ledgerChanges', '#ledgerCategories']) empty($(id), '暂无同范围的基准账单');
  } else loadLedgerComparison();
}
async function loadLedgerComparison() {
  const sequence = ++ledgerSequence;
  try {
    const data = await request('ledger?' + new URLSearchParams({ before: $('#ledgerBefore').value, after: $('#ledgerAfter').value }));
    if (sequence !== ledgerSequence) return;
    ledgerTotals(data.before, data.after);
    $('#ledgerCoverage').textContent = (data.partial ? '覆盖不完整 · ' : '') + '基准 ' + date(data.before.scannedAt) + ' → 当前 ' + date(data.after.scannedAt);
    $('#ledgerCoverage').classList.toggle('error', data.partial);
    if (data.directories.length) $('#ledgerChanges').innerHTML = ledgerTable(data.directories, '一级目录 / 根目录文件'); else empty($('#ledgerChanges'), '目录大小没有变化');
    $('#ledgerLimit').textContent = data.truncated ? '显示变化最大的 100 项' : '';
    const categories = [...new Set([...data.before.categories, ...data.after.categories].map(item => item.name))].map(name => {
      const before = data.before.categories.find(item => item.name === name)?.bytes || 0, after = data.after.categories.find(item => item.name === name)?.bytes || 0;
      return { path: name, before, after, delta: after - before };
    }).sort((a, b) => Math.abs(b.delta) - Math.abs(a.delta));
    if (categories.length) $('#ledgerCategories').innerHTML = ledgerTable(categories, '文件类型'); else empty($('#ledgerCategories'), '没有文件类型记录');
  } catch (error) {
    if (sequence === ledgerSequence) {
      empty($('#ledgerChanges'), error.message); empty($('#ledgerCategories'), '暂无对比结果'); ledgerTotals(null, null);
    }
  }
}
async function loadAlerts() {
  const sequence = ++alertSequence;
  try {
    const data = await request('alerts?page=' + alertPage); if (sequence !== alertSequence) return;
    $('#alertCount').hidden = !data.unread; $('#alertCount').textContent = data.unread > 99 ? '99+' : data.unread;
    $('#alertSummary').textContent = number(data.unread) + ' 条未确认';
    if (!data.items.length) empty($('#alerts'), '暂无提醒', 'bell');
    else $('#alerts').innerHTML = data.items.map(item => '<article class="alert-row' + (item.acknowledged ? ' read' : '') + '"><div><p>' + escape(item.message) + '</p><span class="subtle">' + date(item.at) + (item.acknowledged ? ' · 已确认' : '') + '</span></div>' + (item.acknowledged ? '' : '<button class="icon-button" data-alert="' + item.id + '" title="确认提醒" aria-label="确认提醒">' + icon('check') + '</button>') + '</article>').join('');
    pager('#alertPager', data, page => { alertPage = page; loadAlerts(); });
  } catch (error) { if (sequence === alertSequence) { empty($('#alerts'), error.message); $('#alertPager').textContent = ''; } }
}
async function acknowledgeAlert(id) {
  try { await post('alerts', { id }); await loadAlerts(); } catch (error) { hint(error.message, true); }
}
async function loadInspection() {
  const sequence = ++settingsSequence;
  $('#settingsMessage').textContent = '';
  try {
    const data = await request('inspection'); if (sequence !== settingsSequence) return;
    const settings = data.settings;
    $('#scheduleEnabled').checked = settings.enabled; $('#scheduleAt').value = settings.at; $('#scheduleZone').value = settings.timezone;
    $('#scanRate').value = String(settings.entriesPerSecond);
    if (!$('#scanRate').value) { $('#scanRate').add(new Option(settings.entriesPerSecond + ' 项/秒', settings.entriesPerSecond)); $('#scanRate').value = settings.entriesPerSecond; }
    $('#scanTimeout').value = settings.timeoutMinutes; $('#growthEnabled').checked = settings.growthEnabled;
    $('#growthGB').value = settings.growthBytes / 1024 ** 3; $('#growthPercent').value = settings.growthPercent;
    $('#freeEnabled').checked = settings.freeEnabled; $('#freePercent').value = settings.freePercent;
    $('#scheduleExcludes').value = settings.excludes.join('\n');
    const chosen = settings.roots.length ? settings.roots : selectedRoots, choices = [...new Set([...roots, ...settings.roots])];
    $('#scheduleRoots').innerHTML = choices.map(path => '<label><input type="checkbox" name="scheduleRoot" value="' + escape(path) + '"' + (chosen.includes(path) ? ' checked' : '') + '><span>' + escape(path) + '</span></label>').join('');
    const states = { running: '运行中', completed: '已完成', cancelled: '已停止', error: '扫描失败', interrupted: '上次运行被中断', authorization_failed: '授权检查未通过' };
    $('#scheduleStatus').textContent = !data.scopeValid ? '任务目录授权已失效，请重新选择' : (settings.enabled ? '下次启动窗口：' + (data.nextRun ? date(data.nextRun) + ' 起 1 小时内' : '待确定') : '定时扫描未启用') + (data.lastAttempt ? ' · 上次 ' + date(data.lastAttempt) + ' ' + (states[data.lastState] || '') : '');
    $('#scheduleStatus').classList.toggle('error', !data.scopeValid);
  } catch (error) { if (sequence === settingsSequence) { $('#settingsMessage').textContent = error.message; $('#settingsMessage').classList.add('error'); } }
}
async function saveInspection(event) {
  event.preventDefault(); $('#saveInspection').disabled = true; $('#settingsMessage').textContent = '';
  const growthBytes = Math.round(Number($('#growthGB').value) * 1024 ** 3);
  try {
    if (!Number.isSafeInteger(growthBytes) || growthBytes < 1) throw new Error('增长量阈值无效');
    await post('inspection', {
      enabled: $('#scheduleEnabled').checked, at: $('#scheduleAt').value, timezone: $('#scheduleZone').value,
      roots: [...document.querySelectorAll('[name="scheduleRoot"]:checked')].map(input => input.value), excludes: $('#scheduleExcludes').value.split('\n').map(line => line.trim()).filter(Boolean),
      entriesPerSecond: Number($('#scanRate').value), timeoutMinutes: Number($('#scanTimeout').value),
      growthEnabled: $('#growthEnabled').checked, growthBytes, growthPercent: Number($('#growthPercent').value),
      freeEnabled: $('#freeEnabled').checked, freePercent: Number($('#freePercent').value),
    });
    await loadInspection(); $('#settingsMessage').textContent = '设置已保存'; $('#settingsMessage').classList.remove('error');
  } catch (error) { $('#settingsMessage').textContent = error.message; $('#settingsMessage').classList.add('error'); }
  finally { controls(); }
}
document.addEventListener('click', event => {
  const button = event.target.closest('button'); if (!button || button.disabled) return;
  if (button.dataset.view) navigate(button.dataset.view); if (button.dataset.open) navigate(button.dataset.open);
  if (button.hasAttribute('data-directory')) { directoryPath = button.dataset.directory; directoryPage = 1; navigate('directories'); }
  if (button.hasAttribute('data-category')) { $('#filters').reset(); $('#fileCategory').value = button.dataset.category; filePage = 1; navigate('files'); }
  if (button.dataset.copy) copyPath(button.dataset.copy, button);
  if (button.dataset.history) { const item = history.find(item => item.id === button.dataset.history); if (item) { adopt(item); state('历史记录'); hint('历史快照 · ' + date(item.scannedAt)); navigate('overview'); } }
  if (button.dataset.close) $('#' + button.dataset.close).close(); if (button.dataset.group) loadPaths(button.dataset.group);
  if (button.dataset.alert) acknowledgeAlert(Number(button.dataset.alert));
});
root.addEventListener('change', () => { selectedRoots = [root.value]; controls(); if (!result) rootLabel(selectedRoots); });
$('#scope').onclick = () => {
  $('#rootChecks').innerHTML = roots.map(path => '<label><input type="checkbox" name="scanRoot" value="' + escape(path) + '"' + (selectedRoots.includes(path) ? ' checked' : '') + '><span>' + escape(path) + '</span></label>').join('');
  $('#excludes').value = excludes.join('\n'); $('#scopeError').textContent = ''; $('#scopeDialog').showModal();
};
$('#scopeForm').onsubmit = event => {
  event.preventDefault(); const checked = [...document.querySelectorAll('[name="scanRoot"]:checked')].map(input => input.value);
  if (!checked.length || checked.length > 32) { $('#scopeError').textContent = '请选择 1 至 32 个目录'; return; }
  selectedRoots = checked; excludes = $('#excludes').value.split('\n').map(line => line.trim()).filter(Boolean);
  root.value = checked[0]; controls(); if (!result) rootLabel(selectedRoots); $('#scopeDialog').close();
};
$('#dirUp').onclick = () => { directoryPath = resultRoots().includes(directoryPath) ? '' : directoryPath.slice(0, directoryPath.lastIndexOf('/')); directoryPage = 1; loadDirectories(); };
$('#filters').onsubmit = event => event.preventDefault();
$('#advancedToggle').onclick = () => { const visible = $('#advancedFilters').hidden; $('#advancedFilters').hidden = !visible; $('#advancedToggle').setAttribute('aria-expanded', String(visible)); };
function filterChange() { clearTimeout(filterTimer); fileSequence++; filePage = 1; filterTimer = setTimeout(loadFiles, 180); }
$('#filters').addEventListener('input', filterChange); $('#filters').addEventListener('change', filterChange);
$('#filters').addEventListener('reset', () => setTimeout(filterChange, 0));
$('#export').onclick = () => { if (!result?.id) return; try { const anchor = document.createElement('a'); anchor.href = './api/export?' + filterParams(); anchor.download = 'DiskLens.csv'; anchor.click(); } catch (error) { hint(error.message, true); } };
$('#compareAfter').onchange = compareChoices;
$('#ledgerScope').onchange = renderLedgerScope; $('#ledgerAfter').onchange = renderLedgerAfter; $('#ledgerBefore').onchange = loadLedgerComparison;
$('#inspectionForm').onsubmit = saveInspection;
document.addEventListener('visibilitychange', () => { if (!document.hidden) poll().catch(error => hint(error.message, true)); else clearTimeout(timer); });
$('#compareForm').onsubmit = event => { event.preventDefault(); comparePage = 1; loadComparison(); };
$('#duplicateStart').onclick = startDuplicates;
$('#duplicateStop').onclick = async () => { $('#duplicateStop').disabled = true; try { renderDuplicates(await request('duplicates', { method: 'DELETE' })); } catch (error) { $('#duplicateStatus').textContent = error.message; $('#duplicateStop').disabled = false; } };
refreshButton.onclick = loadConfig; scanButton.onclick = startScan;
cancelButton.onclick = async () => { cancelButton.disabled = true; try { applyJob(await request('scan', { method: 'DELETE' })); } catch (error) { hint(error.message, true); cancelButton.disabled = false; } };
render(); loadConfig();
