import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, open, rm, writeFile, readFile, unlink, realpath } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { once } from 'node:events';

const fixture = await mkdtemp(path.join(tmpdir(), 'disklens-ui-'));
const dataDirectory = await mkdtemp(path.join(tmpdir(), 'disklens-ui-data-'));
const secondRoot = await mkdtemp(path.join(tmpdir(), 'disklens-ui-second-'));
const output = path.resolve('.build/screenshots');
await mkdir(output, { recursive: true });
for (const [name, size] of [
  ['系统镜像/ubuntu-server.iso', 256 * 1024 ** 2],
  ['系统镜像/debian-12.iso', 192 * 1024 ** 2],
  ['家庭照片/2026/旅行.jpg', 8 * 1024 ** 2],
  ['项目文档/开发说明.pdf', 3 * 1024 ** 2],
  ['备份归档/backup.zip', 64 * 1024 ** 2],
  ['日志/service.log', 512 * 1024],
]) {
  const file = path.join(fixture, name);
  await mkdir(path.dirname(file), { recursive: true });
  const handle = await open(file, 'w');
  await handle.truncate(size);
  await handle.close();
}
const launchServer = (port = '0') => spawn(path.resolve(process.env.DISKLENS_TEST_BIN || '.build/disklens-preview'), {
  env: { ...process.env, TRIM_APPNAME: '', DISKLENS_SOCKET: '', DISKLENS_ROOTS: fixture + ',' + secondRoot, DISKLENS_PORT: port, DISKLENS_DATA_DIR: dataDirectory },
  stdio: ['ignore','pipe','pipe'],
});
let server = launchServer();
let browser;
try {
  const url = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('preview did not start')), 10000);
    server.stderr.on('data', chunk => {
      const match = chunk.toString().match(/listening on (127\.0\.0\.1:\d+)/);
      if (match) { clearTimeout(timeout); resolve('http://' + match[1] + '/'); }
    });
    server.once('exit', code => { clearTimeout(timeout); reject(new Error('preview exited: ' + code)); });
  });
  browser = await chromium.launch({ executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true, args: ['--disable-gpu'] });
  const page = await browser.newPage({ viewport: { width: 1100, height: 700 }, deviceScaleFactor: 1 });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('response', response => { if (response.status() >= 400) errors.push(response.url() + ': ' + response.status()); });
  await page.goto(url);
  await page.waitForFunction(() => !document.querySelector('#scan').disabled);
  assert.equal(await page.locator('.brand').isVisible(), true, 'standalone application identity missing');
  const shellPage = await browser.newPage({ viewport: { width: 1100, height: 800 } });
  await shellPage.route('**/native-shell-test', route => route.fulfill({ contentType: 'text/html; charset=utf-8', body: '<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>磁盘瞭望</title><body style="margin:0"><header style="height:42px;padding:12px;box-sizing:border-box;font:14px sans-serif">磁盘瞭望</header><iframe title="DiskLens" src="' + url + '" style="display:block;width:100%;height:750px;border:0"></iframe></body></html>' }));
  await shellPage.goto(url + 'native-shell-test');
  const embedded = shellPage.frameLocator('iframe');
  await embedded.locator('#connection').getByText('已连接', { exact: true }).waitFor();
  assert.equal(await embedded.locator('.brand').isVisible(), false, 'native shell duplicated application title');
  assert.equal(await embedded.locator('[data-view="overview"]').isVisible(), true);
  await shellPage.screenshot({ path: path.join(output, 'embedded-desktop.png') });
  await shellPage.close();
  await page.screenshot({ path: path.join(output, 'empty-desktop.png'), fullPage: true });
  await page.click('#scan');
  await page.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  assert.equal(await page.textContent('#fileCount'), '6');
  assert.match(await page.textContent('#overview-files'), /ubuntu-server.iso/);
  assert.equal(await page.locator('.view:visible').count(), 1);
  await page.screenshot({ path: path.join(output, 'overview-desktop.png'), fullPage: true });
  for (const [name, title] of Object.entries({ directories: '目录分析', types: '文件类型', files: '大文件', overview: '概览' })) {
    await page.click('[data-view="' + name + '"]');
    assert.equal(await page.textContent('#viewTitle'), title);
    assert.equal(await page.locator('.view:visible').count(), 1);
  }
  await page.click('[data-view="files"]');
  await page.fill('#fileSearch', 'ubuntu');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 1);
  assert.equal(await page.locator('#files tbody tr').count(), 1);
  await page.fill('#fileSearch', 'does-not-exist');
  await page.waitForFunction(() => document.querySelector('#files').textContent.includes('没有匹配'));
  assert.match(await page.textContent('#files'), /没有匹配/);
  await page.fill('#fileSearch', '');
  await page.click('[data-view="overview"]');
  for (const width of [1100, 800, 620, 375]) {
    await page.setViewportSize({ width, height: 700 });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
    assert.equal(overflow, false, 'horizontal overflow at ' + width);
    const invalid = await page.locator('button:visible').evaluateAll(items => items.filter(item => item.scrollWidth > item.clientWidth + 1).map(item => item.textContent));
    assert.deepEqual(invalid, [], 'button overflow at ' + width);
  }
  const mobile = await browser.newPage({ viewport: { width: 375, height: 700 }, deviceScaleFactor: 1 });
  await mobile.goto(url);
  await mobile.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  await mobile.screenshot({ path: path.join(output, 'overview-mobile.png') });
  await mobile.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await mobile.screenshot({ path: path.join(output, 'overview-mobile-bottom.png') });
  await mobile.close();
  await page.setViewportSize({ width: 1100, height: 700 });

  // Query beyond the old Top 50 and exercise multi-root scope/exclusions.
  for (let i = 1; i <= 60; i++) {
    const file = path.join(fixture, 'tiny', String(i).padStart(3, '0') + '.txt');
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, Buffer.alloc(i));
  }
  await writeFile(path.join(fixture, 'duplicate-a.bin'), 'same bytes');
  await writeFile(path.join(fixture, 'duplicate-b.bin'), 'same bytes');
  await writeFile(path.join(fixture, 'excluded.tmp'), 'excluded');
  await writeFile(path.join(secondRoot, 'other.txt'), 'another root');
  await page.click('#scope');
  await page.locator('[name="scanRoot"]').nth(1).check();
  await page.fill('#excludes', '@recycle\n.snapshot\n*.tmp');
  await page.click('#scopeForm [type="submit"]');
  await page.click('#scan');
  await page.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  assert.equal(await page.textContent('#fileCount'), '69');
  await page.click('[data-view="directories"]');
  await page.waitForFunction(() => document.querySelectorAll('#dirs tbody tr').length === 2);
  await page.locator('#dirs [data-directory]').filter({ hasText: path.basename(fixture) }).click();
  await page.locator('#dirs [data-directory]').filter({ hasText: '家庭照片' }).click();
  await page.locator('#dirs [data-directory]').filter({ hasText: '2026' }).click();
  await page.waitForFunction(() => document.querySelector('#dirs').textContent.includes('旅行.jpg'));
  assert.match(await page.textContent('#breadcrumbs'), /家庭照片.*2026/);
  await page.click('#dirUp');
  await page.waitForFunction(() => document.querySelectorAll('#dirs [data-directory]').length === 1);
  await page.click('[data-view="files"]');
  await page.fill('#fileSearch', '001.txt');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 1 && document.querySelector('#files').textContent.includes('001.txt'));
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.click('#files [data-copy]');
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), await realpath(path.join(fixture, 'tiny/001.txt')));
  await page.fill('#fileSearch', '');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 50);
  await page.click('#filePager [data-next]');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 19);
  await page.selectOption('#fileCategory', '镜像');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 2);
  await page.click('#advancedToggle');
  await page.fill('[name="min"]', '200');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 1);
  const downloadPromise = page.waitForEvent('download');
  await page.click('#export');
  const download = await downloadPromise;
  const csv = await readFile(await download.path(), 'utf8');
  assert.match(csv, /ubuntu-server.iso/);
  assert.doesNotMatch(csv, /debian-12.iso/);
  await page.click('#filters [type="reset"]');
  await page.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 50);
  await page.click('#advancedToggle');
  await page.screenshot({ path: path.join(output, 'files-desktop.png'), fullPage: true });

  // Compare matching multi-root snapshots, then restore an older record.
  await writeFile(path.join(fixture, 'tiny/001.txt'), 'changed content');
  await unlink(path.join(fixture, 'tiny/002.txt'));
  await writeFile(path.join(fixture, 'added.txt'), 'new file added');
  await page.click('#scan');
  await page.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  await page.click('[data-view="history"]');
  await page.waitForFunction(() => document.querySelectorAll('#history tbody tr').length === 3);
  await page.click('#compareButton');
  await page.waitForFunction(() => document.querySelector('#compareSummary').textContent.includes('新增 1'));
  assert.match(await page.textContent('#compareSummary'), /消失 1.*变化 1/);
  await page.screenshot({ path: path.join(output, 'history-desktop.png'), fullPage: true });
  await page.locator('[data-history]').last().click();
  assert.equal(await page.textContent('#fileCount'), '6');
  await page.click('[data-view="history"]');
  await page.locator('[data-history]').first().click();
  assert.equal(await page.textContent('#fileCount'), '69');

  // Full-content confirmation with actual small files, not a mocked result.
  await page.click('[data-view="duplicates"]');
  await page.fill('#duplicateMin', '0');
  await page.click('#duplicateStart');
  await page.waitForFunction(() => document.querySelector('#duplicateStatus').textContent.startsWith('检测完成'));
  assert.match(await page.textContent('#duplicateSummary'), /1 组/);
  assert.match(await page.textContent('#duplicates'), /duplicate-a.bin/);
  await page.click('[data-group]');
  await page.waitForFunction(() => document.querySelector('#pathsDialog').open);
  assert.match(await page.textContent('#duplicatePaths'), /duplicate-b.bin/);
  await page.click('[data-close="pathsDialog"]');
  await page.screenshot({ path: path.join(output, 'duplicates-desktop.png'), fullPage: true });
  await page.reload();
  await page.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  assert.equal(await page.textContent('#fileCount'), '69');
  assert.equal(await page.textContent('#scopeLabel'), '2 个目录');
  for (const width of [1100, 800, 620, 375]) {
    await page.setViewportSize({ width, height: 800 });
    for (const view of Object.keys({ overview: 1, directories: 1, types: 1, files: 1, duplicates: 1, history: 1 })) {
      await page.click('[data-view="' + view + '"]');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth), false, view + ': overflow at ' + width);
      const invalid = await page.locator('button:visible').evaluateAll(items => items.filter(item => item.scrollWidth > item.clientWidth + 1).map(item => item.textContent));
      assert.deepEqual(invalid, [], view + ': button overflow at ' + width);
    }
  }
  const mobileFiles = await browser.newPage({ viewport: { width: 375, height: 800 }, deviceScaleFactor: 1 });
  await mobileFiles.goto(url);
  await mobileFiles.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  await mobileFiles.click('[data-view="files"]');
  await mobileFiles.waitForFunction(() => document.querySelectorAll('#files tbody tr').length === 50);
  await mobileFiles.screenshot({ path: path.join(output, 'files-mobile.png') });
  await mobileFiles.close();

  // Restart the real process with the same database, not just the browser.
  const previous = await (await fetch(url + 'api/config')).json();
  server.kill('SIGTERM'); await once(server, 'exit');
  server = launchServer(new URL(url).port);
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('restart did not complete')), 10000);
    server.stderr.on('data', chunk => { if (chunk.toString().includes('listening on')) { clearTimeout(timeout); resolve(); } });
    server.once('exit', code => { clearTimeout(timeout); reject(new Error('restart exited: ' + code)); });
  });
  const restored = await (await fetch(url + 'api/config')).json();
  assert.equal(restored.lastScan.id, previous.lastScan.id);
  assert.equal(restored.lastScan.fileCount, 69);
  assert.equal((await (await fetch(url + 'api/history')).json()).length, 3);
  await page.reload();
  await page.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  assert.equal(await page.textContent('#fileCount'), '69');

  // Space ledger stays separate from retained file indexes; preferences persist.
  await page.setViewportSize({ width: 1100, height: 800 });
  await page.click('[data-view="ledger"]');
  await page.waitForFunction(() => document.querySelector('#ledgerCoverage').textContent.includes('基准'));
  assert.match(await page.textContent('#ledgerChanges'), /tiny|根目录文件/);
  assert.match(await page.textContent('#ledgerCategories'), /文档/);
  assert.equal(await page.locator('#ledgerTrend .trend-column').count(), 2);
  await page.screenshot({ path: path.join(output, 'ledger-desktop.png'), fullPage: true });
  await page.click('[data-view="settings"]');
  await page.waitForFunction(() => document.querySelector('#scheduleRoots input'));
  assert.equal(await page.locator('#scheduleEnabled').isChecked(), false);
  await page.selectOption('#scanRate', '128');
  await page.fill('#scanTimeout', '120');
  await page.fill('#growthGB', '0.000001');
  await page.fill('#growthPercent', '0');
  await page.click('#saveInspection');
  await page.waitForFunction(() => document.querySelector('#settingsMessage').textContent === '设置已保存');
  const inspection = await (await fetch(url + 'api/inspection')).json();
  assert.equal(inspection.settings.entriesPerSecond, 128);
  assert.equal(inspection.settings.timeoutMinutes, 120);
  assert.equal(inspection.settings.enabled, false);
  await page.screenshot({ path: path.join(output, 'settings-desktop.png'), fullPage: true });
  await page.click('[data-view="overview"]');
  await writeFile(path.join(fixture, 'growth-notification.txt'), Buffer.alloc(8192, 1));
  await page.click('#scan');
  await page.waitForFunction(() => document.querySelector('#status').textContent === '扫描完成');
  assert.equal(await page.textContent('#fileCount'), '70');
  await page.click('[data-view="alerts"]');
  await page.waitForFunction(() => document.querySelector('#alerts [data-alert]'));
  assert.match(await page.textContent('#alerts'), /逻辑大小增加/);
  await page.screenshot({ path: path.join(output, 'alerts-desktop.png'), fullPage: true });
  await page.click('#alerts [data-alert]');
  await page.waitForFunction(() => document.querySelector('#alertSummary').textContent === '0 条未确认');
  for (const width of [1100, 800, 620, 375]) {
    await page.setViewportSize({ width, height: 800 });
    for (const view of ['ledger', 'alerts', 'settings']) {
      await page.click('[data-view="' + view + '"]');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth), false, view + ': overflow at ' + width);
      const invalid = await page.locator('button:visible').evaluateAll(items => items.filter(item => item.scrollWidth > item.clientWidth + 1).map(item => item.textContent));
      assert.deepEqual(invalid, [], view + ': button overflow at ' + width);
      if (width === 375) await page.screenshot({ path: path.join(output, view + '-mobile.png'), fullPage: true });
    }
  }

  // Reproduce authorization added after the initial empty response.
  const auth = await browser.newPage();
  let authorized = false;
  await auth.route('**/api/config', route => route.fulfill({ json: { allowedRoots: authorized ? [fixture] : [], lastScan: null, version: '0.2.0' } }));
  await auth.route('**/api/scan', route => route.fulfill({ json: null }));
  await auth.route('**/api/duplicates?*', route => route.fulfill({ json: null }));
  await auth.goto(url);
  await auth.waitForFunction(() => document.querySelector('#status').textContent === '等待授权');
  assert.equal(await auth.locator('#scan').isDisabled(), true);
  authorized = true;
  await auth.click('#refresh');
  await auth.waitForFunction(() => !document.querySelector('#scan').disabled);

  // Exercise progress and cancellation UI without depending on disk speed.
  let phase = 'running';
  await auth.unroute('**/api/scan');
  await auth.route('**/api/scan', async route => {
    if (route.request().method() === 'DELETE') phase = 'cancelled';
    await route.fulfill({ json: { id: 'test', root: fixture, state: phase, progress: { totalBytes: 2048, fileCount: 10, dirCount: 2, skipped: 0, durationMs: 1200, currentPath: '系统镜像' } } });
  });
  await auth.click('#scan');
  await auth.waitForFunction(() => document.querySelector('#status').textContent === '扫描中');
  assert.match(await auth.textContent('#hint'), /10 个文件/);
  await auth.click('#cancel');
  await auth.waitForFunction(() => document.querySelector('#status').textContent === '已停止');
  assert.equal(await auth.locator('#scan').isDisabled(), false);
  assert.deepEqual(errors, []);
  console.log('PASS: multi-root scan, full index, filters, drilldown, clipboard, CSV, history, duplicate confirmation, authorization, cancellation, persistent inspection settings, ledger, growth alerts and nine responsive views.');
  console.log('Screenshots: ' + output);
} finally {
  if (browser) await browser.close();
  if (server.exitCode === null) { server.kill('SIGTERM'); await once(server, 'exit'); }
  await rm(fixture, { recursive: true, force: true });
  await rm(dataDirectory, { recursive: true, force: true });
  await rm(secondRoot, { recursive: true, force: true });
}
