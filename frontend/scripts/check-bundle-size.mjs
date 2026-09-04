import { gzipSync } from 'node:zlib';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

const assetsDirectory = new URL('../dist/assets/', import.meta.url);
const files = readdirSync(assetsDirectory).filter((name) => !name.endsWith('.map'));

const assets = files.map((name) => {
  const path = join(assetsDirectory.pathname, name);
  const contents = readFileSync(path);
  return {
    name,
    bytes: statSync(path).size,
    gzipBytes: gzipSync(contents, { level: 9 }).byteLength,
  };
});

const limits = {
  entryJavaScript: { raw: 715_000, gzip: 210_000 },
  entryCSS: { raw: 270_000, gzip: 45_000 },
  // Secondary settings and continuation surfaces are split by interaction,
  // and terminals ship one accelerated renderer with the built-in DOM
  // fallback. Measured total: 1,169,914 bytes; retain 10 KB of headroom.
  totalJavaScript: 1_179_914,
};

const entryJavaScript = assets.find((asset) => /^index-[^.]+\.js$/.test(asset.name));
const entryCSS = assets.find((asset) => /^index-[^.]+\.css$/.test(asset.name));
const totalJavaScript = assets
  .filter((asset) => asset.name.endsWith('.js'))
  .reduce((total, asset) => total + asset.bytes, 0);

if (!entryJavaScript || !entryCSS) {
  throw new Error('Bundle-size guard could not find the built index JavaScript and CSS assets.');
}

const failures = [];
if (entryJavaScript.bytes > limits.entryJavaScript.raw) {
  failures.push(`${entryJavaScript.name} is ${entryJavaScript.bytes} bytes; limit ${limits.entryJavaScript.raw}`);
}
if (entryJavaScript.gzipBytes > limits.entryJavaScript.gzip) {
  failures.push(`${entryJavaScript.name} is ${entryJavaScript.gzipBytes} gzip bytes; limit ${limits.entryJavaScript.gzip}`);
}
if (entryCSS.bytes > limits.entryCSS.raw) {
  failures.push(`${entryCSS.name} is ${entryCSS.bytes} bytes; limit ${limits.entryCSS.raw}`);
}
if (entryCSS.gzipBytes > limits.entryCSS.gzip) {
  failures.push(`${entryCSS.name} is ${entryCSS.gzipBytes} gzip bytes; limit ${limits.entryCSS.gzip}`);
}
if (totalJavaScript > limits.totalJavaScript) {
  failures.push(`all JavaScript is ${totalJavaScript} bytes; limit ${limits.totalJavaScript}`);
}

if (failures.length > 0) {
  console.error('Frontend bundle budget exceeded:');
  for (const failure of failures) console.error(`  ${failure}`);
  console.error('Reduce or lazy-load the added code; do not raise the budget without a measured reason.');
  process.exit(1);
}

console.log(
  `Bundle-size guard passed: entry JS ${entryJavaScript.bytes} B (${entryJavaScript.gzipBytes} B gzip), ` +
    `entry CSS ${entryCSS.bytes} B (${entryCSS.gzipBytes} B gzip), total JS ${totalJavaScript} B.`,
);
