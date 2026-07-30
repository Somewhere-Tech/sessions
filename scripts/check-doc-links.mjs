import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, isAbsolute, resolve, sep } from 'node:path';
import process from 'node:process';

const root = process.cwd();
const tracked = execFileSync('git', ['ls-files', '*.md'], {
  cwd: root,
  encoding: 'utf8'
}).trim().split('\n').filter(Boolean);
const broken = [];
const markdownLink = /(?<!!)\[[^\]]*]\(([^)]+)\)/g;

for (const relativePath of tracked) {
  const source = readFileSync(resolve(root, relativePath), 'utf8');
  for (const match of source.matchAll(markdownLink)) {
    let target = match[1].trim();
    if (target.startsWith('<') && target.endsWith('>')) {
      target = target.slice(1, -1);
    }
    if (
      target === ''
      || target.startsWith('#')
      || /^[a-z][a-z0-9+.-]*:/i.test(target)
      || target.startsWith('//')
    ) {
      continue;
    }
    target = target.split('#', 1)[0].split('?', 1)[0];
    if (target === '') continue;
    const destination = isAbsolute(target)
      ? resolve(root, `.${target}`)
      : resolve(root, dirname(relativePath), target);
    if (destination !== root && !destination.startsWith(root + sep)) {
      broken.push(`${relativePath}: link escapes the repository: ${match[1]}`);
    } else if (!existsSync(destination)) {
      broken.push(`${relativePath}: missing relative link target: ${match[1]}`);
    }
  }
}

if (broken.length > 0) {
  process.stderr.write(`broken public documentation links:\n${broken.join('\n')}\n`);
  process.exit(1);
}

process.stdout.write('public documentation links passed\n');
