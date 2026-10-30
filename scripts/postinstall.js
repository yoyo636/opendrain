#!/usr/bin/env node
'use strict';

// Postinstall: fetch the prebuilt binary for this platform from the GitHub
// release. It must never break `npm install` — on any failure we warn and
// exit 0, and the launcher retries the download on first use.

const fs = require('fs');
const path = require('path');

const { assetName, binaryPath, releaseUrls } = require('../lib/platform');
const { download } = require('../lib/download');

async function main() {
  const asset = assetName();
  const dest = binaryPath();

  if (fs.existsSync(dest)) {
    return; // already present
  }

  const urls = releaseUrls(asset);
  let lastError = null;

  for (const url of urls) {
    try {
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      await download(url, dest);
      console.log(`[opendrain] installed ${asset}`);
      return;
    } catch (err) {
      lastError = err;
    }
  }

  console.warn(
    `[opendrain] could not download ${asset} (${lastError && lastError.message}). ` +
      'It will be fetched automatically on first run.'
  );
}

main().catch((err) => {
  console.warn(`[opendrain] postinstall skipped: ${err.message}`);
});
