#!/usr/bin/env node
'use strict';

// Launcher: make sure the platform binary exists, then hand over to it.
// It re-downloads on demand, so `npm install --ignore-scripts` still works.

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const { assetName, binaryPath, releaseUrls } = require('../lib/platform');
const { download } = require('../lib/download');

async function ensureBinary() {
  const dest = binaryPath();
  if (fs.existsSync(dest)) return dest;

  const urls = releaseUrls(assetName());
  let lastError = null;

  for (const url of urls) {
    try {
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      await download(url, dest);
      console.error(`[opendrain] fetched ${assetName()}`);
      return dest;
    } catch (err) {
      lastError = err;
    }
  }

  throw new Error(
    `could not download the ${assetName()} binary for ${process.platform}/${process.arch}: ` +
      `${lastError && lastError.message}\n` +
      'Install it manually from https://github.com/yoyo636/opendrain/releases'
  );
}

ensureBinary()
  .then((bin) => {
    const res = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
    if (res.error) {
      console.error(`[opendrain] failed to run ${bin}: ${res.error.message}`);
      process.exit(1);
    }
    process.exit(res.status === null ? 1 : res.status);
  })
  .catch((err) => {
    console.error(`[opendrain] ${err.message}`);
    process.exit(1);
  });
