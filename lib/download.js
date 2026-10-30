'use strict';

// Minimal, dependency-free HTTPS downloader with redirect following.
// node:https only — no third-party modules, so `npm install` stays cheap and
// auditable.

const fs = require('fs');
const https = require('https');
const { pipeline } = require('stream/promises');

function get(url, redirects = 0) {
  return new Promise((resolve, reject) => {
    if (redirects > 5) {
      reject(new Error('too many redirects'));
      return;
    }
    const req = https.get(
      url,
      { headers: { 'User-Agent': 'opendrain-installer' } },
      (res) => {
        const status = res.statusCode || 0;
        if (status >= 300 && status < 400 && res.headers.location) {
          res.resume();
          resolve(get(new URL(res.headers.location, url).toString(), redirects + 1));
          return;
        }
        if (status !== 200) {
          res.resume();
          reject(new Error(`HTTP ${status} for ${url}`));
          return;
        }
        resolve(res);
      }
    );
    req.on('error', reject);
    req.setTimeout(60000, () => req.destroy(new Error('download timeout')));
  });
}

async function download(url, dest) {
  const res = await get(url);
  const tmp = `${dest}.part`;
  try {
    await pipeline(res, fs.createWriteStream(tmp));
    fs.renameSync(tmp, dest);
  } catch (err) {
    try { fs.unlinkSync(tmp); } catch (_) { /* nothing to clean up */ }
    throw err;
  }
  try { fs.chmodSync(dest, 0o755); } catch (_) { /* best effort on odd filesystems */ }
  return dest;
}

module.exports = { get, download };
