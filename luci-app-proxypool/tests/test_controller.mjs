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

test('node notes cross the Lua bridge with bounded control-safe text', () => {
  assert.match(source, /local note\s*=\s*bounded\(http\.formvalue\("note"\),\s*800,\s*false\)/);
  assert.match(source, /note:find\("%c"\)/);
  assert.match(source, /note\s*=\s*note/);
  assert.match(mainView, /<input name="note" maxlength="200"/);
});

test('node editor and importer expose live L2TP and SOCKS5 without SLP migration claims', () => {
  assert.match(mainView, /<select name="protocol">\s*<option value="l2tp">L2TP<\/option>\s*<option value="socks5">SOCKS5<\/option>/);
  assert.match(mainView, /id="pp-v2-import-protocol"><option value="l2tp">L2TP<\/option><option value="socks5">SOCKS5<\/option>/);
  assert.doesNotMatch(mainView, /value="slp"|仅迁移保存|不会让设备通过它们上网/);
  assert.match(v2Script, /protocol:\s*form\.elements\.protocol\.value/);
});

test('node table exposes compact traffic columns and text-only note rendering', () => {
  assert.match(mainView, /<th>累计流量<\/th><th>实时速度<\/th>/);
  assert.match(v2Script, /pp-v2-node-note[^\n]+node\.note/);
  assert.match(v2Script, /cell\.colSpan\s*=\s*11/);
  assert.doesNotMatch(v2Script, /innerHTML/);
});

test('atomic device membership writes accept one bounded dense JSON array', () => {
  assert.match(source, /bindings_replace\s*=\s*"device\.bindings\.replace"/);
  assert.match(source, /formvalue\("device_ids_json"\)/);
  assert.match(source, /pcall\(json\.parse,\s*raw\)/);
  assert.match(source, /count\s*>\s*60/);
  assert.match(source, /type\(key\)\s*~\=\s*"number"/);
  assert.match(source, /seen\[device\]/);
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

test('main page delegates global navigation to the active theme', () => {
  assert.doesNotMatch(mainView, /proxypool-global\.(css|js)/);
  assert.doesNotMatch(mainView, /id="proxypool-global-menu"/);
});

test('node page exposes searchable multi-device membership with migration confirmation', () => {
  for (const id of ['pp-v2-binding-modal', 'pp-v2-binding-search', 'pp-v2-binding-list', 'pp-v2-binding-save', 'pp-v2-binding-cancel']) {
    assert.match(mainView, new RegExp(`id="${id}"`));
  }
  assert.match(v2Script, /deviceBindingRows\(bindingDevices,\s*bindingNodes/);
  assert.match(v2Script, /apiCall\('bindings_replace'/);
  assert.match(v2Script, /device_ids_json:\s*JSON\.stringify/);
  assert.match(v2Script, /confirm\([^\n]*迁移/);
});

test('ordinary node job and diagnostic rendering uses Chinese label helpers', () => {
  assert.match(v2Script, /stateLabel\('node',\s*node\.state\)/);
  assert.match(v2Script, /errorLabel\(node\.error_code\)/);
  assert.match(v2Script, /jobKindLabel\(job\.kind\)/);
  assert.match(v2Script, /stateLabel\('diagnostic',\s*model\.state\)/);
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

test('diagnostic creation is RPC-only and download is protected one-time streaming', () => {
  assert.match(source, /diagnostics_create\s*=\s*"diagnostics\.create"/);
  assert.match(source, /diagnostics\s*=\s*"diagnostics\.get"/);
  assert.match(source, /"download"[^\n]+post\("diagnostics_download"\)/);
  assert.match(source, /rpc\.call\("diagnostics\.claim"/);
  assert.match(source, /rpc\.call\("diagnostics\.release"/);
  assert.match(source, /expected_path\s*=\s*"\/tmp\/proxypool\/diagnostics\/"\s*\.\.\s*artifact/);
  assert.doesNotMatch(source, /diagnostics_claim\s*=/);
  assert.match(mainView, /id="pp-v2-diagnostics-create"/);
  assert.match(mainView, /id="pp-v2-diagnostics-download-form"/);
});

test('legacy auxiliary pages remain reachable without a dead mutation form', () => {
  assert.match(source, /"locked"[^\n]+call\("locked_page"\)/);
  assert.match(source, /"lease"[^\n]+call\("lease_page"\)/);
  assert.match(source, /function locked_page\(\)/);
  assert.match(source, /function lease_page\(\)/);
  assert.equal(leaseView.includes('[[sz]]'), false);
  assert.equal(leaseView.includes('"sz"'), false);
});
