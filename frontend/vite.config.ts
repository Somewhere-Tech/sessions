import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { createHash, randomBytes } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { Plugin } from 'vite';

const packageSource = readFileSync(new URL('./package.json', import.meta.url));
const buildSeed = process.env.VITE_BUILD_ID?.trim() || randomBytes(16).toString('hex');
const BUILD_HASH = createHash('sha256')
  .update(packageSource)
  .update('\0')
  .update(buildSeed)
  .digest('hex')
  .slice(0, 12);

function injectServiceWorkerBuildHash(): Plugin {
  return {
    name: 'inject-service-worker-build-hash',
    apply: 'build',
    enforce: 'post',
    writeBundle(options) {
      if (!options.dir) throw new Error('Service worker build hash requires a directory output.');
      const serviceWorkerPath = resolve(options.dir, 'sw.js');
      const source = readFileSync(serviceWorkerPath, 'utf8');
      if (!source.includes('__BUILD_HASH__')) {
        throw new Error('Service worker build hash marker was not found.');
      }
      writeFileSync(serviceWorkerPath, source.replaceAll('__BUILD_HASH__', BUILD_HASH));
    }
  };
}

const SESSIONS_HOST = process.env.SESSIONS_HOST ?? '127.0.0.1';
const SESSIONS_PORT = process.env.SESSIONS_PORT ?? '8787';
const SESSIONS_HTTP = `http://${SESSIONS_HOST}:${SESSIONS_PORT}`;
const SESSIONS_WS = `ws://${SESSIONS_HOST}:${SESSIONS_PORT}`;

const VITE_HOST = process.env.VITE_HOST ?? '127.0.0.1';
const ANY_BIND = new Set(['0.0.0.0', '::', '::0', '*']);
if (ANY_BIND.has(VITE_HOST) || ANY_BIND.has(SESSIONS_HOST)) {
  // Hard fail before Vite even starts. We never bind to any-interface;
  // for tailnet access, set the env var to your specific tailnet IP.
  throw new Error(
    `sessions config: ${ANY_BIND.has(VITE_HOST) ? 'VITE_HOST' : 'SESSIONS_HOST'} ` +
    `is set to ${ANY_BIND.has(VITE_HOST) ? VITE_HOST : SESSIONS_HOST}, which is rejected. ` +
    `Use a specific address (127.0.0.1 for loopback, or a 100.x.y.z tailnet IP).`
  );
}

export default defineConfig({
  // Relative asset URLs let the exact same dist work at a hosted project
  // root, under a static preview subpath, and from sessionsd's embedded UI.
  base: './',
  plugins: [react(), injectServiceWorkerBuildHash()],
  server: {
    host: VITE_HOST,
    // 5273 (not 5173) so sessions can run alongside sessions-tmux,
    // which is still on the default :5173 until Phase 5.
    port: Number(process.env.VITE_PORT ?? 5273),
    strictPort: false,
    proxy: {
      '/api': { target: SESSIONS_HTTP, changeOrigin: true },
      '/ws': { target: SESSIONS_WS, ws: true, changeOrigin: true }
    }
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          xterm: ['@xterm/xterm', '@xterm/addon-fit']
        }
      }
    }
  }
});
