'use strict';

// Platform/arch mapping for the prebuilt opendrain binaries published as
// GitHub release assets. Kept dependency-free on purpose: this file runs
// during `npm install` in whatever environment the user happens to have.

const path = require('path');

const REPO = 'yoyo636/opendrain';

function platformTriple(platform = process.platform, arch = process.arch) {
  let os = platform;
  if (os === 'win32') os = 'windows';

  let cpu = arch;
  if (cpu === 'x64') cpu = 'amd64';
  else if (cpu === 'arm64') cpu = 'arm64';
  else if (cpu === 'arm') cpu = 'armv7';

  return { os, arch: cpu };
}

function assetName(platform = process.platform, arch = process.arch) {
  const t = platformTriple(platform, arch);
  return `opendrain-${t.os}-${t.arch}${t.os === 'windows' ? '.exe' : ''}`;
}

function vendorDir() {
  return path.join(__dirname, '..', 'vendor');
}

function binaryPath(platform = process.platform, arch = process.arch) {
  return path.join(vendorDir(), assetName(platform, arch));
}

// Pinned release first (reproducible), then "latest" as a fallback for the
// case where a package version was published without its matching release.
function releaseUrls(asset, version) {
  const v = version || require('../package.json').version;
  return [
    `https://github.com/${REPO}/releases/download/v${v}/${asset}`,
    `https://github.com/${REPO}/releases/latest/download/${asset}`,
  ];
}

module.exports = { REPO, platformTriple, assetName, vendorDir, binaryPath, releaseUrls };
