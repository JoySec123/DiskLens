import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import assert from 'node:assert/strict';

const version = readFileSync('fnos/manifest', 'utf8').match(/^version="([^"]+)"/m)[1];
const goVersion = readFileSync('go.mod', 'utf8').match(/^go (\d+\.\d+\.\d+)$/m)[1];
assert.match(readFileSync('cmd/disklens/main.go', 'utf8'), new RegExp('const version = "' + version.replaceAll('.', '\\.') + '"'));

for (const [arch, platform, machine] of [['amd64','x86',62], ['arm64','arm',183]]) {
  const file = 'dist/DiskLens-' + version + '-' + arch + '.fpk';
  const outer = name => execFileSync('tar', ['-xOf', file, name], { maxBuffer: 32 * 1024 ** 2 });
  const app = outer('app.tgz');
  const inner = name => execFileSync('tar', ['-xzOf', '-', name], { input: app, maxBuffer: 32 * 1024 ** 2 });
  const manifest = outer('manifest').toString();
  assert.match(manifest, new RegExp('version\\s*=\\s*' + version.replaceAll('.', '\\.')));
  assert.match(manifest, new RegExp('platform\\s*=\\s*' + platform));
  const ui = JSON.parse(inner('ui/config'));
  const entry = ui['.url']['DiskLens.Application'];
  assert.equal(entry.gatewayPrefix, '/app/DiskLens');
  assert.equal(entry.gatewaySocket, 'app.sock');
  assert.equal(entry.allUsers, false);
  for (const size of [64,256]) {
    const icon = outer(size === 64 ? 'ICON.PNG' : 'ICON_256.PNG');
    assert.equal(icon.readUInt32BE(16), size);
    assert.equal(icon.readUInt32BE(20), size);
    assert.deepEqual(icon, inner('ui/' + entry.icon.replace('{0}', size)));
  }
  const binary = inner('bin/disklens');
  assert.equal(binary.subarray(0,4).toString('hex'), '7f454c46');
  assert.equal(binary.readUInt16LE(18), machine);
  assert.ok(binary.includes(Buffer.from('go' + goVersion)), 'binary was not built with Go ' + goVersion);
  assert.match(outer('cmd/main').toString(), /DISKLENS_SOCKET/);
  assert.match(outer('cmd/main').toString(), /DISKLENS_DATA_DIR/);
  assert.match(outer('cmd/main').toString(), /TRIM_PKGVAR/);
  assert.match(inner('licenses/NOTICES.txt').toString(), /modernc.org\/sqlite/);
  assert.ok(inner('licenses/Go-LICENSE').length > 1000);
  assert.deepEqual(JSON.parse(outer('config/resource'))['api-scope'], ['trim.file.sharedAccess']);
  const sha = createHash('sha256').update(readFileSync(file)).digest('hex');
  console.log(arch + ': version, Go ' + goVersion + ', architecture, gateway, icons, scope, lifecycle OK; sha256 ' + sha);
}
