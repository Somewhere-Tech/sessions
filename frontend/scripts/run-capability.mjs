// The capability suite, as a bare `node <script>` so the smoke gate can run it.
//
// run-smoke.mjs deliberately only knows how to spawn `node <script>` and says
// so: "Wrap the extra work inside the script itself." This is that wrapper. It
// exists so `test:capability` can be a first-class member of GATE rather than a
// documented exclusion — a capability suite that the gate does not run is the
// same shape of problem the gate was written to stop.
//
// It runs vitest in-band and forwards the exit code unchanged. A capability
// that is broken today makes this exit non-zero, and that is the intended
// signal, not a defect in this file.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const frontendDir = fileURLToPath(new URL('..', import.meta.url));

const child = spawn(
  process.execPath,
  [
    fileURLToPath(new URL('../node_modules/vitest/vitest.mjs', import.meta.url)),
    'run',
    '--config',
    'vitest.config.ts',
    ...process.argv.slice(2)
  ],
  { cwd: frontendDir, stdio: 'inherit', env: { ...process.env, CI: process.env.CI ?? '1' } }
);

child.on('error', (error) => {
  process.stderr.write(`run-capability: could not start vitest: ${error.message}\n`);
  process.exit(2);
});
child.on('close', (code, signal) => {
  if (signal) {
    process.stderr.write(`run-capability: vitest was killed by ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
