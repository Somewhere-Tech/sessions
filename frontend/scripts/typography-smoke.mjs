import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(fileURLToPath(new URL('..', import.meta.url)), 'src');
const styles = [];

function collect(directory) {
  for (const name of readdirSync(directory)) {
    const path = join(directory, name);
    if (statSync(path).isDirectory()) collect(path);
    else if (name.endsWith('.css')) styles.push(path);
  }
}

collect(root);

const violations = [];
const declaration = /font(?:-size)?\s*:[^;\n]*?\b(\d+(?:\.\d+)?)px\b/g;

for (const path of styles) {
  const source = readFileSync(path, 'utf8');
  for (const match of source.matchAll(declaration)) {
    const size = Number(match[1]);
    if (size === 0 || size >= 12) continue;
    const line = source.slice(0, match.index).split('\n').length;
    violations.push(`${relative(root, path)}:${line}: ${match[0].trim()}`);
  }
}

if (violations.length > 0) {
  console.error('Typography floor failed: user-visible CSS must be at least 12px.');
  for (const violation of violations) console.error(`  ${violation}`);
  process.exit(1);
}

function luminance(hex) {
  const channels = hex.slice(1).match(/../g).map((channel) => Number.parseInt(channel, 16) / 255);
  const linear = channels.map((channel) =>
    channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4
  );
  return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
}

function contrast(foreground, background) {
  const values = [luminance(foreground), luminance(background)].sort((a, b) => b - a);
  return (values[0] + 0.05) / (values[1] + 0.05);
}

const globals = readFileSync(join(root, 'styles', 'globals.css'), 'utf8');
const darkTheme = globals.match(/:root\s*\{([\s\S]*?)\n\}/)?.[1] ?? '';
const lightTheme = globals.match(/\.operations-shell\[data-theme="light"\]\s*\{([\s\S]*?)\n\}/)?.[1] ?? '';
function token(block, name) {
  const value = block.match(new RegExp(`${name}:\\s*(#[0-9a-f]{6})`, 'i'))?.[1];
  if (!value) throw new Error(`Missing solid color token ${name}`);
  return value;
}

const contrastPairs = [
  ['dark secondary', token(darkTheme, '--fg-dim'), token(darkTheme, '--bg-base')],
  ['dark muted', token(darkTheme, '--fg-faint'), token(darkTheme, '--bg-base')],
  ['dark accent', token(darkTheme, '--accent'), token(darkTheme, '--bg-base')],
  ['light secondary', token(lightTheme, '--fg-dim'), token(lightTheme, '--bg-base')],
  ['light muted', token(lightTheme, '--fg-faint'), token(lightTheme, '--bg-base')],
  ['light accent', token(lightTheme, '--accent'), token(lightTheme, '--bg-base')]
];
const lowContrast = contrastPairs.filter(([, foreground, background]) =>
  contrast(foreground, background) < 4.5
);
if (lowContrast.length > 0) {
  console.error('Typography contrast failed: normal text tokens must meet 4.5:1.');
  for (const [name, foreground, background] of lowContrast) {
    console.error(`  ${name}: ${contrast(foreground, background).toFixed(2)}:1 (${foreground} on ${background})`);
  }
  process.exit(1);
}

console.log(`typography smoke passed (${styles.length} stylesheets, 12px floor, AA text tokens)`);
