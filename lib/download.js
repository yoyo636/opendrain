'use strict';

// Minimal, dependency-free HTTPS downloader with redirect following.
// node:https only — no third-party modules, so `npm install` stays cheap and
// auditable.

const crypto = require('crypto');
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

// Fetch a small text asset (SHA256SUMS) with the same redirect handling.
async function fetchText(url, limit = 64 * 1024) {
  const res = await get(url);
  return await new Promise((resolve, reject) => {
    let total = 0;
    const chunks = [];
    res.on('data', (chunk) => {
      total += chunk.length;
      if (total > limit) {
        res.destroy(new Error('response too large'));
        return;
      }
      chunks.push(chunk);
    });
    res.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    res.on('error', reject);
  });
}

function sha256File(path) {
  return new Promise((resolve, reject) => {
    const hash = crypto.createHash('sha256');
    const stream = fs.createReadStream(path);
    stream.on('data', (chunk) => hash.update(chunk));
    stream.on('end', () => resolve(hash.digest('hex')));
    stream.on('error', reject);
  });
}

// `sha256sum` output: "<64 hex>  <name>" (binary mode may use " *<name>").
async function verifyChecksum(file, sumsText, asset) {
  let expected = null;
  for (const line of sumsText.split(/\r?\n/)) {
    const m = line.trim().match(/^([0-9a-fA-F]{64})\s+\*?(.+)$/);
    if (m && m[2].trim() === asset) {
      expected = m[1].toLowerCase();
      break;
    }
  }
  if (!expected) return { ok: false, reason: `no checksum entry for ${asset}` };
  const actual = await sha256File(file);
  return { ok: actual === expected, expected, actual };
}

// Download a release asset and compare it with the release's SHA256SUMS.
// A missing checksum file is a warning (old releases / offline install);
// an actual mismatch deletes the file and fails so the caller can try the
// next release URL.
async function downloadVerified(url, dest, asset) {
  await download(url, dest);
  const sumsUrl = url.replace(/\/[^/]+\/?$/, '/SHA256SUMS');
  let sums;
  try {
    sums = await fetchText(sumsUrl);
  } catch (err) {
    console.warn(`[opendrain] checksum file unavailable (${err.message}); keeping ${asset}`);
    return dest;
  }
  const check = await verifyChecksum(dest, sums, asset);
  if (!check.ok) {
    if (!check.expected) {
      console.warn(`[opendrain] ${check.reason}; keeping ${asset}`);
      return dest;
    }
    try { fs.unlinkSync(dest); } catch (_) { /* best effort */ }
    throw new Error(
      `checksum mismatch for ${asset}: expected ${check.expected}, got ${check.actual}`
    );
  }
  return dest;
}

module.exports = { get, fetchText, download, downloadVerified, sha256File, verifyChecksum };
