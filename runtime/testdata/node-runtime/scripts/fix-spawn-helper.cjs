// node-pty ships per-platform prebuilds with a small Mach-O / ELF helper
// called `spawn-helper`. Some npm versions extract the prebuild without the
// executable bit set, which makes posix_spawnp fail with the unhelpful
// "posix_spawnp failed." error at session-create time.
// This postinstall walks the prebuilds dir and chmods every spawn-helper to 755.
const fs = require('node:fs');
const path = require('node:path');

const prebuildsDir = path.resolve(__dirname, '..', 'node_modules', 'node-pty', 'prebuilds');
if (!fs.existsSync(prebuildsDir)) process.exit(0);

for (const entry of fs.readdirSync(prebuildsDir)) {
  const helper = path.join(prebuildsDir, entry, 'spawn-helper');
  if (fs.existsSync(helper)) {
    try {
      fs.chmodSync(helper, 0o755);
    } catch (err) {
      console.warn(`[prettyd postinstall] could not chmod ${helper}: ${err.message}`);
    }
  }
}
