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
  assert.doesNotMatch(mainView, /target=api\+/);
  assert.match(mainView, /target=apiRead\+'/);
  assert.match(mainView, /target=mutation\?apiWrite:apiRead/);
});

test('legacy auxiliary pages remain reachable without a dead mutation form', () => {
  assert.match(source, /"locked"[^\n]+call\("locked_page"\)/);
  assert.match(source, /"lease"[^\n]+call\("lease_page"\)/);
  assert.match(source, /function locked_page\(\)/);
  assert.match(source, /function lease_page\(\)/);
  assert.equal(leaseView.includes('[[sz]]'), false);
  assert.equal(leaseView.includes('"sz"'), false);
});
