import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import test from 'node:test';

const require = createRequire(import.meta.url);
const ui = require('../../htdocs/luci-static/resources/proxypool-v2.js');

test('newer status replaces state and stale status is ignored', () => {
  const initial = ui.initialState();
  const newer = ui.reduceState(initial, {
    type: 'status.received',
    value: { config: { revision: 8 }, desired: { nodes: [{ id: 'node-a' }] }, runtime: { nodes: [] } },
  });
  assert.equal(newer.revision, 8);
  assert.equal(newer.nodes[0].id, 'node-a');

  const stale = ui.reduceState(newer, {
    type: 'status.received',
    value: { config: { revision: 7 }, desired: { nodes: [{ id: 'stale' }] }, runtime: { nodes: [] } },
  });
  assert.strictEqual(stale, newer);
});

test('stale device response cannot overwrite a newer configuration view', () => {
  const state = { ...ui.initialState(), revision: 9, devices: [{ id: 'fresh' }] };
  const stale = ui.reduceState(state, {
    type: 'devices.received',
    value: { config_revision: 8, devices: [{ id: 'stale' }] },
  });
  assert.strictEqual(stale, state);

  const future = ui.reduceState(state, {
    type: 'devices.received',
    value: { config_revision: 10, devices: [{ id: 'future' }] },
  });
  assert.strictEqual(future, state);
});

test('successful mutation locally queues affected nodes until daemon progress arrives', () => {
  const state = {
    ...ui.initialState(),
    nodes: [{ id: 'node-a', state: 'online' }, { id: 'node-b', state: 'online' }],
  };
  const queued = ui.reduceState(state, {
    type: 'mutation.queued', jobId: 'job-next', nodeIds: ['node-a'],
  });
  assert.equal(queued.nodes[0].state, 'queued');
  assert.equal(queued.nodes[1].state, 'online');
  assert.equal(queued.pendingNodes['node-a'], 'job-next');
});

test('node presentation includes runtime error retry and bound device count', () => {
  const nodes = ui.presentNodes(
    [{ id: 'node-a', state: 'backoff', attempts: 2, retry_at: '2030-01-01T00:00:05Z', last_error: { code: 'probe_failed' } }],
    [{ id: 'phone', node_id: 'node-a', enabled: true }, { id: 'old', node_id: 'node-a', enabled: false }],
    Date.parse('2030-01-01T00:00:01Z'),
  );
  assert.equal(nodes[0].bound_count, 1);
  assert.equal(nodes[0].retry_seconds, 4);
  assert.equal(nodes[0].error_code, 'probe_failed');
});

test('job progress summary includes aggregate and per-node failures', () => {
  const summary = ui.jobSummary({
    id: 'job-1', state: 'running', total: 3, succeeded: 1, failed: 1, running: 1,
    nodes: [
      { node_id: 'node-a', state: 'online' },
      { node_id: 'node-b', state: 'failed', error: { code: 'probe_failed', message: 'probe failed' } },
    ],
  });
  assert.match(summary, /1\/3/);
  assert.match(summary, /node-b/);
  assert.match(summary, /probe_failed/);
});

test('retry countdown is bounded at zero', () => {
  assert.equal(ui.retryCountdown('2030-01-01T00:00:05Z', Date.parse('2030-01-01T00:00:01Z')), 4);
  assert.equal(ui.retryCountdown('2030-01-01T00:00:00Z', Date.parse('2030-01-01T00:00:01Z')), 0);
  assert.equal(ui.retryCountdown('', Date.now()), null);
});

test('node validation preserves blank credentials on update', () => {
  const result = ui.validateNodeForm({
    node_id: 'node-a', name: 'Node A', protocol: 'l2tp', enabled: true,
    server: 'vpn.example', port: '1701', username: '', password: '', expires_at: '',
    has_username: true, has_password: true,
  }, 12);
  assert.deepEqual(result.errors, []);
  assert.equal(result.params.username, '');
  assert.equal(result.params.password, '');
  assert.equal(result.params.expected_revision, 12);
});

test('node validation rejects unsupported protocol and missing create credentials', () => {
  const unsupported = ui.validateNodeForm({
    node_id: '', name: 'Proxy', protocol: 'socks5', enabled: true,
    server: 'proxy.example', port: '1080', username: 'u', password: 'p', expires_at: '',
  }, 2);
  assert.ok(unsupported.errors.includes('unsupported_protocol'));

  const missing = ui.validateNodeForm({
    node_id: '', name: 'L2TP', protocol: 'l2tp', enabled: true,
    server: 'vpn.example', port: '1701', username: '', password: '', expires_at: '',
  }, 2);
  assert.ok(missing.errors.includes('credentials_required'));
});

test('date-only expiry is normalized to the daemon RFC3339 contract', () => {
  const result = ui.validateNodeForm({
    node_id: '', name: 'L2TP', protocol: 'l2tp', enabled: true,
    server: 'vpn.example', port: '1701', username: 'user', password: 'secret',
    expires_at: '2030-01-02',
  }, 2);
  assert.deepEqual(result.errors, []);
  assert.equal(result.params.expires_at, '2030-01-02T23:59:59Z');
});

test('error formatting and escaping never produce raw markup', () => {
  assert.match(ui.formatError({ code: 'revision_conflict' }), /刷新/);
  assert.equal(ui.escapeText('<script>&"'), '&lt;script&gt;&amp;&quot;');
});

test('tracked jobs survive refresh through bounded session storage', () => {
  const values = new Map();
  const storage = {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };
  ui.saveTrackedJobIDs(storage, ['job-a', 'bad id', 'job-b', 'job-a']);
  assert.deepEqual(ui.loadTrackedJobIDs(storage), ['job-a', 'job-b']);
  assert.deepEqual(
    ui.visibleJobs([{ id: 'older' }, { id: 'job-b' }, { id: 'newer' }], ['job-b']).map((job) => job.id),
    ['job-b', 'newer', 'older'],
  );
});

test('diagnostic view exposes only safe artifact metadata', () => {
  const model = ui.diagnosticViewModel({
    job_id: 'diagnostic-job-1', state: 'ready',
    artifact: { artifact_id: 'diag-0123456789abcdef', filename: 'proxypool-diagnostics-diag-0123456789abcdef.tar.gz', size: 123,
      expires_at: '2030-01-01T00:01:00Z', path: '/tmp/proxypool/diagnostics/secret.tar.gz' },
  }, Date.parse('2030-01-01T00:00:00Z'));
  assert.equal(model.can_download, true);
  assert.equal(model.artifact_id, 'diag-0123456789abcdef');
  assert.equal(JSON.stringify(model).includes('/tmp/'), false);
  assert.equal(ui.diagnosticViewModel({ state: 'running' }).can_download, false);
  assert.equal(ui.diagnosticViewModel({ state: 'ready', artifact: {
    artifact_id: 'diag-0123456789abcdef', expires_at: '2029-12-31T23:59:59Z',
  } }, Date.parse('2030-01-01T00:00:00Z')).state, 'expired');
});

test('import preview sends untouched raw text without browser credential parsing', () => {
  const raw = 'vpn.example|alice|p@ss|with|pipes\nsecond.example|bob|secret';
  assert.deepEqual(ui.buildImportPreviewRequest(raw, 17), {
    protocol: 'l2tp', raw, expected_revision: 17,
  });
  assert.deepEqual(ui.buildImportPreviewRequest(raw, 17, 'socks5'), {
    protocol: 'socks5', raw, expected_revision: 17,
  });
});

test('server preview is rendered from sanitized fields and blocking errors disable commit', () => {
  const preview = {
    preview_id: 'preview-a', preview_hash: 'a'.repeat(64), base_revision: 17,
    blocked: true, added: 1, skipped: 0,
    rows: [{ line: 1, action: 'add', protocol: 'l2tp', server: 'vpn.example', port: 1701, secret_set: true }],
    errors: [{ line: 2, code: 'invalid_fields', message: '字段数量错误' }],
  };
  const model = ui.importPreviewModel(preview);
  assert.equal(model.can_commit, false);
  assert.equal(model.rows[0].secret_label, '已设置（不显示）');
  assert.match(model.summary, /1/);
  assert.equal(JSON.stringify(model).includes('p@ss'), false);
});

test('import commit is bound to preview hash and base revision', () => {
  const preview = { preview_id: 'preview-a', preview_hash: 'b'.repeat(64), base_revision: 23 };
  assert.deepEqual(ui.buildImportCommitRequest(preview), {
    preview_id: 'preview-a', preview_hash: 'b'.repeat(64), expected_revision: 23,
  });
});

test('revision conflict invalidates preview and successful commit job remains tracked', () => {
  let state = ui.reduceState(ui.initialState(), {
    type: 'import.preview.received', value: { preview_id: 'preview-a', blocked: false },
  });
  state = ui.reduceState(state, { type: 'import.failed', error: { code: 'revision_conflict' } });
  assert.equal(state.importPreview, null);
  assert.equal(state.importNeedsPreview, true);
  state = ui.reduceState(state, { type: 'job.tracked', jobId: 'job-import' });
  assert.deepEqual(state.trackedJobIds, ['job-import']);
});

test('pending legacy IP bindings are shown as waiting for device discovery', () => {
  const rows = ui.pendingBindingRows(
    [{ id: 'pending-a', legacy_ipv4: '192.168.9.88', node_id: 'node-a' }],
    [{ id: 'node-a', name: 'Node A' }],
  );
  assert.deepEqual(rows, [{
    id: 'pending-a', ipv4: '192.168.9.88', node_name: 'Node A', state: '等待设备出现', error_code: '',
  }]);
});

test('sanitized export uses an explicit allowlist and contains no credential material', () => {
  const exported = ui.sanitizedExport({
    config: { revision: 9 },
    desired: {
      enabled: true,
      nodes: [{ id: 'node-a', name: 'A', protocol: 'l2tp', server: 'vpn.example', port: 1701, enabled: true, policy_id: 1, has_password: true, password: 'DO-NOT-EXPORT' }],
      devices: [{ id: 'device-a', mac: '00:11:22:33:44:55', node_id: 'node-a', enabled: true }],
      pending_bindings: [],
    },
  });
  const encoded = JSON.stringify(exported);
  assert.match(encoded, /vpn\.example/);
  assert.equal(encoded.includes('DO-NOT-EXPORT'), false);
  assert.equal(encoded.includes('has_password'), false);
});
