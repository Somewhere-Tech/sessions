import { gzipSync } from 'node:zlib';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const assetsDirectory = fileURLToPath(new URL('../dist/assets/', import.meta.url));
const files = readdirSync(assetsDirectory).filter((name) => !name.endsWith('.map'));

const assets = files.map((name) => {
  const path = join(assetsDirectory, name);
  const contents = readFileSync(path);
  return {
    name,
    bytes: statSync(path).size,
    gzipBytes: gzipSync(contents, { level: 9 }).byteLength,
  };
});

const limits = {
  // Baseline measured 2026-09-03. These two assets are what the browser must
  // fetch before it can draw the first screen, so keep their headroom strict.
  entryJavaScript: 489_814,
  entryCSS: 279_911,
  // Deferred code may grow as the product gains secondary surfaces, but no
  // one interaction should have to download an oversized lazy chunk.
  totalJavaScript: 1_500_000,
  lazyJavaScriptChunkGzip: 250_000,
};

const entryJavaScript = assets.find((asset) => /^index-[^.]+\.js$/.test(asset.name));
const entryCSS = assets.find((asset) => /^index-[^.]+\.css$/.test(asset.name));
const javaScriptChunks = assets
  .filter((asset) => asset.name.endsWith('.js'))
  .sort((left, right) => right.bytes - left.bytes);
const totalJavaScript = javaScriptChunks
  .reduce((total, asset) => total + asset.bytes, 0);

if (!entryJavaScript || !entryCSS) {
  throw new Error('Bundle-size guard could not find the built index JavaScript and CSS assets.');
}

const failures = [];
if (entryJavaScript.bytes > limits.entryJavaScript) {
  failures.push(`${entryJavaScript.name} is ${entryJavaScript.bytes} bytes; entry limit ${limits.entryJavaScript}`);
}
if (entryCSS.bytes > limits.entryCSS) {
  failures.push(`${entryCSS.name} is ${entryCSS.bytes} bytes; entry limit ${limits.entryCSS}`);
}
if (totalJavaScript > limits.totalJavaScript) {
  failures.push(`all JavaScript is ${totalJavaScript} bytes; limit ${limits.totalJavaScript}`);
}
for (const chunk of javaScriptChunks) {
  if (chunk !== entryJavaScript && chunk.gzipBytes > limits.lazyJavaScriptChunkGzip) {
    failures.push(`${chunk.name} is ${chunk.gzipBytes} gzip bytes; lazy-chunk limit ${limits.lazyJavaScriptChunkGzip}`);
  }
}

console.log('Largest JavaScript chunks:');
for (const chunk of javaScriptChunks.slice(0, 8)) {
  console.log(
    `  ${chunk.name}: ${chunk.bytes} B (${chunk.gzipBytes} B gzip)`
      + (chunk === entryJavaScript ? ' [entry]' : ''),
  );
}

if (failures.length > 0) {
  console.error('Frontend bundle budget exceeded:');
  for (const failure of failures) console.error(`  ${failure}`);
  console.error('Reduce the entry path or split the interaction; do not raise the budget without a measured reason.');
  process.exit(1);
}

console.log(
  `Bundle-size guard passed: entry JS ${entryJavaScript.bytes} B (${entryJavaScript.gzipBytes} B gzip), ` +
    `entry CSS ${entryCSS.bytes} B (${entryCSS.gzipBytes} B gzip), total JS ${totalJavaScript} B.`,
);
