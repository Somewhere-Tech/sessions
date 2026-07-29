const fs = require('node:fs');
const path = require('node:path');

const packageDir = path.resolve(__dirname, '..');
const repoDir = path.resolve(packageDir, '..');
const sourceWebDir = path.join(repoDir, 'frontend', 'dist');
const sourceWebIndex = path.join(sourceWebDir, 'index.html');
const bundledWebDir = path.join(packageDir, 'web');
const sourceLicense = path.join(repoDir, 'LICENSE');
const bundledLicense = path.join(packageDir, 'LICENSE');

if (!fs.existsSync(sourceWebIndex)) {
  throw new Error(`frontend build is missing ${sourceWebIndex}`);
}
if (!fs.existsSync(sourceLicense)) {
  throw new Error(`repository license is missing ${sourceLicense}`);
}

fs.rmSync(bundledWebDir, { recursive: true, force: true });
fs.cpSync(sourceWebDir, bundledWebDir, { recursive: true });
fs.copyFileSync(sourceLicense, bundledLicense);

process.stdout.write(`staged frontend in ${bundledWebDir}\n`);
