import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(
  new URL('../luasrc/controller/proxypool.lua', import.meta.url),
  'utf8',
);
const mainView = await readFile(
  new URL('../luasrc/view/proxypool/main.htm', import.meta.url),
  'utf8',
);
const leaseView = await readFile(
  new URL('../luasrc/view/proxypool/lease.htm', import.meta.url),
  'utf8',
);
const v2Script = await readFile(
  new URL('../htdocs/luci-static/resources/proxypool-v2.js', import.meta.url),
  'utf8',
);

test('controller contains no direct system mutation or socket implementation', () => {
  for (const forbidden of [
    'os.execute',
    'sys.exec',
    'setsid',
    '/usr/lib/proxypool/',
    'uci:set',
    'require "nixio"',
    'nixio.socket',
  ]) {
    assert.equal(source.includes(forbidden), false, `forbidden controller primitive: ${forbidden}`);
  }
});

test('controller separates GET reads from LuCI protected POST writes', () => {
  assert.match(source, /entry\([^\n]+"read"[^\n]+call\("api_read"\)/);
  assert.match(source, /entry\([^\n]+"write"[^\n]+post\("api_write"\)/);
  assert.match(source, /node_save\s*=\s*"node\.save"/);
  assert.match(source, /node_delete\s*=\s*"node\.delete"/);
});

test('read and write handlers have disjoint action maps', () => {
  assert.match(source, /local READ_ACTIONS\s*=/);
  assert.match(source, /local WRITE_ACTIONS\s*=/);
  assert.doesNotMatch(source, /local ACTIONS\s*=/);
});

test('main page sends reads and writes to their defined endpoints', () => {
  assert.match(mainView, /data-api-read=/);
  assert.match(mainView, /data-api-write=/);
  assert.doesNotMatch(v2Script, /target=api\+/);
  assert.match(v2Script, /target\s*=\s*mutation\s*\?\s*apiWrite\s*:\s*apiRead/);
});

test('device unbind option has an explicit empty value', () => {
  assert.match(v2Script, /unboundOption\.value\s*=\s*''/);
});

test('polling degrades without AbortController and resumes after bfcache restore', () => {
  assert.match(v2Script, /typeof environment\.AbortController === 'function'/);
  assert.match(v2Script, /addEventListener\('pageshow'/);
  assert.match(v2Script, /event\.persisted/);
});

test('transactional import UI uses daemon preview and commit without per-node browser loops', () => {
  for (const id of ['pp-v2-import-open', 'pp-v2-import-raw', 'pp-v2-import-preview', 'pp-v2-import-commit']) {
    assert.match(mainView, new RegExp(`id="${id}"`));
  }
  assert.match(v2Script, /apiCall\('import_preview', params, true\)/);
  assert.match(v2Script, /apiCall\('import_commit', params, true\)/);
  assert.doesNotMatch(v2Script, /sequentialConnect|pollJob|pending marker/i);
});

test('cancelled import generations cannot revive stale previews and commit cannot pretend to cancel', () => {
  assert.match(v2Script, /var importGeneration = 0/);
  assert.match(v2Script, /generation !== importGeneration/);
  assert.match(v2Script, /cancel\.disabled = true/);
  assert.match(v2Script, /rawInput\.disabled = true/);
  assert.match(v2Script, /protocolInput\.disabled = true/);
  assert.match(v2Script, /addEventListener\('input', invalidateImportPreview\)/);
  assert.match(v2Script, /addEventListener\('change', invalidateImportPreview\)/);
});

test('sanitized export is local allowlisted JSON and never requests credentials', () => {
  assert.match(mainView, /id="pp-v2-export-safe"/);
  assert.match(v2Script, /sanitizedExport\(state\.status\)/);
  assert.doesNotMatch(v2Script, /include_credentials|export_secret|credentials_export/);
});

test('legacy auxiliary pages remain reachable without a dead mutation form', () => {
  assert.match(source, /"locked"[^\n]+call\("locked_page"\)/);
  assert.match(source, /"lease"[^\n]+call\("lease_page"\)/);
  assert.match(source, /function locked_page\(\)/);
  assert.match(source, /function lease_page\(\)/);
  assert.equal(leaseView.includes('[[sz]]'), false);
  assert.equal(leaseView.includes('"sz"'), false);
});
