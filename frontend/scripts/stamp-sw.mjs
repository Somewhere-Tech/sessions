// Post-build: stamp dist/sw.js's __BUILD_HASH__ with a hash of the built
// assets. vite `define` does not process public/ files, so the placeholder
// survives the copy verbatim — this script finishes the job. Deterministic:
// derived from dist/index.html content (changes whenever the bundle does).
import fs from 'node:fs';
import crypto from 'node:crypto';
const html = fs.readFileSync('dist/index.html');
const hash = crypto.createHash('sha256').update(html).digest('hex').slice(0, 12);
const p = 'dist/sw.js';
const src = fs.readFileSync(p, 'utf8');
if (!src.includes('__BUILD_HASH__')) { console.log('[stamp-sw] no placeholder (already stamped?)'); process.exit(0); }
fs.writeFileSync(p, src.replaceAll('__BUILD_HASH__', hash));
console.log(`[stamp-sw] sw.js cache version → ${hash}`);
