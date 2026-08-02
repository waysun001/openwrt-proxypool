(function(root, factory) {
    'use strict';
    var api = factory();
    if (typeof module === 'object' && module.exports) module.exports = api;
    if (root) root.ProxyPoolV2 = api;
    if (root && root.document) {
        var start = function() { api.boot(root.document, root); };
        if (root.document.readyState === 'loading') root.document.addEventListener('DOMContentLoaded', start);
        else start();
    }
})(typeof globalThis !== 'undefined' ? globalThis : this, function() {
    'use strict';

    var TRACKED_JOBS_KEY = 'proxypool.v2.tracked_jobs';
    var ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

    function initialState() {
        return {
            revision: 0,
            status: null,
            nodes: [],
            devices: [],
            jobs: [],
            trackedJobIds: [],
            pendingNodes: {},
            pendingBindings: [],
            importPreview: null,
            importNeedsPreview: false
        };
    }

    function reduceState(state, event) {
        state = state || initialState();
        event = event || {};
        if (event.type === 'status.received') {
            var status = event.value || {};
            var revision = Number(status.config && status.config.revision || 0);
            if (revision < state.revision) return state;
            var runtime = {};
            ((status.runtime && status.runtime.nodes) || []).forEach(function(node) {
                runtime[node.node_id] = node;
            });
            var pendingNodes = Object.assign({}, state.pendingNodes);
            var nodes = ((status.desired && status.desired.nodes) || []).map(function(node) {
                var copy = Object.assign({}, node);
                var observed = runtime[node.id];
                if (observed) {
                    copy.state = observed.state;
                    copy.attempts = observed.attempts || 0;
                    copy.last_error = observed.last_error || null;
                    copy.retry_at = observed.retry_at || '';
                } else {
                    copy.state = node.enabled ? 'queued' : 'disabled';
                }
                if (pendingNodes[node.id]) {
                    if (observed && observed.job_id === pendingNodes[node.id]) delete pendingNodes[node.id];
                    else copy.state = 'queued';
                }
                return copy;
            });
            return Object.assign({}, state, {
                revision: revision,
                status: status,
                nodes: nodes,
                pendingNodes: pendingNodes,
                pendingBindings: ((status.desired && status.desired.pending_bindings) || []).slice()
            });
        }
        if (event.type === 'devices.received') {
            var deviceResult = event.value || {};
            var deviceRevision = Number(deviceResult.config_revision || 0);
            if (state.revision && deviceRevision !== state.revision) return state;
            return Object.assign({}, state, { devices: (deviceResult.devices || []).slice() });
        }
        if (event.type === 'jobs.received') {
            return Object.assign({}, state, { jobs: ((event.value || {}).jobs || []).slice() });
        }
        if (event.type === 'job.tracked') {
            var tracked = uniqueIDs(state.trackedJobIds.concat([event.jobId]));
            return Object.assign({}, state, { trackedJobIds: tracked });
        }
        if (event.type === 'mutation.queued') {
            var pending = Object.assign({}, state.pendingNodes);
            var nextNodes = state.nodes.map(function(node) {
                if ((event.nodeIds || []).indexOf(node.id) === -1) return node;
                pending[node.id] = event.jobId;
                return Object.assign({}, node, { state: 'queued', last_error: null, retry_at: '' });
            });
            return Object.assign({}, state, { nodes: nextNodes, pendingNodes: pending });
        }
        if (event.type === 'import.preview.received') {
            return Object.assign({}, state, { importPreview: event.value || null, importNeedsPreview: false });
        }
        if (event.type === 'import.cleared') {
            return Object.assign({}, state, { importPreview: null, importNeedsPreview: false });
        }
        if (event.type === 'import.failed' && event.error && event.error.code === 'revision_conflict') {
            return Object.assign({}, state, { importPreview: null, importNeedsPreview: true });
        }
        return state;
    }

    function escapeText(value) {
        return String(value == null ? '' : value)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function formatError(error) {
        error = error || {};
        var messages = {
            revision_conflict: '配置已变化，请刷新页面后重试。',
            capacity_exceeded: '节点数量已达到上限（60）。',
            service_unavailable: 'ProxyPool 服务暂时不可用。',
            bad_gateway: 'ProxyPool 服务响应异常。',
            unsupported: '当前版本暂不支持该协议。',
            not_found: '目标已不存在，请刷新页面。',
            invalid_request: '提交内容不完整或格式错误。',
            invalid_config: '配置未通过安全校验。'
        };
        return messages[error.code] || String(error.message || error.code || '未知错误');
    }

    function retryCountdown(deadline, nowMilliseconds) {
        if (!deadline) return null;
        var target = Date.parse(deadline);
        if (!Number.isFinite(target)) return null;
        var now = Number.isFinite(nowMilliseconds) ? nowMilliseconds : Date.now();
        return Math.max(0, Math.ceil((target - now) / 1000));
    }

    function jobSummary(job) {
        job = job || {};
        var parts = [
            String(job.state || 'unknown'),
            String(Number(job.succeeded || 0)) + '/' + String(Number(job.total || 0))
        ];
        (job.nodes || []).forEach(function(node) {
            if (node.error || node.state === 'failed') {
                parts.push(String(node.node_id || '?') + ':' + String(node.error && node.error.code || 'failed'));
            }
        });
        return parts.join(' · ');
    }

    function validateNodeForm(form, revision) {
        form = form || {};
        var errors = [];
        var protocol = String(form.protocol || '');
        var name = String(form.name || '').trim();
        var server = String(form.server || '').trim();
        var port = Number(form.port);
        var nodeID = String(form.node_id || '');
        var username = String(form.username || '');
        var password = String(form.password || '');
        var expiresAt = String(form.expires_at || '');
        var creating = nodeID === '';

        if (protocol !== 'l2tp') errors.push('unsupported_protocol');
        if (!name || name.length > 128) errors.push('invalid_name');
        if (!server || server.length > 253) errors.push('invalid_server');
        if (!Number.isInteger(port) || port < 1 || port > 65535) errors.push('invalid_port');
        if (!creating && !ID_PATTERN.test(nodeID)) errors.push('invalid_node_id');
        if (!Number.isInteger(Number(revision)) || Number(revision) < 1) errors.push('invalid_revision');
        if (/^\d{4}-\d{2}-\d{2}$/.test(expiresAt)) expiresAt += 'T23:59:59Z';
        else if (expiresAt && !Number.isFinite(Date.parse(expiresAt))) errors.push('invalid_expiry');
        if ((creating || !form.has_username) && !username) errors.push('credentials_required');
        if ((creating || !form.has_password) && !password && !errors.includes('credentials_required')) errors.push('credentials_required');

        return {
            errors: errors,
            params: {
                node_id: nodeID,
                name: name,
                protocol: protocol,
                enabled: !!form.enabled,
                server: server,
                port: port,
                username: username,
                password: password,
                expires_at: expiresAt,
                expected_revision: Number(revision)
            }
        };
    }

    function uniqueIDs(values) {
        var seen = {};
        var result = [];
        (values || []).forEach(function(value) {
            value = String(value || '');
            if (!ID_PATTERN.test(value) || seen[value]) return;
            seen[value] = true;
            result.push(value);
        });
        return result.slice(-32);
    }

    function loadTrackedJobIDs(storage) {
        if (!storage || typeof storage.getItem !== 'function') return [];
        try {
            var parsed = JSON.parse(storage.getItem(TRACKED_JOBS_KEY) || '[]');
            return Array.isArray(parsed) ? uniqueIDs(parsed) : [];
        } catch (_) {
            return [];
        }
    }

    function saveTrackedJobIDs(storage, values) {
        var normalized = uniqueIDs(values);
        if (storage && typeof storage.setItem === 'function') {
            try { storage.setItem(TRACKED_JOBS_KEY, JSON.stringify(normalized)); } catch (_) {}
        }
        return normalized;
    }

    function presentNodes(nodes, devices, nowMilliseconds) {
        var counts = {};
        (devices || []).forEach(function(device) {
            if (device.enabled && device.node_id) counts[device.node_id] = (counts[device.node_id] || 0) + 1;
        });
        return (nodes || []).map(function(node) {
            var copy = Object.assign({}, node);
            copy.bound_count = counts[node.id] || 0;
            copy.retry_seconds = retryCountdown(node.retry_at, nowMilliseconds);
            copy.error_code = node.last_error && node.last_error.code || '';
            return copy;
        });
    }

    function visibleJobs(jobs, trackedJobIds) {
        var byID = {};
        (jobs || []).forEach(function(job) { byID[job.id] = job; });
        var result = [];
        var seen = {};
        (trackedJobIds || []).slice().reverse().forEach(function(id) {
            if (byID[id] && !seen[id]) {
                seen[id] = true;
                result.push(byID[id]);
            }
        });
        (jobs || []).slice().reverse().forEach(function(job) {
            if (!seen[job.id]) {
                seen[job.id] = true;
                result.push(job);
            }
        });
        return result;
    }

    function encodeParams(params) {
        return Object.keys(params || {}).map(function(key) {
            return encodeURIComponent(key) + '=' + encodeURIComponent(params[key] == null ? '' : String(params[key]));
        }).join('&');
    }

    function buildImportPreviewRequest(raw, revision, protocol) {
        protocol = String(protocol || 'l2tp');
        if (['l2tp', 'socks5', 'slp'].indexOf(protocol) === -1) protocol = 'l2tp';
        return { protocol: protocol, raw: String(raw || ''), expected_revision: Number(revision) };
    }

    function importPreviewModel(preview) {
        preview = preview || {};
        return {
            can_commit: !preview.blocked && !!preview.preview_id && !!preview.preview_hash,
            summary: '新增 ' + Number(preview.added || 0) + '，跳过 ' + Number(preview.skipped || 0) + (preview.blocked ? '，存在阻断错误' : '，可以提交'),
            rows: (preview.rows || []).map(function(row) {
                return {
                    line: Number(row.line || 0),
                    action: String(row.action || ''),
                    protocol: String(row.protocol || ''),
                    endpoint: String(row.server || '') + ':' + String(row.port || ''),
                    expires_at: String(row.expires_at || ''),
                    secret_label: row.secret_set ? '已设置（不显示）' : '未设置'
                };
            }),
            errors: (preview.errors || []).map(function(error) {
                return { line: Number(error.line || 0), code: String(error.code || ''), message: String(error.message || '') };
            })
        };
    }

    function buildImportCommitRequest(preview) {
        preview = preview || {};
        return {
            preview_id: String(preview.preview_id || ''),
            preview_hash: String(preview.preview_hash || ''),
            expected_revision: Number(preview.base_revision || 0)
        };
    }

    function pendingBindingRows(bindings, nodes) {
        var names = {};
        (nodes || []).forEach(function(node) { names[node.id] = node.name; });
        return (bindings || []).map(function(binding) {
            return {
                id: String(binding.id || ''),
                ipv4: String(binding.legacy_ipv4 || ''),
                node_name: String(names[binding.node_id] || binding.node_id || '-'),
                state: '等待设备出现',
                error_code: String(binding.error_code || '')
            };
        });
    }

    function sanitizedExport(status) {
        status = status || {};
        var desired = status.desired || {};
        return {
            schema_version: 2,
            kind: 'proxypool-v2-sanitized',
            config_revision: Number(status.config && status.config.revision || 0),
            desired: {
                enabled: !!desired.enabled,
                nodes: (desired.nodes || []).map(function(node) {
                    return {
                        id: node.id, name: node.name, protocol: node.protocol, enabled: !!node.enabled,
                        delete_pending: !!node.delete_pending, server: node.server, port: node.port,
                        expires_at: node.expires_at || '', policy_id: node.policy_id, revision: node.revision
                    };
                }),
                devices: (desired.devices || []).map(function(device) {
                    return {
                        id: device.id, mac: device.mac, hostname: device.hostname || '', fixed_ipv4: device.fixed_ipv4 || '',
                        node_id: device.node_id || '', enabled: !!device.enabled
                    };
                }),
                pending_bindings: (desired.pending_bindings || []).map(function(binding) {
                    return {
                        id: binding.id, legacy_ipv4: binding.legacy_ipv4, node_id: binding.node_id,
                        error_code: binding.error_code || ''
                    };
                })
            }
        };
    }

    function boot(document, environment) {
        var app = document.getElementById('pp-v2-app');
        if (!app || app.getAttribute('data-booted') === '1') return;
        app.setAttribute('data-booted', '1');
        var apiRead = app.getAttribute('data-api-read');
        var apiWrite = app.getAttribute('data-api-write');
        var token = app.getAttribute('data-token');
        var storage = environment.sessionStorage;
        var state = initialState();
        state.trackedJobIds = loadTrackedJobIDs(storage);
        var timer = null;
        var pollController = null;
        var pollGeneration = 0;
        var importGeneration = 0;
        var stopped = false;

        function element(tag, className, text) {
            var node = document.createElement(tag);
            if (className) node.className = className;
            if (text != null) node.textContent = String(text);
            return node;
        }

        function request(target, options) {
            if (typeof environment.fetch === 'function') return environment.fetch(target, options);
            return new Promise(function(resolve, reject) {
                var xhr = new environment.XMLHttpRequest();
                xhr.open(options.method, target, true);
                Object.keys(options.headers || {}).forEach(function(name) { xhr.setRequestHeader(name, options.headers[name]); });
                xhr.withCredentials = true;
                xhr.onload = function() {
                    resolve({ ok: xhr.status >= 200 && xhr.status < 300, text: function() { return Promise.resolve(xhr.responseText); } });
                };
                xhr.onerror = function() { reject({ code: 'service_unavailable' }); };
                xhr.onabort = function() { reject({ name: 'AbortError' }); };
                if (options.signal && typeof options.signal.addEventListener === 'function') {
                    options.signal.addEventListener('abort', function() { xhr.abort(); });
                }
                xhr.send(options.body || null);
            });
        }

        function apiCall(action, params, mutation, signal) {
            params = Object.assign({}, params || {}, { action: action });
            var options = { method: mutation ? 'POST' : 'GET', credentials: 'same-origin', headers: { Accept: 'application/json' } };
            var target = mutation ? apiWrite : apiRead;
            if (mutation) {
                params.token = token;
                options.headers['Content-Type'] = 'application/x-www-form-urlencoded;charset=UTF-8';
                options.body = encodeParams(params);
            } else {
                target += '?' + encodeParams(params);
                options.signal = signal;
            }
            return request(target, options).then(function(response) {
                return response.text().then(function(text) {
                    var envelope;
                    try { envelope = JSON.parse(text); } catch (_) { throw { code: 'bad_gateway' }; }
                    if (!response.ok || !envelope.success) throw envelope.error || { code: 'bad_gateway' };
                    return envelope.result;
                });
            });
        }

        function showError(error) {
            document.getElementById('pp-v2-error').textContent = error ? formatError(error) : '';
        }

        function renderImportPreview() {
            var target = document.getElementById('pp-v2-import-result');
            var commit = document.getElementById('pp-v2-import-commit');
            target.textContent = '';
            if (state.importNeedsPreview) {
                target.appendChild(element('div', 'pp-v2-import-error', '配置已变化，请重新点击安全预览。'));
                commit.disabled = true;
                return;
            }
            if (!state.importPreview) {
                commit.disabled = true;
                return;
            }
            var model = importPreviewModel(state.importPreview);
            target.appendChild(element('div', 'pp-v2-import-summary', model.summary));
            model.rows.forEach(function(row) {
                target.appendChild(element('div', 'pp-v2-import-row',
                    '第 ' + row.line + ' 行 · ' + row.action + ' · ' + row.protocol.toUpperCase() + ' · ' + row.endpoint + ' · 认证：' + row.secret_label));
            });
            model.errors.forEach(function(error) {
                target.appendChild(element('div', 'pp-v2-import-error',
                    (error.line ? '第 ' + error.line + ' 行 · ' : '') + error.code + '：' + error.message));
            });
            commit.disabled = !model.can_commit;
        }

        function trackMutation(result, params) {
            if (result && result.job_id) {
                state = reduceState(state, { type: 'job.tracked', jobId: result.job_id });
                state = reduceState(state, { type: 'mutation.queued', jobId: result.job_id, nodeIds: params && params.node_id ? [params.node_id] : [] });
                state.trackedJobIds = saveTrackedJobIDs(storage, state.trackedJobIds);
                render();
            }
            return poll();
        }

        function mutate(action, params) {
            showError(null);
            return apiCall(action, params, true).then(function(result) { return trackMutation(result, params); }).catch(showError);
        }

        function actionButton(label, handler, disabled) {
            var button = element('button', 'cbi-button pp-v2-button', label);
            button.type = 'button';
            button.disabled = !!disabled;
            button.addEventListener('click', handler);
            return button;
        }

        function renderSummary() {
            var online = state.nodes.filter(function(node) { return node.state === 'online'; }).length;
            var bound = state.devices.filter(function(device) { return device.enabled && device.node_id; }).length;
            document.getElementById('pp-v2-total').textContent = String(state.nodes.length);
            document.getElementById('pp-v2-online').textContent = String(online);
            document.getElementById('pp-v2-bound').textContent = String(bound);
            document.getElementById('pp-v2-revision').textContent = state.revision ? 'Revision ' + state.revision : '';
        }

        function openNodeEditor(node) {
            var modal = document.getElementById('pp-v2-node-modal');
            var form = document.getElementById('pp-v2-node-form');
            form.reset();
            form.elements.node_id.value = node && node.id || '';
            form.elements.name.value = node && node.name || '';
            form.elements.server.value = node && node.server || '';
            form.elements.port.value = node && node.port || 1701;
            form.elements.enabled.checked = node ? !!node.enabled : true;
            form.elements.expires_at.value = node && node.expires_at ? String(node.expires_at).slice(0, 10) : '';
            form.elements.username.value = '';
            form.elements.password.value = '';
            form.setAttribute('data-has-username', node && node.has_username ? '1' : '0');
            form.setAttribute('data-has-password', node && node.has_password ? '1' : '0');
            document.getElementById('pp-v2-node-modal-title').textContent = node ? '编辑节点' : '新增 L2TP 节点';
            modal.hidden = false;
        }

        function renderNodes() {
            var body = document.getElementById('pp-v2-node-list');
            body.textContent = '';
            presentNodes(state.nodes, state.devices, Date.now()).forEach(function(node) {
                var row = element('tr');
                row.appendChild(element('td', '', node.name));
                row.appendChild(element('td', '', node.server + ':' + node.port));
                row.appendChild(element('td', '', node.protocol.toUpperCase()));
                row.appendChild(element('td', 'pp-v2-state pp-v2-state-' + node.state, node.delete_pending ? '删除中' : node.state));
                row.appendChild(element('td', '', node.error_code || '-'));
                row.appendChild(element('td', '', node.retry_seconds == null ? (node.attempts ? '尝试 ' + node.attempts : '-') : node.retry_seconds + 's'));
                row.appendChild(element('td', '', node.bound_count));
                row.appendChild(element('td', '', node.policy_id));
                var actions = element('td', 'pp-v2-actions');
                actions.appendChild(actionButton('编辑', function() { openNodeEditor(node); }, node.delete_pending));
                actions.appendChild(actionButton(node.enabled ? '重连' : '连接', function() {
                    mutate('node_action', { node_id: node.id, operation: node.enabled ? 'reconnect' : 'connect', expected_revision: state.revision });
                }, node.delete_pending));
                if (node.enabled) {
                    actions.appendChild(actionButton('停止', function() {
                        mutate('node_action', { node_id: node.id, operation: 'stop', expected_revision: state.revision });
                    }, node.delete_pending));
                }
                actions.appendChild(actionButton('删除', function() {
                    if (environment.confirm('确定删除节点“' + node.name + '”？绑定设备会同时解除。')) {
                        mutate('node_delete', { node_id: node.id, expected_revision: state.revision });
                    }
                }, node.delete_pending));
                row.appendChild(actions);
                body.appendChild(row);
            });
            if (!state.nodes.length) {
                var empty = element('tr');
                var cell = element('td', 'pp-v2-empty', '暂无节点，请先新增或批量导入。');
                cell.colSpan = 9;
                empty.appendChild(cell);
                body.appendChild(empty);
            }
        }

        function renderDevices() {
            var body = document.getElementById('pp-v2-device-list');
            body.textContent = '';
            state.devices.forEach(function(device) {
                var row = element('tr');
                row.appendChild(element('td', '', (device.hostname || '未命名设备') + ' / ' + device.mac));
                row.appendChild(element('td', '', device.current_ipv4 || '-'));
                row.appendChild(element('td', '', device.fixed_ipv4 || '-'));
                row.appendChild(element('td', '', device.ingress || '-'));
                var binding = element('td');
                var select = element('select', 'cbi-input-select');
                var unboundOption = element('option', '', '不绑定');
                unboundOption.value = '';
                select.appendChild(unboundOption);
                state.nodes.filter(function(node) { return !node.delete_pending; }).forEach(function(node) {
                    var option = element('option', '', node.name);
                    option.value = node.id;
                    option.selected = node.id === device.node_id;
                    select.appendChild(option);
                });
                binding.appendChild(select);
                row.appendChild(binding);
                var actions = element('td', 'pp-v2-actions');
                actions.appendChild(actionButton('保存绑定', function() {
                    var action = select.value ? 'bind' : 'unbind';
                    var params = { device_id: device.id, expected_revision: state.revision };
                    if (select.value) params.node_id = select.value;
                    mutate(action, params);
                }, !device.confirmed && !device.configured));
                row.appendChild(actions);
                body.appendChild(row);
            });
            pendingBindingRows(state.pendingBindings, state.nodes).forEach(function(pending) {
                var row = element('tr');
                row.appendChild(element('td', '', pending.state));
                row.appendChild(element('td', '', pending.ipv4));
                row.appendChild(element('td', '', pending.ipv4));
                row.appendChild(element('td', '', '-'));
                row.appendChild(element('td', '', pending.node_name));
                row.appendChild(element('td', 'pp-v2-state pp-v2-state-queued', pending.error_code || pending.state));
                body.appendChild(row);
            });
            if (!state.devices.length && !state.pendingBindings.length) {
                var empty = element('tr');
                var cell = element('td', 'pp-v2-empty', '暂未发现 LAN 或 WiFi 终端。');
                cell.colSpan = 6;
                empty.appendChild(cell);
                body.appendChild(empty);
            }
        }

        function renderJobs() {
            var target = document.getElementById('pp-v2-job-list');
            target.textContent = '';
            visibleJobs(state.jobs, state.trackedJobIds).slice(0, 20).forEach(function(job) {
                var card = element('div', 'pp-v2-job');
                card.appendChild(element('strong', '', job.kind + ' · ' + job.id));
                card.appendChild(element('span', 'pp-v2-job-summary', jobSummary(job)));
                target.appendChild(card);
            });
            if (!state.jobs.length) target.appendChild(element('div', 'pp-v2-empty', '暂无后台任务。'));
        }

        function render() {
            renderSummary();
            renderNodes();
            renderDevices();
            renderJobs();
        }

        function poll() {
            if (stopped) return Promise.resolve();
            var generation = ++pollGeneration;
            if (pollController) pollController.abort();
            pollController = typeof environment.AbortController === 'function' ? new environment.AbortController() : null;
            if (timer) environment.clearTimeout(timer);
            return Promise.all([
                apiCall('status', {}, false, pollController && pollController.signal),
                apiCall('devices', {}, false, pollController && pollController.signal),
                apiCall('jobs', {}, false, pollController && pollController.signal)
            ]).then(function(values) {
                if (stopped || generation !== pollGeneration) return;
                state = reduceState(state, { type: 'status.received', value: values[0] });
                state = reduceState(state, { type: 'devices.received', value: values[1] });
                state = reduceState(state, { type: 'jobs.received', value: values[2] });
                showError(null);
                render();
            }).catch(function(error) {
                if (generation === pollGeneration && (!error || error.name !== 'AbortError')) showError(error);
            }).then(function() {
                if (!stopped && generation === pollGeneration) timer = environment.setTimeout(poll, 3000);
            });
        }

        document.querySelectorAll('[data-pp-tab]').forEach(function(button) {
            button.addEventListener('click', function() {
                document.querySelectorAll('[data-pp-tab]').forEach(function(item) { item.classList.toggle('active', item === button); });
                document.querySelectorAll('[data-pp-panel]').forEach(function(panel) { panel.hidden = panel.getAttribute('data-pp-panel') !== button.getAttribute('data-pp-tab'); });
            });
        });
        document.getElementById('pp-v2-add-node').addEventListener('click', function() { openNodeEditor(null); });
        document.getElementById('pp-v2-node-cancel').addEventListener('click', function() { document.getElementById('pp-v2-node-modal').hidden = true; });
        document.getElementById('pp-v2-node-form').addEventListener('submit', function(event) {
            event.preventDefault();
            var form = event.currentTarget;
            var validation = validateNodeForm({
                node_id: form.elements.node_id.value,
                name: form.elements.name.value,
                protocol: 'l2tp',
                enabled: form.elements.enabled.checked,
                server: form.elements.server.value,
                port: form.elements.port.value,
                username: form.elements.username.value,
                password: form.elements.password.value,
                expires_at: form.elements.expires_at.value,
                has_username: form.getAttribute('data-has-username') === '1',
                has_password: form.getAttribute('data-has-password') === '1'
            }, state.revision);
            if (validation.errors.length) {
                showError({ code: validation.errors[0] === 'unsupported_protocol' ? 'unsupported' : 'invalid_request' });
                return;
            }
            document.getElementById('pp-v2-node-modal').hidden = true;
            mutate('node_save', validation.params);
        });
        document.getElementById('pp-v2-import-open').addEventListener('click', function() {
            importGeneration++;
            state = reduceState(state, { type: 'import.cleared' });
            document.getElementById('pp-v2-import-raw').value = '';
            document.getElementById('pp-v2-import-preview').disabled = false;
            document.getElementById('pp-v2-import-cancel').disabled = false;
            document.getElementById('pp-v2-import-raw').disabled = false;
            document.getElementById('pp-v2-import-protocol').disabled = false;
            renderImportPreview();
            document.getElementById('pp-v2-import-modal').hidden = false;
        });
        document.getElementById('pp-v2-import-cancel').addEventListener('click', function() {
            importGeneration++;
            document.getElementById('pp-v2-import-raw').value = '';
            state = reduceState(state, { type: 'import.cleared' });
            document.getElementById('pp-v2-import-modal').hidden = true;
        });
        function invalidateImportPreview() {
            importGeneration++;
            state = reduceState(state, { type: 'import.cleared' });
            document.getElementById('pp-v2-import-preview').disabled = false;
            renderImportPreview();
        }
        document.getElementById('pp-v2-import-raw').addEventListener('input', invalidateImportPreview);
        document.getElementById('pp-v2-import-protocol').addEventListener('change', invalidateImportPreview);
        document.getElementById('pp-v2-import-preview').addEventListener('click', function() {
            var rawInput = document.getElementById('pp-v2-import-raw');
            var raw = rawInput.value;
            if (!raw.trim()) {
                showError({ code: 'invalid_request' });
                return;
            }
            var button = document.getElementById('pp-v2-import-preview');
            var generation = ++importGeneration;
            var protocol = document.getElementById('pp-v2-import-protocol').value;
            var params = buildImportPreviewRequest(raw, state.revision, protocol);
            rawInput.value = '';
            button.disabled = true;
            document.getElementById('pp-v2-import-commit').disabled = true;
            showError(null);
            apiCall('import_preview', params, true).then(function(result) {
                if (generation !== importGeneration) return;
                state = reduceState(state, { type: 'import.preview.received', value: result });
                renderImportPreview();
            }).catch(function(error) {
                if (generation !== importGeneration) return;
                state = reduceState(state, { type: 'import.failed', error: error });
                renderImportPreview();
                showError(error);
            }).then(function() {
                params.raw = '';
                raw = '';
                if (generation === importGeneration) button.disabled = false;
            });
        });
        document.getElementById('pp-v2-import-commit').addEventListener('click', function() {
            if (!state.importPreview || !importPreviewModel(state.importPreview).can_commit) return;
            var button = document.getElementById('pp-v2-import-commit');
            var cancel = document.getElementById('pp-v2-import-cancel');
            var previewButton = document.getElementById('pp-v2-import-preview');
            var rawInput = document.getElementById('pp-v2-import-raw');
            var protocolInput = document.getElementById('pp-v2-import-protocol');
            var generation = ++importGeneration;
            var params = buildImportCommitRequest(state.importPreview);
            button.disabled = true;
            cancel.disabled = true;
            previewButton.disabled = true;
            rawInput.disabled = true;
            protocolInput.disabled = true;
            showError(null);
            apiCall('import_commit', params, true).then(function(result) {
                if (generation !== importGeneration) return;
                state = reduceState(state, { type: 'import.cleared' });
                document.getElementById('pp-v2-import-modal').hidden = true;
                return trackMutation(result, {});
            }).catch(function(error) {
                if (generation !== importGeneration) return;
                state = reduceState(state, { type: 'import.failed', error: error });
                renderImportPreview();
                showError(error);
                cancel.disabled = false;
                previewButton.disabled = false;
                rawInput.disabled = false;
                protocolInput.disabled = false;
                if (state.importPreview) button.disabled = !importPreviewModel(state.importPreview).can_commit;
            });
        });
        document.getElementById('pp-v2-export-safe').addEventListener('click', function() {
            if (!state.status) return;
            var blob = new environment.Blob([JSON.stringify(sanitizedExport(state.status), null, 2) + '\n'], { type: 'application/json' });
            if (environment.navigator && typeof environment.navigator.msSaveBlob === 'function') {
                environment.navigator.msSaveBlob(blob, 'proxypool-v2-sanitized.json');
                return;
            }
            var url = environment.URL.createObjectURL(blob);
            var link = document.createElement('a');
            link.href = url;
            link.download = 'proxypool-v2-sanitized.json';
            document.body.appendChild(link);
            link.click();
            document.body.removeChild(link);
            environment.setTimeout(function() { environment.URL.revokeObjectURL(url); }, 0);
        });
        environment.addEventListener('pagehide', function() {
            stopped = true;
            pollGeneration++;
            if (timer) environment.clearTimeout(timer);
            if (pollController) pollController.abort();
        });
        environment.addEventListener('pageshow', function(event) {
            if (!event.persisted) return;
            stopped = false;
            poll();
        });
        poll();
    }

    return {
        initialState: initialState,
        reduceState: reduceState,
        escapeText: escapeText,
        formatError: formatError,
        retryCountdown: retryCountdown,
        jobSummary: jobSummary,
        validateNodeForm: validateNodeForm,
        loadTrackedJobIDs: loadTrackedJobIDs,
        saveTrackedJobIDs: saveTrackedJobIDs,
        presentNodes: presentNodes,
        visibleJobs: visibleJobs,
        buildImportPreviewRequest: buildImportPreviewRequest,
        importPreviewModel: importPreviewModel,
        buildImportCommitRequest: buildImportCommitRequest,
        pendingBindingRows: pendingBindingRows,
        sanitizedExport: sanitizedExport,
        boot: boot
    };
});
