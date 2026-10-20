import { copyFileSync, mkdirSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
const root = fileURLToPath(new URL('..', import.meta.url));
const assets = path.join(root, 'cmd/disklens/web/assets');
mkdirSync(assets, { recursive: true });
mkdirSync(path.join(root, 'fnos/app/ui/images'), { recursive: true });
const names = ['layout-grid','folders','chart-pie','file-search','refresh-cw','scan-line','square','chevron-right','chevron-left','search','folder-open','folder','file','copy','download','history','files','sliders-horizontal','x','arrow-up','check','git-compare-arrows','chart-no-axes-combined','bell','calendar-clock','save'];
for (const name of names) {
  copyFileSync(path.join(root, 'node_modules/lucide-static/icons', name + '.svg'), path.join(assets, name + '.svg'));
}
copyFileSync(path.join(root, 'node_modules/lucide-static/LICENSE'), path.join(assets, 'LUCIDE-LICENSE.txt'));
for (const size of [64, 256]) {
  const target = path.join(root, 'fnos', size === 64 ? 'ICON.PNG' : 'ICON_256.PNG');
  execFileSync('rsvg-convert', ['-w', String(size), '-h', String(size), '-o', target, path.join(root, 'assets/disklens-icon.svg')]);
  copyFileSync(target, path.join(root, 'fnos/app/ui/images/icon_' + size + '.png'));
  copyFileSync(target, path.join(root, 'fnos/app/ui/images/disklens_' + size + '.png'));
}
copyFileSync(path.join(root, 'fnos/ICON_256.PNG'), path.join(assets, 'app-icon.png'));
console.log('Built desktop icons and bundled Lucide assets.');
