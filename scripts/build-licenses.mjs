import { execFileSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, readdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';

const output = process.argv[2];
if (!output) throw new Error('Usage: node scripts/build-licenses.mjs OUTPUT [ARCH]');
mkdirSync(output, { recursive: true });
const env = { ...process.env, GOOS: 'linux', GOARCH: process.argv[3] || 'amd64', CGO_ENABLED: '0' };
const lines = execFileSync('go', ['list', '-deps', '-f', '{{if .Module}}{{.Module.Path}}\t{{.Module.Dir}}{{end}}', './cmd/disklens'], { encoding: 'utf8', env });
const modules = new Map(lines.trim().split('\n').filter(Boolean).map(line => line.split('\t')));
const notices = ['DiskLens third-party notices', ''];
for (const [name, directory] of modules) {
  if (name === 'disklens') continue;
  const licenses = readdirSync(directory, { withFileTypes: true }).filter(item => item.isFile() && /^(licen[sc]e|copying|copyright|notice)([._-].*)?$/i.test(item.name));
  if (!licenses.length) throw new Error('No license found for ' + name);
  for (const file of licenses) {
    const target = name.replaceAll('/', '__') + '-' + file.name;
    copyFileSync(path.join(directory, file.name), path.join(output, target));
    notices.push(name + ': ' + target);
  }
}
const goroot = execFileSync('go', ['env', 'GOROOT'], { encoding: 'utf8' }).trim();
const goLicense = [path.join(goroot, 'LICENSE'), path.join(goroot, '..', 'LICENSE')].find(existsSync);
if (!goLicense) throw new Error('Go toolchain license not found');
copyFileSync(goLicense, path.join(output, 'Go-LICENSE'));
copyFileSync('cmd/disklens/web/assets/LUCIDE-LICENSE.txt', path.join(output, 'Lucide-LICENSE.txt'));
notices.push('Go: Go-LICENSE', 'Lucide: Lucide-LICENSE.txt');
writeFileSync(path.join(output, 'NOTICES.txt'), notices.join('\n') + '\n');
console.log('Bundled licenses for ' + (modules.size - 1) + ' Go modules, Go and Lucide.');
