'use strict';
const https = require('node:https');
const LIMIT = 128 * 1024 * 1024;

function download(address, redirects = 0) {
  return new Promise((resolve, reject) => {
    const url = new URL(address);
    if (url.protocol !== 'https:' || url.username || url.password || redirects > 5) {
      reject(new Error('release downloads require HTTPS without credentials, with at most five redirects'));
      return;
    }
    const request = https.get(url, (response) => {
      if ([301, 302, 303, 307, 308].includes(response.statusCode) && response.headers.location) {
        response.resume();
        resolve(download(new URL(response.headers.location, url).href, redirects + 1));
        return;
      }
      if (response.statusCode !== 200) {
        response.resume();
        reject(new Error(`release download returned HTTP ${response.statusCode}; check network access and retry npm installation`));
        return;
      }
      const chunks = [];
      let size = 0;
      response.on('data', (chunk) => {
        size += chunk.length;
        if (size > LIMIT) response.destroy(new Error('release download exceeds the 128 MiB limit'));
        else chunks.push(chunk);
      });
      response.on('end', () => resolve(Buffer.concat(chunks)));
      response.on('error', reject);
    });
    const deadline = setTimeout(() => request.destroy(new Error('release download exceeded two minutes; check network access and retry npm installation')), 120000);
    request.on('close', () => clearTimeout(deadline));
    request.setTimeout(30000, () => request.destroy(new Error('release download timed out; check network access and retry npm installation')));
    request.on('error', reject);
  });
}

module.exports = { download };
