'use strict';
require('./lib/install.cjs').install().then((directory) => {
  console.log(`Sessions runtime staged at ${directory}. No service or session was started or stopped.`);
}).catch((error) => {
  console.error(`Sessions installation failed: ${error.message}`);
  process.exitCode = 1;
});
