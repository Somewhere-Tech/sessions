'use strict';
const path = require('node:path');
const { spawn } = require('node:child_process');
const { install } = require('./install.cjs');

module.exports = async function launch(command) {
  try {
    const directory = await install();
    const child = spawn(path.join(directory, command), process.argv.slice(2), { stdio: 'inherit', windowsHide: true });
    const signals = ['SIGINT', 'SIGTERM', 'SIGHUP'];
    const handlers = new Map(signals.map((signal) => [signal, () => child.kill(signal)]));
    for (const [signal, handler] of handlers) process.on(signal, handler);
    child.on('error', (error) => {
      console.error(`Sessions could not execute ${command}: ${error.message}`);
      process.exitCode = 1;
    });
    child.on('exit', (code, signal) => {
      for (const [name, handler] of handlers) process.removeListener(name, handler);
      if (signal) process.kill(process.pid, signal);
      else process.exitCode = code ?? 1;
    });
  } catch (error) {
    console.error(`Sessions could not start ${command}: ${error.message}`);
    process.exitCode = 1;
  }
};
