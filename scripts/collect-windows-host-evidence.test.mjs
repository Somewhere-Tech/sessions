import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const root = new URL('../', import.meta.url);
const read = (path) => readFile(new URL(path, root), 'utf8');

test('Windows evidence collection is read-only, redacted, and wired into the preview', async () => {
  const [script, workflow, morning, host] = await Promise.all([
    read('scripts/collect-windows-host-evidence.ps1'),
    read('.github/workflows/windows-preview.yml'),
    read('docs/WINDOWS_TEST.md'),
    read('docs/WINDOWS_HOST.md')
  ]);

  for (const binary of ['sessions.exe', 'sessionsd.exe', 'sessions-runner.exe']) {
    assert.match(script, new RegExp(binary.replace('.', '\\.')));
  }
  assert.match(script, /runtime-manifest\.json/);
  assert.match(script, /Get-Sha256Hex/);
  assert.match(script, /Get-AuthenticodeSignature/);
  assert.match(script, /ExpectedSignerThumbprint/);
  assert.match(script, /HKCU:\\Environment/);
  assert.match(script, /Somewhere Sessions/);
  assert.match(script, /--json support --diagnostics/);
  assert.match(script, /provider-child/);
  assert.match(script, /CompareBaseline/);
  assert.match(script, /runner\/provider PID preservation failed/);
  assert.match(script, /sessionContentCollected = \$false/);
  assert.match(script, /credentialsCollected = \$false/);
  assert.match(script, /processCommandLinesCollected = \$false/);
  assert.match(script, /uploaded = \$false/);

  for (const forbidden of [
    /Stop-Process/,
    /taskkill/i,
    /TerminateProcess/,
    /Remove-Item/,
    /Set-ItemProperty/,
    /New-ItemProperty/,
    /Invoke-WebRequest/,
    /Invoke-RestMethod/,
    /\.CommandLine/
  ]) {
    assert.doesNotMatch(script, forbidden);
  }

  assert.match(workflow, /collect-windows-host-evidence\.ps1/);
  assert.match(workflow, /windows-host-package-evidence\.json/);
  assert.match(workflow, /Sessions-Windows-preview/);
  assert.match(morning, /CompareBaseline/);
  assert.match(morning, /read-only evidence collector/);
  assert.match(host, /read-only\s+evidence collector/);
});
