import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import { createServer, request } from 'node:http';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { once } from 'node:events';
import path from 'node:path';
import assert from 'node:assert/strict';

const fixture = await mkdtemp(path.join(tmpdir(), 'disklens-gateway-'));
const version = (await readFile('fnos/manifest', 'utf8')).match(/^version="([^"]+)"/m)[1];
const socket = path.join(fixture, 'app.sock');
const roots = path.join(fixture, 'files');
await mkdir(roots);
await writeFile(path.join(roots, 'one.txt'), 'first test file');
await writeFile(path.join(roots, 'two.txt'), 'second test file');
const server = spawn(path.resolve(process.env.DISKLENS_TEST_BIN || '.build/disklens-preview'), {
  env: { ...process.env, TRIM_APPNAME: '', DISKLENS_SOCKET: socket, DISKLENS_ROOTS: roots, DISKLENS_DATA_DIR: path.join(fixture, 'data') },
  stdio: ['ignore', 'ignore', 'pipe'],
});
let browser;
let writes = 0;
const proxy = createServer((req, res) => {
  if (req.url === '/') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end('<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>磁盘瞭望</title><body style="margin:0;font:14px sans-serif"><header style="height:40px;box-sizing:border-box;padding:10px">磁盘瞭望</header><iframe title="DiskLens" src="/app/DiskLens/" style="display:block;border:0;width:100%;height:760px"></iframe></body></html>');
    return;
  }
  // Model fnOS Host normalization and an HTTP LAN browser without Fetch Metadata.
  const headers = { ...req.headers, host: new URL('http://' + req.headers.host).hostname, 'x-trim-userid': '1000', 'x-trim-isadmin': 'true' };
  delete headers['sec-fetch-site'];
  if (!['GET', 'HEAD', 'OPTIONS'].includes(req.method)) writes++;
  const upstream = request({ socketPath: socket, path: req.url, method: req.method, headers }, reply => {
    res.writeHead(reply.statusCode, reply.headers);
    reply.pipe(res);
  });
  upstream.on('error', error => { res.writeHead(502); res.end(error.message); });
  req.pipe(upstream);
});
const otherOrigin = createServer((req, res) => { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end('<!doctype html><title>Other origin</title>'); });

try {
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('gateway backend did not start')), 10000);
    server.once('error', error => { clearTimeout(timeout); reject(error); });
    server.once('exit', code => { clearTimeout(timeout); reject(new Error('backend exited: ' + code)); });
    server.stderr.on('data', chunk => { if (chunk.toString().includes('listening on')) { clearTimeout(timeout); resolve(); } });
  });
  proxy.listen(0, '127.0.0.1');
  await once(proxy, 'listening');
  otherOrigin.listen(0, '127.0.0.1');
  await once(otherOrigin, 'listening');
  const url = 'http://127.0.0.1:' + proxy.address().port;
  browser = await chromium.launch({ executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true });
  const page = await browser.newPage({ viewport: { width: 1100, height: 800 } });
  page.setDefaultTimeout(10000);
  await page.goto(url);
  const app = page.frameLocator('iframe');
  await app.locator('#connection').getByText('已连接', { exact: true }).waitFor();
  assert.equal(await app.locator('.brand').isVisible(), false);
  assert.equal(await app.locator('.version').textContent(), 'v' + version);
  await app.locator('#scan').click();
  await app.locator('#status').getByText('扫描完成', { exact: true }).waitFor();
  assert.equal(await app.locator('#fileCount').textContent(), '2');
  assert.ok(writes > 0);
  await mkdir('.build/screenshots', { recursive: true });
  await page.screenshot({ path: '.build/screenshots/gateway-embedded-desktop.png' });
  await app.locator('[data-view="settings"]').click();
  await app.locator('#scanRate').selectOption('2048');
  const saved = page.waitForResponse(response => response.url().endsWith('/api/inspection') && response.request().method() === 'POST');
  await app.locator('#saveInspection').click();
  assert.equal((await saved).status(), 200);
  const frame = page.frames().find(frame => frame.url().endsWith('/app/DiskLens/'));
  const denied = await frame.evaluate(async () => {
    const response = await fetch('./api/scan', { method: 'DELETE' });
    return { status: response.status, body: await response.json() };
  });
  assert.equal(denied.status, 403);
  assert.equal(denied.body.code, 'cross_site_request');
  const cancelled = await frame.evaluate(async () => (await fetch('./api/scan', { method: 'DELETE', headers: { 'X-DiskLens-Request': '1' } })).status);
  assert.equal(cancelled, 200);

  // Even the same hostname on a different port cannot send the write marker.
  const attacker = await browser.newPage();
  await attacker.goto('http://127.0.0.1:' + otherOrigin.address().port);
  const previousWrites = writes;
  const attack = await attacker.evaluate(async target => {
    try {
      await fetch(target + '/app/DiskLens/api/health', { method: 'POST', credentials: 'include', headers: { 'X-DiskLens-Request': '1', 'Content-Type': 'application/json' }, body: '{}' });
      return 'allowed';
    } catch { return 'blocked'; }
  }, url);
  assert.equal(attack, 'blocked');
  assert.equal(writes, previousWrites, 'cross-origin preflight allowed a mutation');

  // A request rejection must not be presented as revoked folder authorization.
  await page.route('**/api/scan', async route => {
    if (route.request().method() === 'POST') await route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ code: 'cross_site_request', error: '不允许跨站操作，请重新打开应用后重试' }) });
    else await route.continue();
  });
  await app.locator('[data-view="overview"]').click();
  await app.locator('#scan').click();
  await app.locator('#status').getByText('请求被拦截', { exact: true }).waitFor();
  assert.equal(await app.locator('#fileCount').textContent(), '2');
  assert.equal(await app.locator('#scan').isDisabled(), false);
  console.log('PASS: fnOS Unix-socket proxy, stripped Host port, absent Fetch Metadata, scan, settings, DELETE, CORS isolation, request-error state and single embedded title.');
} finally {
  if (browser) await browser.close();
  await Promise.all([proxy, otherOrigin].map(server => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); })));
  if (server.exitCode === null) { server.kill('SIGTERM'); await once(server, 'exit'); }
  await rm(fixture, { recursive: true, force: true });
}
