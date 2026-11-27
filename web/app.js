'use strict';

const $ = id => document.getElementById(id);
const labels = {
  created: '已生成', reused: '已复用', manual_selection: '待手动筛选',
  over_limit: '超出大小上限', size_unknown: '大小待确认', paused: '已暂停',
  pending_confirmation: '待核对', failed: '失败',
  share_active: '分享有效', share_inactive: '已失效',
};
const rows = new Map();
let running = false;
let qrTimer;
let qrRun = 0;
let loggedIn = false;
let currentLimit = '100G';

async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const previousFocus = document.activeElement;
  const field = document.createElement('textarea');
  field.value = text;
  field.readOnly = true;
  field.className = 'clipboard-field';
  document.body.append(field);
  try {
    field.select();
    if (!document.execCommand('copy')) throw new Error('浏览器无法复制，请使用导出功能');
  } finally {
    field.remove();
    previousFocus?.focus({preventScroll: true});
  }
}

async function api(path, data) {
  const options = data === undefined ? undefined : {
    method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(data),
  };
  const response = await fetch('/api/v1/' + path, options);
  const value = await response.json();
  if (!response.ok || value.error) throw new Error(value.error || '请求失败');
  return value;
}

function alertWork(message = '') {
  $('notice').textContent = message;
  $('notice').hidden = !message;
}

function action(text, handler, onError = alertWork) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'secondary';
  button.textContent = text;
  button.addEventListener('click', async () => {
    button.disabled = true;
    try { await handler(); } catch (err) { onError(err.message); }
    finally { button.disabled = false; }
  });
  return button;
}

function link(address) {
  const a = document.createElement('a');
  a.href = address;
  a.textContent = address;
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  return a;
}

function acceptedLinks() {
  return [...rows.values()].filter(row => row.link).map(row => row.link).join('\n');
}

function fileSize(number) {
  const unit = number >= 2 ** 40 ? 'T' : 'G';
  return (number / 2 ** (unit === 'T' ? 40 : 30)).toLocaleString('zh-CN', {maximumFractionDigits: 3}) + unit;
}

function detail(text) {
  const el = document.createElement('span');
  el.className = 'row-note';
  el.textContent = text;
  return el;
}

function recordDate(seconds) {
  if (!seconds) return '';
  const date = new Date(seconds * 1000);
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString('zh-CN', {hour12: false, dateStyle: 'short', timeStyle: 'short'});
}

function csvCell(value) {
  let text = String(value ?? '');
  if (/^[\s\uFEFF]*[=+\-@]|^[\t\r\n]/.test(text)) text = "'" + text;
  return '"' + text.replaceAll('"', '""') + '"';
}

function exportShares(items) {
  const table = [
    ['标题', '分享链接', '分享时间', '状态'],
    ...items.map(row => [row.title, row.link, recordDate(row.recorded_at), labels[row.status]]),
  ];
  const filename = '我的分享.csv';
  const blob = new Blob(['\uFEFF', table.map(row => row.map(csvCell).join(',')).join('\r\n') + '\r\n'], {type: 'text/csv;charset=utf-8'});
  const href = URL.createObjectURL(blob);
  const download = document.createElement('a');
  download.href = href;
  download.download = filename;
  download.click();
  setTimeout(() => URL.revokeObjectURL(href), 1000);
}

const recordIcons = {
  copy: 'M8 7h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V9a2 2 0 0 1 2-2ZM4 16H3V5a2 2 0 0 1 2-2h11v1',
  unlink: 'M10 8l2.2-2.2a4 4 0 0 1 5.7 5.7L16 13.4M14 16l-2.2 2.2a4 4 0 0 1-5.7-5.7L8 10.6M4 4l16 16',
  remove: 'M4 7h16M10 11v6m4-6v6M6 7l1 13h10l1-13M9 7V4h6v3',
};

function recordIconButton(control, icon, label) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS(ns, 'path');
  path.setAttribute('d', recordIcons[icon]);
  svg.append(path);
  control.classList.add('record-icon-button');
  control.setAttribute('aria-label', label);
  control.title = label;
  control.replaceChildren(svg);
  return control;
}

function toggleRecord(selection, key, additive) {
  if (additive && selection.delete(key)) return;
  if (!additive) selection.clear();
  selection.add(key);
}

function enableRectangleSelection(list, selection, updateSelection, enabled) {
  let drag;
  let frame;

  function draw() {
    const {startX, startY, clientX, clientY, original, additive, box} = drag;
    const bounds = list.getBoundingClientRect();
    const x = clientX - bounds.left;
    const y = clientY - bounds.top;
    const left = Math.max(0, Math.min(startX, x));
    const top = Math.max(0, Math.min(startY, y));
    const right = Math.min(list.clientWidth, Math.max(startX, x));
    const bottom = Math.min(list.clientHeight, Math.max(startY, y));
    // Position the overlay through CSSOM, without injecting an inline style rule.
    box.style.transform = 'translate(' + left + 'px,' + top + 'px)';
    box.style.width = Math.max(0, right - left) + 'px';
    box.style.height = Math.max(0, bottom - top) + 'px';
    const hits = [];
    for (const row of list.querySelectorAll('.history-entry')) {
      if (right > row.offsetLeft && left < row.offsetLeft + row.offsetWidth &&
          bottom > row.offsetTop && top < row.offsetTop + row.offsetHeight) {
        hits.push(row.dataset.key);
      }
    }
    // Use the drag's starting selection so repeated pointer moves do not toggle rows again.
    const next = additive ? new Set(original) : new Set();
    for (const key of hits) toggleRecord(next, key, true);
    if (next.size !== selection.size || [...next].some(key => !selection.has(key))) {
      selection.clear();
      for (const key of next) selection.add(key);
      updateSelection();
    }
  }

  function autoScroll() {
    const y = drag.clientY;
    const edge = 45;
    if (y < edge || y > window.innerHeight - edge) {
      const delta = y < edge ? y - edge : y - (window.innerHeight - edge);
      window.scrollBy(0, Math.sign(delta) * Math.min(20, Math.ceil(Math.abs(delta) / 3)));
    }
    draw();
    frame = requestAnimationFrame(autoScroll);
  }

  function stop(restore = false) {
    if (!drag) return;
    const {id, box, original} = drag;
    drag = undefined;
    cancelAnimationFrame(frame);
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', finish);
    window.removeEventListener('pointercancel', cancel);
    window.removeEventListener('blur', cancel);
    window.removeEventListener('keydown', escape);
    if (!box) return;
    box.remove();
    list.classList.remove('is-selecting');
    if (list.hasPointerCapture(id)) list.releasePointerCapture(id);
    if (restore) {
      selection.clear();
      for (const key of original) selection.add(key);
      updateSelection();
    }
  }

  function move(event) {
    if (event.pointerId !== drag?.id) return;
    drag.clientX = event.clientX;
    drag.clientY = event.clientY;
    if (!drag.box) {
      if (Math.hypot(event.clientX - drag.originX, event.clientY - drag.originY) < 5) return;
      drag.box = document.createElement('div');
      drag.box.className = 'record-selection-box';
      drag.box.setAttribute('aria-hidden', 'true');
      list.append(drag.box);
      list.classList.add('is-selecting');
      list.setPointerCapture(drag.id);
      frame = requestAnimationFrame(autoScroll);
    }
    event.preventDefault();
    draw();
  }

  function finish(event) {
    if (event.pointerId !== drag?.id) return;
    if (drag.box) {
      drag.clientX = event.clientX;
      drag.clientY = event.clientY;
      draw();
    }
    stop();
  }

  function cancel() { stop(true); }
  function escape(event) {
    if (event.key === 'Escape') cancel();
  }

  list.addEventListener('pointerdown', event => {
    if (event.button !== 0 || event.pointerType === 'touch' || !enabled() || drag || event.target.closest('input,button')) return;
    const bounds = list.getBoundingClientRect();
    const original = new Set(selection);
    drag = {
      id: event.pointerId, originX: event.clientX, originY: event.clientY,
      startX: event.clientX - bounds.left, startY: event.clientY - bounds.top,
      original, additive: event.ctrlKey || event.metaKey,
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', finish);
    window.addEventListener('pointercancel', cancel);
    window.addEventListener('blur', cancel);
    window.addEventListener('keydown', escape);
  });
  return stop;
}

function setupRecordPanel(name) {
  const isShares = name === 'shares';
  const panel = $(name);
  panel.append($('record-list-template').content.cloneNode(true));
  const field = name => panel.querySelector('[data-' + name + ']');
  const destroyLabel = isShares ? '取消分享并删除源文件' : '移除本地记录';
  field('list-title').textContent = isShares ? '我的分享' : '本地历史';
  field('date-heading').textContent = isShares ? '分享时间' : '记录时间';
  field('destroy').textContent = destroyLabel;
  field('export').hidden = !isShares;

  let records = [];
  const selection = new Set();
  let loadingVersion = 0;
  let changing = false;
  const key = row => isShares ? row.key : row.source;
  const address = row => row.link || (isShares ? '' : row.source);
  const canDestroy = row => isShares ? Boolean(row.id) : row.status === 'reused';
  const status = message => { field('status').textContent = message; };
  const selected = () => records.filter(row => selection.has(key(row)));

  function updateSelection() {
    const items = selected();
    const all = field('select-all');
    all.checked = items.length > 0 && items.length === records.length;
    all.indeterminate = items.length > 0 && !all.checked;
    all.disabled = changing || !records.length;
    field('columns').hidden = !records.length;
    field('batch').hidden = !items.length;
    field('copy').disabled = changing || !items.some(address);
    field('destroy').disabled = changing || items.some(row => !canDestroy(row));
    field('export').disabled = changing;
    field('refresh').disabled = changing;
    field('selection').textContent = items.length ? '已选中 ' + items.length + ' 项' : '';
    field('count').textContent = records.length + ' 项';
    const list = field('list');
    list.classList.toggle('has-selection', items.length > 0);
    for (const item of list.querySelectorAll('.history-entry')) {
      const checked = selection.has(item.dataset.key);
      item.classList.toggle('is-selected', checked);
      const checkbox = item.querySelector('.history-checkbox');
      checkbox.checked = checked;
      checkbox.disabled = changing;
    }
  }

  const stopSelection = enableRectangleSelection(field('list'), selection, updateSelection, () => !changing && records.length > 0);

  async function copy(items) {
    const links = items.map(address).filter(Boolean);
    if (!links.length) return;
    await copyText(links.join('\n'));
    status('已复制 ' + links.length + ' 条链接');
  }

  async function destroy(items) {
    if (changing || !items.length || items.some(row => !canDestroy(row))) return;
    const prompt = isShares
      ? '取消这 ' + items.length + ' 条分享并删除源文件？原链接将失效。你自己网盘中的源文件或文件夹会移入回收站，同一文件的其他分享也可能受影响。'
      : '移除 ' + items.length + ' 条本地记录？夸克网盘里的文件和分享链接会保留。';
    if (!window.confirm(prompt)) return;
    changing = true;
    loadingVersion++;
    updateSelection();
    const deleted = new Set(items.map(key));
    let deleteError;
    try {
      if (isShares) {
        const outcome = await api('shares/cancel-and-delete', {ids: items.map(row => row.id)});
        deleteError = outcome.delete_error;
      } else {
        await api('history/remove', {sources: [...deleted]});
        for (const source of rows.keys()) {
          if (deleted.has(source.split('?')[0])) rows.delete(source);
        }
        render();
      }
      records = records.filter(row => !deleted.has(key(row)));
      for (const id of deleted) selection.delete(id);
      renderList();
      status(isShares
        ? '已取消 ' + deleted.size + ' 条分享。' + (deleteError || '源文件已移入回收站。')
        : '已移除 ' + deleted.size + ' 条本地记录');
    } finally {
      changing = false;
      updateSelection();
    }
  }

  function recordRow(row) {
    const item = document.createElement('div');
    item.className = 'history-grid history-entry';
    item.dataset.key = key(row);
    item.addEventListener('click', event => {
      if (changing || event.target.closest('input,button')) return;
      toggleRecord(selection, key(row), event.ctrlKey || event.metaKey);
      updateSelection();
    });
    const check = document.createElement('input');
    check.type = 'checkbox';
    check.className = 'history-checkbox';
    const title = row.title || '未记录标题';
    const target = address(row);
    check.setAttribute('aria-label', '选择 ' + title);
    check.onchange = () => {
      if (check.checked) selection.add(key(row));
      else selection.delete(key(row));
      updateSelection();
    };

    const file = document.createElement('div');
    file.className = 'history-file';
    const icon = document.createElement('span');
    icon.className = 'history-file-icon';
    icon.setAttribute('aria-hidden', 'true');
    const text = document.createElement('div');
    text.className = 'history-file-text';
    const heading = document.createElement('strong');
    heading.textContent = title;
    heading.draggable = false;
    if (target) heading.title = target;
    text.append(heading, detail(row.message || target || '链接暂不可用'));
    file.append(icon, text);

    const date = document.createElement('time');
    date.className = 'history-date';
    const when = recordDate(row.recorded_at);
    if (when) date.dateTime = new Date(row.recorded_at * 1000).toISOString();
    date.textContent = when || '—';
    const state = document.createElement('span');
    state.className = 'history-state';
    state.textContent = row.status === 'reused' ? '已完成' : labels[row.status];

    const options = document.createElement('div');
    options.className = 'record-actions';
    if (target) {
      const label = row.link ? '复制链接' : '复制原链接';
      options.append(recordIconButton(action(label, () => copy([row]), status), 'copy', label));
    }
    if (canDestroy(row)) {
      options.append(recordIconButton(action(destroyLabel, () => destroy([row]), status), isShares ? 'unlink' : 'remove', destroyLabel));
    }
    item.append(check, file, options, date, state);
    return item;
  }

  function renderList() {
    stopSelection();
    field('list').replaceChildren(...records.map(recordRow));
    updateSelection();
  }

  async function load(refresh = false) {
    if (changing) return;
    const current = ++loadingVersion;
    status('正在读取…');
    try {
      const list = await api(name + (isShares && refresh ? '?refresh=1' : ''));
      if (current !== loadingVersion) return;
      records = list;
      selection.clear();
      renderList();
      status('');
    } catch (err) {
      if (current === loadingVersion) status(err.message);
    }
  }

  field('select-all').onchange = event => {
    selection.clear();
    if (event.currentTarget.checked) {
      for (const row of records) selection.add(key(row));
    }
    updateSelection();
  };
  field('copy').onclick = () => copy(selected()).catch(err => status(err.message));
  field('destroy').onclick = () => destroy(selected()).catch(err => status(err.message));
  field('refresh').onclick = () => load(true);
  field('export').onclick = () => exportShares(selected());
  return {load, clear() {
    loadingVersion++;
    records = [];
    selection.clear();
    renderList();
    status('');
  }};
}

const recordPanels = {history: setupRecordPanel('history'), shares: setupRecordPanel('shares')};

function resultRow(row, controls) {
  const el = document.createElement('div');
  el.className = 'row';
  const main = document.createElement('div');
  main.className = 'row-main';
  const badge = document.createElement('span');
  badge.className = 'badge';
  badge.textContent = labels[row.status] || '网盘分享';
  main.append(link(row.link || row.source), badge);
  el.append(main);
  if (row.title) el.append(detail(row.title));
  if (row.message) el.append(detail(row.message));
  if (row.size_bytes !== undefined && row.limit_bytes) {
    el.append(detail((row.size_reported ? '夸克汇总 ' : '已核算 ') + fileSize(row.size_bytes) + ' · 上限 ' + fileSize(row.limit_bytes)));
  }
  if (controls && !row.link) {
    const actions = document.createElement('div');
    actions.className = 'row-control';
    actions.append(action('复制原链接', () => copyText(row.source)));
    if (!running && row.candidates?.length) {
      for (const candidate of row.candidates) {
        const reuse = action('复用 ' + (candidate.title || '旧分享'), async () => {
          const accepted = await api('confirm', {source: row.source, link: candidate.link});
          rows.set(row.source, accepted);
          render();
        });
        actions.append(reuse, link(candidate.link));
      }
      actions.append(action('继续转存', () => convert(row.source, true, false)));
    }
    el.append(actions);
  }
  return el;
}

function render() {
  const filter = $('filter').value;
  const list = [...rows.values()];
  const filtered = list.filter(row => !filter || (filter === 'success' ? row.link : row.status === filter));
  $('results').replaceChildren(...filtered.map(row => resultRow(row, true)));
  const success = list.filter(row => row.link).length;
  $('summary').textContent = list.length
    ? '已处理 ' + list.length + ' 条 · ' + success + ' 条可复制' + (running ? ' · 转换中' : '')
    : (running ? '正在检查分享…' : '转换后的链接会出现在这里');
  $('copy').disabled = $('download').disabled = !success;
}

async function convert(text, fresh, reset, retest = false, attach = false) {
  if (running) return;
  alertWork();
  if (reset) rows.clear();
  running = true;
  $('convert').disabled = true;
  render();
  try {
    const response = await fetch('/api/v1/convert?stream=1', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({text, fresh, retest, attach}),
    });
    if (!response.ok) throw new Error((await response.json()).error || '转换请求失败');
    let buffer = '';
    function append(line) {
      if (!line.trim()) return;
      const item = JSON.parse(line);
      if (item.error) throw new Error(item.error);
      rows.set(item.source, item);
    }
    for await (const part of response.body.pipeThrough(new TextDecoderStream())) {
      const lines = (buffer + part).split('\n');
      buffer = lines.pop();
      for (const line of lines) append(line);
      render();
    }
    append(buffer);
  } catch (err) { alertWork(err.message); }
  finally {
    running = false;
    $('convert').disabled = false;
    render();
  }
}

async function loadConfig() {
  const config = await api('settings');
  loggedIn = config.logged_in;
  currentLimit = config.limit;
  $('account-name').textContent = loggedIn ? config.nickname || '夸克已连接' : '连接夸克网盘';
  $('account-avatar').hidden = !loggedIn;
  $('account-avatar-fallback').textContent = Array.from(config.nickname?.trim() || '夸')[0];
  const avatar = $('account-avatar-image');
  const avatarURL = loggedIn ? config.avatar_url : '';
  if (avatarURL && avatar.getAttribute('src') !== avatarURL) {
    avatar.hidden = true;
    avatar.onload = () => { avatar.hidden = false; };
    avatar.onerror = () => { avatar.hidden = true; };
    avatar.src = avatarURL;
  } else if (!avatarURL) {
    avatar.hidden = true;
    avatar.removeAttribute('src');
  }
  $('limit-label').textContent = '上限 ' + currentLimit;
  $('logout').hidden = !loggedIn;
}

async function importFile(file) {
  if (!file) return;
  if (!file.name.toLowerCase().endsWith('.txt')) { alertWork('请选择 TXT 文件'); return; }
  if (file.size > 2 * 1024 ** 2) { alertWork('文件不能超过 2 MiB'); return; }
  $('input').value = await file.text();
  alertWork();
}

$('convert-form').addEventListener('submit', event => {
  event.preventDefault();
  if (!loggedIn) { $('account').click(); return; }
  const retest = $('retest').checked;
  $('retest').checked = false;
  convert($('input').value, false, true, retest, $('attach').checked);
});
$('clear').onclick = () => { $('input').value = ''; $('input').focus(); };
$('preview').onclick = async () => {
  try {
    const items = await api('preview', {text: $('input').value});
    const list = $('preview-list');
    list.replaceChildren();
    for (const address of items) {
      const row = document.createElement('p');
      row.textContent = address;
      list.append(row);
    }
    if (!items.length) list.textContent = '没有识别到夸克分享链接';
    $('preview-dialog').showModal();
  } catch (err) { alertWork(err.message); }
};
$('file').onchange = event => {
  importFile(event.currentTarget.files[0]).catch(err => alertWork(err.message));
  event.currentTarget.value = '';
};
$('convert-form').ondragover = event => {
  if (event.dataTransfer.types.includes('Files')) {
    event.preventDefault();
    $('convert-form').classList.add('drag');
  }
};
$('convert-form').ondragleave = () => $('convert-form').classList.remove('drag');
$('convert-form').ondrop = event => {
  if (!event.dataTransfer.files.length) return;
  event.preventDefault();
  $('convert-form').classList.remove('drag');
  importFile(event.dataTransfer.files[0]).catch(err => alertWork(err.message));
};
$('filter').onchange = render;
$('copy').onclick = async () => {
  try {
    await copyText(acceptedLinks());
    $('summary').textContent = '链接已复制';
  } catch (err) { alertWork(err.message); }
};
$('download').onclick = () => {
  const href = URL.createObjectURL(new Blob([acceptedLinks() + '\n'], {type: 'text/plain;charset=utf-8'}));
  const a = document.createElement('a');
  a.href = href;
  a.download = 'quark-links.txt';
  a.click();
  setTimeout(() => URL.revokeObjectURL(href), 1000);
};

let updatesPage = 1;
let selectedUpdate;
let updatePage = 1;
let updateDir = '';
let updateDialogBusy = false;
const showUpdateError = message => { $('updates-status').textContent = message; };
const showUpdateDetail = message => { $('update-status').textContent = message; };

async function loadUpdates(page) {
  showUpdateError('正在查询夸克的转存更新…');
  const data = await api('updates/list', {page});
  updatesPage = page;
  $('updates-prev').hidden = page <= 1;
  $('updates-next').hidden = !data.has_next_page;
  $('updates-list').replaceChildren(...data.items.map(item => {
    const row = document.createElement('div');
    row.className = 'row';
    const header = document.createElement('div');
    header.className = 'panel-head';
    const title = link(item.share_url);
    title.textContent = item.title || item.share_url;
    header.append(title, action('查看新增', async () => {
      selectedUpdate = item;
      $('update-title').textContent = item.title || '查看新增文件';
      $('update-dialog').showModal();
      await runUpdateDialog(() => loadUpdateFiles(1));
    }, showUpdateDetail));
    row.append(header, detail(item.updated_at));
    return row;
  }));
  showUpdateError(data.items.length ? '第 ' + page + ' 页' : '夸克没有返回待更新的转存分享。');
}

async function loadUpdateFiles(page) {
  updateDir = '';
  $('update-apply').hidden = true;
  $('update-next').hidden = true;
  showUpdateDetail('正在读取新增文件和上次转存目录…');
  if (page === 1) $('update-files').replaceChildren();
  const data = await api('updates/preview', {url: selectedUpdate.share_url, page});
  updatePage = page;
  updateDir = data.pdir_fid;
  for (const item of data.files || []) {
    const row = document.createElement('li');
    row.textContent = [item.title, item.file_type, item.size > 0 ? fileSize(item.size) : ''].filter(Boolean).join(' · ');
    $('update-files').append(row);
  }
  $('update-next').hidden = !data.has_next_page;
  $('update-apply').hidden = !data.total || !updateDir;
  showUpdateDetail(data.total ? '新增 ' + data.total + ' 项 · 转存到「' + (data.pdir_name || '上次转存目录') + '」' : '没有待转存的新增文件。');
}

async function runUpdateDialog(handler) {
  updateDialogBusy = true;
  const close = $('update-dialog').querySelector('.close');
  close.disabled = true;
  try { await handler(); }
  finally {
    close.disabled = false;
    updateDialogBusy = false;
  }
}

$('update-dialog').addEventListener('cancel', event => {
  if (updateDialogBusy) event.preventDefault();
});

function bindUpdateAction(id, handler, onError) {
  $(id).onclick = async () => {
    $(id).disabled = true;
    try { await handler(); } catch (err) { onError(err.message); }
    finally { $(id).disabled = false; }
  };
}
bindUpdateAction('updates-check', () => loadUpdates(1), showUpdateError);
bindUpdateAction('updates-prev', () => loadUpdates(updatesPage - 1), showUpdateError);
bindUpdateAction('updates-next', () => loadUpdates(updatesPage + 1), showUpdateError);
bindUpdateAction('update-next', () => runUpdateDialog(() => loadUpdateFiles(updatePage + 1)), showUpdateDetail);
bindUpdateAction('update-apply', () => runUpdateDialog(async () => {
  $('update-apply').hidden = true;
  $('update-next').hidden = true;
  showUpdateDetail('正在转存新增文件…');
  const data = await api('updates/apply', {url: selectedUpdate.share_url, dir_id: updateDir});
  showUpdateDetail(data.message || '没有待转存的新增文件。');
}), showUpdateDetail);

async function selectView(view) {
  for (const name of ['work', 'history', 'shares', 'updates']) {
    $(name).hidden = name !== view;
  }
  document.querySelectorAll('.tab').forEach(tab => tab.classList.toggle('selected', tab.dataset.view === view));
  if (view === 'work') return;
  if (view === 'updates') {
    try {
      const status = await api('updates/status');
      $('updates-check').disabled = !status.configured;
      if (!status.configured) showUpdateError('启动时用 --quark-cli 指定夸克官方 CLI，即可检查转存更新。');
    } catch (err) { showUpdateError(err.message); }
    return;
  }
  return recordPanels[view].load();
}

document.querySelectorAll('[data-view]').forEach(button => button.onclick = () => selectView(button.dataset.view));
document.querySelectorAll('.close').forEach(button => button.onclick = () => button.closest('dialog').close());
$('account').onclick = () => {
  $('login-status').textContent = loggedIn ? '当前会话已保存在本机。' : '使用夸克 App 扫码，或导入已登录的网页会话。';
  $('account-dialog').showModal();
};
$('account-dialog').addEventListener('close', () => {
  clearTimeout(qrTimer);
  qrRun++;
  $('qr').hidden = true;
  $('cookie').value = '';
});
$('settings').onclick = () => {
  $('limit').value = currentLimit;
  $('settings-status').hidden = true;
  $('settings-dialog').showModal();
};
$('settings-form').onsubmit = async event => {
  event.preventDefault();
  try { await api('settings', {limit: $('limit').value}); await loadConfig(); $('settings-dialog').close(); }
  catch (err) { $('settings-status').hidden = false; $('settings-status').textContent = err.message; }
};
$('login-form').onsubmit = async event => {
  event.preventDefault();
  const cookie = $('cookie').value;
  $('cookie').value = '';
  try {
    $('login-status').textContent = '正在登录并同步我的分享…';
    const state = await api('login', {cookie});
    await finishLogin(state);
  }
  catch (err) { $('login-status').textContent = err.message; }
};
async function finishLogin(state) {
  await loadConfig();
  $('account-dialog').close();
  if (state.share_sync_error) alertWork('已登录，同步我的分享失败。' + state.share_sync_error);
}
$('logout').onclick = async () => {
  try {
    await api('logout', {});
    await loadConfig();
    rows.clear();
    recordPanels.history.clear();
    recordPanels.shares.clear();
    render();
    selectView('work');
    $('account-dialog').close();
  } catch (err) { $('login-status').textContent = err.message; }
};
$('start-qr').onclick = async () => {
  clearTimeout(qrTimer);
  const generation = ++qrRun;
  try {
    const response = await api('login/qr', {});
    if (generation !== qrRun) return;
    $('qr').src = response.image;
    $('qr').hidden = false;
    $('login-status').textContent = '请打开夸克 App 扫码并确认登录。';
    const check = async () => {
      try {
        const state = await api('login/qr/check', {});
        if (generation !== qrRun) return;
        if (state.logged_in) await finishLogin(state);
        else qrTimer = setTimeout(check, 2200);
      } catch (err) {
        if (generation !== qrRun) return;
        $('login-status').textContent = err.message;
        $('qr').hidden = true;
      }
    };
    qrTimer = setTimeout(check, 2200);
  } catch (err) { $('login-status').textContent = err.message; }
};

loadConfig().catch(err => alertWork(err.message));

