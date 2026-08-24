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
    var DIAGNOSTIC_JOB_KEY = 'proxypool.v2.diagnostic_job';
    var ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;
    var ARTIFACT_PATTERN = /^diag-[a-f0-9]{16,64}$/;
    var STATE_LABELS = Object.freeze({
        node: Object.freeze({
            disabled: '已停用', queued: '等待连接', starting: '正在启动', validating: '正在验证',
            online: '在线', degraded: '连接不稳定', stopping: '正在停止', failed: '失败',
            backoff: '等待重试', recovering: '正在恢复'
        }),
        job: Object.freeze({
            queued: '等待执行', running: '执行中', succeeded: '成功', failed: '失败',
            cancelled: '已取消', replaced: '已替换'
        }),
        diagnostic: Object.freeze({
            idle: '尚未生成', queued: '等待生成', running: '生成中', ready: '可下载',
            expired: '已过期', failed: '生成失败'
        })
    });
    var ERROR_LABELS = Object.freeze({
        invalid_request: '请求内容格式错误', internal: '内部服务错误', auth_failed: '节点认证失败',
        invalid_config: '配置未通过安全校验', unsupported: '当前版本不支持此协议', wan_down: '外网连接不可用',
        connect_timeout: '节点连接超时', stop_timeout: '节点停止超时', capacity_exceeded: '节点数量已达到上限（60）',
        revision_conflict: '配置已变化，请刷新页面后重试', duplicate: '操作重复，请刷新页面后重试',
        not_found: '目标不存在，请刷新页面', resolve_failed: '域名解析失败', probe_failed: '连通性检测失败',
        l2tp_interface_failed: 'L2TP 接口创建失败', l2tp_daemon_failed: 'L2TP 服务启动失败',
        l2tp_negotiation_failed: 'L2TP 协商失败', l2tp_server_no_response: 'L2TP 服务器无响应，请检查节点地址、端口或上游网络',
        l2tp_no_address: 'L2TP 未获得 IPv4 地址',
        dataplane_failed: '网络通道建立失败', dns_failed: 'DNS 检测失败', service_unavailable: 'ZeanLink 服务暂时不可用',
        bad_gateway: 'ZeanLink 服务响应异常', operation_timeout: '操作超时', unknown_method: '功能接口不可用',
        collection_failed: '诊断包生成失败', collection_cancelled: '诊断包生成已取消', unavailable: '信息不可用',
        invalid_protocol: '协议不受支持', invalid_fields: '字段数量或格式错误', invalid_server: '服务器地址格式错误',
        invalid_port: '端口格式错误', invalid_expiry: '到期日期格式错误', invalid_character: '内容包含不允许的字符',
        invalid_secret: '账号或密码格式错误', request_too_large: '导入内容超过大小限制', preview_not_found: '导入预览不存在',
        preview_mismatch: '导入预览校验失败', preview_blocked: '导入内容存在阻断错误', preview_expired: '导入预览已过期',
        preview_capacity: '待处理导入预览过多', device_capacity: '单个节点最多绑定 60 台设备'
    });
    var JOB_KIND_LABELS = Object.freeze({
        'system.recover': '系统恢复', 'pending.learn': '识别待绑定设备', 'node.connect': '连接节点',
        'node.reconnect': '重连节点', 'node.stop': '停止节点', 'node.save': '保存节点', 'node.delete': '删除节点',
        'device.bind': '绑定设备', 'device.unbind': '解除设备绑定', 'device.bindings.replace': '更新节点设备绑定',
        'import.commit': '批量导入节点', reconciliation: '状态同步', reconcile: '状态同步', import: '批量导入'
    });
    var JOB_STEP_LABELS = Object.freeze({
        queued: '等待执行', start: '正在启动', validate: '正在验证', done: '已完成', failed: '失败',
        cancelled: '已取消', replaced: '已替换', cleanup_failed: '清理失败', retry_scheduled: '已转入后台重试', blocked_by_previous_node: '等待前一节点处理',
        shadow_observed: '已确认停用'
    });

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
                copy.traffic = normalizeTraffic(observed && observed.traffic);
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

    function normalizedCounter(value) {
        value = Number(value);
        if (!Number.isFinite(value) || value <= 0) return 0;
        return Math.min(Number.MAX_SAFE_INTEGER, Math.floor(value));
    }

    function normalizeTraffic(traffic) {
        traffic = traffic || {};
        return {
            download_bytes: normalizedCounter(traffic.download_bytes),
            upload_bytes: normalizedCounter(traffic.upload_bytes),
            download_bytes_per_second: normalizedCounter(traffic.download_bytes_per_second),
            upload_bytes_per_second: normalizedCounter(traffic.upload_bytes_per_second),
            sampled_at: String(traffic.sampled_at || '')
        };
    }

    function formatBytes(value) {
        value = normalizedCounter(value);
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var unit = 0;
        while (value >= 1024 && unit < units.length - 1) {
            value /= 1024;
            unit++;
        }
        var rounded = Math.round(value * 10) / 10;
        return String(rounded) + ' ' + units[unit];
    }

    function formatRate(value) {
        return formatBytes(value) + '/s';
    }

    function stateLabel(kind, value) {
        var labels = STATE_LABELS[String(kind || '')];
        return labels && labels[String(value || '')] || '未知状态';
    }

    function errorLabel(code) {
        return ERROR_LABELS[String(code || '')] || '未知错误';
    }

    function jobKindLabel(kind) {
        return JOB_KIND_LABELS[String(kind || '')] || '未知任务';
    }

    function jobStepLabel(step) {
        return JOB_STEP_LABELS[String(step || '')] || '未知步骤';
    }

    function formatError(error) {
        return errorLabel(error && error.code);
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
            stateLabel('job', job.state),
            String(Number(job.succeeded || 0)) + '/' + String(Number(job.total || 0))
        ];
        (job.nodes || []).forEach(function(node) {
            if (node.error || node.state === 'failed') {
                parts.push(String(node.node_id || '?') + '：' + (node.error ? errorLabel(node.error.code) : stateLabel('node', 'failed')));
            }
        });
        return parts.join(' · ');
    }

    function validateNodeForm(form, revision) {
        form = form || {};
        var errors = [];
        var protocol = String(form.protocol || '');
        var name = String(form.name || '').trim();
        var note = String(form.note || '').trim();
        var server = String(form.server || '').trim();
        var port = Number(form.port);
        var nodeID = String(form.node_id || '');
        var username = String(form.username || '');
        var password = String(form.password || '');
        var expiresAt = String(form.expires_at || '');
        var creating = nodeID === '';

        if (protocol !== 'l2tp') errors.push('unsupported_protocol');
        if (!name || name.length > 128) errors.push('invalid_name');
        if (Array.from(note).length > 200 || /[\u0000-\u001F\u007F-\u009F]/.test(note)) errors.push('invalid_note');
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
                note: note,
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

    function deviceSortKey(device) {
        var name = String(device.hostname || device.display_name || '未命名设备').toLowerCase();
        var address = String(device.current_ipv4 || device.fixed_ipv4 || device.lan_ipv4 || '');
        return name + '\u0000' + address + '\u0000' + String(device.mac || '').toLowerCase() + '\u0000' + String(device.id || '');
    }

    function compareDevices(left, right) {
        var leftKey = deviceSortKey(left);
        var rightKey = deviceSortKey(right);
        return leftKey < rightKey ? -1 : (leftKey > rightKey ? 1 : 0);
    }

    function boundDevicesByNode(nodes, devices) {
        var grouped = {};
        (nodes || []).forEach(function(node) { grouped[node.id] = []; });
        (devices || []).forEach(function(device) {
            if (!device.enabled || !device.node_id) return;
            if (!grouped[device.node_id]) grouped[device.node_id] = [];
            grouped[device.node_id].push({
                id: String(device.id || ''),
                display_name: String(device.hostname || '未命名设备'),
                lan_ipv4: String(device.current_ipv4 || device.fixed_ipv4 || ''),
                fixed_ipv4: String(device.fixed_ipv4 || ''),
                mac: String(device.mac || ''),
                ingress: String(device.ingress || '')
            });
        });
        Object.keys(grouped).forEach(function(nodeID) { grouped[nodeID].sort(compareDevices); });
        return grouped;
    }

    function normalizedSearch(value) {
        return String(value || '').trim().toLowerCase();
    }

    function normalizedMAC(value) {
        return normalizedSearch(value).replace(/[^a-f0-9]/g, '');
    }

    function deviceBindingRows(devices, nodes, targetNodeID, query) {
        targetNodeID = String(targetNodeID || '');
        var names = {};
        (nodes || []).forEach(function(node) { names[node.id] = String(node.name || node.id || ''); });
        var targetName = names[targetNodeID] || targetNodeID;
        var needle = normalizedSearch(query);
        var compactNeedle = /^[\s0-9a-f:-]+$/i.test(String(query || '')) ? normalizedMAC(query) : '';
        return (devices || []).map(function(device) {
            var nodeID = device.enabled ? String(device.node_id || '') : '';
            var nodeName = names[nodeID] || nodeID;
            var current = nodeID === targetNodeID && !!targetNodeID;
            var row = {
                id: String(device.id || ''),
                device_name: String(device.hostname || '未命名设备'),
                current_ipv4: String(device.current_ipv4 || ''),
                fixed_ipv4: String(device.fixed_ipv4 || ''),
                mac: String(device.mac || ''),
                ingress: String(device.ingress || ''),
                node_id: nodeID,
                node_name: nodeName,
                target_node_id: targetNodeID,
                target_node_name: targetName,
                was_selected: current,
                confirmed: !!device.confirmed,
                selectable: current || !!device.confirmed,
                ownership_label: current ? '已绑定当前节点' : (nodeID ? '已绑定：' + nodeName : '未绑定')
            };
            return row;
        }).filter(function(row) {
            if (!needle) return true;
            var fields = [row.device_name, row.current_ipv4, row.fixed_ipv4, row.mac, row.ingress, row.node_name, row.ownership_label];
            var ordinary = fields.some(function(value) { return normalizedSearch(value).indexOf(needle) !== -1; });
            return ordinary || (!!compactNeedle && normalizedMAC(row.mac).indexOf(compactNeedle) !== -1);
        }).sort(compareDevices);
    }

    function buildBindingReplacement(originalRows, selectedIDs, revision) {
        var rows = originalRows || [];
        var byID = {};
        rows.forEach(function(row) { if (ID_PATTERN.test(String(row.id || ''))) byID[row.id] = row; });
        var selected = [];
        var selectedSet = {};
        (selectedIDs || []).forEach(function(id) {
            id = String(id || '');
            if (!byID[id] || selectedSet[id]) return;
            selectedSet[id] = true;
            selected.push(id);
        });
        selected.sort();
        var original = rows.filter(function(row) { return row.was_selected; }).map(function(row) { return row.id; }).sort();
        var migrations = rows.filter(function(row) {
            return selectedSet[row.id] && row.node_id && row.node_id !== row.target_node_id;
        }).map(function(row) {
            return {
                device_id: row.id,
                device_name: row.device_name,
                from_node: row.node_name,
                to_node: row.target_node_name
            };
        }).sort(function(left, right) { return left.device_id < right.device_id ? -1 : (left.device_id > right.device_id ? 1 : 0); });
        return {
            changed: original.join('\u0000') !== selected.join('\u0000'),
            node_id: rows.length ? String(rows[0].target_node_id || '') : '',
            device_ids: selected,
            migrations: migrations,
            expected_revision: Number(revision || 0)
        };
    }

    function presentNodes(nodes, devices, nowMilliseconds) {
        var grouped = boundDevicesByNode(nodes, devices);
        return (nodes || []).map(function(node) {
            var copy = Object.assign({}, node);
            copy.bound_devices = grouped[node.id] || [];
            copy.bound_count = copy.bound_devices.length;
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
                    action: row.action === 'add' ? '新增' : (row.action === 'skip' ? '跳过' : '未知操作'),
                    protocol: String(row.protocol || ''),
                    endpoint: String(row.server || '') + ':' + String(row.port || ''),
                    expires_at: String(row.expires_at || ''),
                    secret_label: row.secret_set ? '已设置（不显示）' : '未设置'
                };
            }),
            errors: (preview.errors || []).map(function(error) {
                return { line: Number(error.line || 0), message: errorLabel(error.code) };
            })
        };
    }

    function ingressLabel(value) {
        value = String(value || '').toLowerCase();
        if (!value) return '-';
        if (value.indexOf('wifi') !== -1 || value.indexOf('wlan') !== -1 || value.indexOf('wireless') !== -1) return 'WiFi';
        if (value.indexOf('lan') !== -1 || value.indexOf('eth') !== -1) return 'LAN';
        return '未知接入';
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
                        id: node.id, name: node.name, note: node.note || '', protocol: node.protocol, enabled: !!node.enabled,
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

    function diagnosticViewModel(status, nowMilliseconds) {
        status = status || {};
        var artifact = status.artifact || {};
        var artifactID = String(artifact.artifact_id || '');
        var expiresAt = String(artifact.expires_at || '');
        var expires = Date.parse(expiresAt);
        // The router is the expiry authority and diagnostics.claim enforces the
        // TTL on the router. A newly flashed router can temporarily have a
        // factory wall clock, so comparing this timestamp with the browser
        // clock would incorrectly hide a freshly generated download.
        void nowMilliseconds;
        var ready = status.state === 'ready' && ARTIFACT_PATTERN.test(artifactID) && Number.isFinite(expires);
        return {
            state: status.state === 'ready' && !ready ? 'expired' : String(status.state || 'idle'),
            error_code: String(status.error_code || ''),
            artifact_id: ready ? artifactID : '',
            filename: ready ? String(artifact.filename || '') : '',
            size: ready ? Number(artifact.size || 0) : 0,
            expires_at: ready ? expiresAt : '',
            can_download: ready
        };
    }

    function boot(document, environment) {
        var app = document.getElementById('pp-v2-app');
        if (!app || app.getAttribute('data-booted') === '1') return;
        app.setAttribute('data-booted', '1');
        var apiRead = app.getAttribute('data-api-read');
        var apiWrite = app.getAttribute('data-api-write');
        var diagnosticsDownload = app.getAttribute('data-diagnostics-download');
        var token = app.getAttribute('data-token');
        var storage = environment.sessionStorage;
        var state = initialState();
        state.trackedJobIds = loadTrackedJobIDs(storage);
        var timer = null;
        var pollController = null;
        var pollGeneration = 0;
        var importGeneration = 0;
        var stopped = false;
        var diagnosticJobId = '';
        var diagnosticStatus = null;
        var bindingNodeID = '';
        var bindingDevices = [];
        var bindingNodes = [];
        var bindingRows = [];
        var bindingSelected = {};
        var bindingRevision = 0;
        var bindingSaving = false;
        try {
            diagnosticJobId = String(storage && storage.getItem(DIAGNOSTIC_JOB_KEY) || '');
            if (!ID_PATTERN.test(diagnosticJobId)) diagnosticJobId = '';
        } catch (_) { diagnosticJobId = ''; }

        function saveDiagnosticJob(id) {
            diagnosticJobId = ID_PATTERN.test(String(id || '')) ? String(id) : '';
            try {
                if (storage && diagnosticJobId) storage.setItem(DIAGNOSTIC_JOB_KEY, diagnosticJobId);
                else if (storage && typeof storage.removeItem === 'function') storage.removeItem(DIAGNOSTIC_JOB_KEY);
            } catch (_) {}
        }

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
                    (error.line ? '第 ' + error.line + ' 行 · ' : '') + error.message));
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

        function selectedBindingIDs() {
            return Object.keys(bindingSelected).filter(function(id) { return bindingSelected[id]; }).sort();
        }

        function renderBindingEditor() {
            if (!bindingNodeID) return;
            var search = document.getElementById('pp-v2-binding-search');
            var list = document.getElementById('pp-v2-binding-list');
            var selectedIDs = selectedBindingIDs();
            var visible = deviceBindingRows(bindingDevices, bindingNodes, bindingNodeID, search.value);
            var replacement = buildBindingReplacement(bindingRows, selectedIDs, bindingRevision);
            list.textContent = '';
            visible.forEach(function(row) {
                var label = element('label', 'pp-v2-binding-row');
                var checkbox = element('input');
                checkbox.type = 'checkbox';
                checkbox.checked = !!bindingSelected[row.id];
                checkbox.disabled = bindingSaving || !row.selectable;
                checkbox.addEventListener('change', function() {
                    if (checkbox.checked && selectedBindingIDs().length >= 60) {
                        checkbox.checked = false;
                        showError({ code: 'device_capacity' });
                        return;
                    }
                    bindingSelected[row.id] = checkbox.checked;
                    showError(null);
                    renderBindingEditor();
                });
                label.appendChild(checkbox);
                label.appendChild(element('strong', '', row.device_name));
                label.appendChild(element('span', '', '当前 IP：' + (row.current_ipv4 || '-') + '；固定 IP：' + (row.fixed_ipv4 || '-')));
                label.appendChild(element('small', '', (row.mac || '-') + ' · ' + ingressLabel(row.ingress) + (row.selectable ? '' : ' · 当前不可选择')));
                label.appendChild(element('span', row.was_selected ? 'pp-v2-binding-owner-current' : (row.node_id ? 'pp-v2-binding-owner-other' : ''), row.ownership_label));
                list.appendChild(label);
            });
            if (!visible.length) list.appendChild(element('div', 'pp-v2-empty', '没有符合条件的设备。'));
            document.getElementById('pp-v2-binding-count').textContent = '已选择 ' + selectedIDs.length + ' 台；当前显示 ' + visible.length + ' 台。';
            var migrations = document.getElementById('pp-v2-binding-migrations');
            migrations.textContent = '';
            migrations.hidden = !replacement.migrations.length;
            if (replacement.migrations.length) {
                migrations.appendChild(element('strong', '', '以下设备将迁移到当前节点：'));
                replacement.migrations.forEach(function(item) {
                    migrations.appendChild(element('div', '', item.device_name + '：' + item.from_node + ' → ' + item.to_node));
                });
            }
            search.disabled = bindingSaving;
            document.getElementById('pp-v2-binding-cancel').disabled = bindingSaving;
            document.getElementById('pp-v2-binding-save').disabled = bindingSaving || !replacement.changed;
        }

        function openBindingEditor(node) {
            bindingNodeID = node.id;
            bindingRevision = state.revision;
            bindingSaving = false;
            bindingDevices = state.devices.slice();
            bindingNodes = state.nodes.slice();
            bindingRows = deviceBindingRows(bindingDevices, bindingNodes, node.id, '');
            bindingSelected = {};
            bindingRows.forEach(function(row) { if (row.was_selected) bindingSelected[row.id] = true; });
            document.getElementById('pp-v2-binding-title').textContent = '绑定设备 · ' + node.name;
            document.getElementById('pp-v2-binding-context').textContent = '勾选表示绑定到该节点；取消当前设备表示解除绑定；勾选其他节点设备表示迁移。';
            document.getElementById('pp-v2-binding-search').value = '';
            document.getElementById('pp-v2-binding-modal').hidden = false;
            renderBindingEditor();
        }

        function closeBindingEditor() {
            if (bindingSaving) return;
            document.getElementById('pp-v2-binding-modal').hidden = true;
            bindingNodeID = '';
            bindingDevices = [];
            bindingNodes = [];
            bindingRows = [];
            bindingSelected = {};
            bindingRevision = 0;
        }

        function renderSummary() {
            var online = state.nodes.filter(function(node) { return node.state === 'online'; }).length;
            var bound = state.devices.filter(function(device) { return device.enabled && device.node_id; }).length;
            document.getElementById('pp-v2-total').textContent = String(state.nodes.length);
            document.getElementById('pp-v2-online').textContent = String(online);
            document.getElementById('pp-v2-bound').textContent = String(bound);
            document.getElementById('pp-v2-revision').textContent = state.revision ? '配置版本 ' + state.revision : '';
        }

        function openNodeEditor(node) {
            var modal = document.getElementById('pp-v2-node-modal');
            var form = document.getElementById('pp-v2-node-form');
            form.reset();
            form.elements.node_id.value = node && node.id || '';
            form.elements.name.value = node && node.name || '';
            form.elements.note.value = node && node.note || '';
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
                var nameCell = element('td', 'pp-v2-node-name');
                nameCell.appendChild(element('strong', '', node.name));
                if (node.note) nameCell.appendChild(element('span', 'pp-v2-node-note', node.note));
                row.appendChild(nameCell);
                row.appendChild(element('td', '', node.server + ':' + node.port));
                row.appendChild(element('td', '', node.protocol.toUpperCase()));
                row.appendChild(element('td', 'pp-v2-state pp-v2-state-' + node.state, node.delete_pending ? '等待删除' : stateLabel('node', node.state)));
                row.appendChild(element('td', '', node.error_code ? errorLabel(node.error_code) : '-'));
                row.appendChild(element('td', '', node.retry_seconds == null ? (node.attempts ? '已尝试 ' + node.attempts + ' 次' : '-') : node.retry_seconds + ' 秒'));
                var totals = element('td', 'pp-v2-traffic-cell');
                totals.appendChild(element('span', '', '下行 ' + formatBytes(node.traffic.download_bytes)));
                totals.appendChild(element('span', '', '上行 ' + formatBytes(node.traffic.upload_bytes)));
                row.appendChild(totals);
                var rates = element('td', 'pp-v2-traffic-cell');
                rates.appendChild(element('span', '', '下行 ' + formatRate(node.traffic.download_bytes_per_second)));
                rates.appendChild(element('span', '', '上行 ' + formatRate(node.traffic.upload_bytes_per_second)));
                row.appendChild(rates);
                var boundCell = element('td', 'pp-v2-bound-cell');
                node.bound_devices.forEach(function(device) {
                    var item = element('div', 'pp-v2-bound-device');
                    item.appendChild(element('strong', '', device.display_name));
                    item.appendChild(element('span', '', device.lan_ipv4 || '-'));
                    item.appendChild(element('small', '', device.mac || '-'));
                    boundCell.appendChild(item);
                });
                if (!node.bound_devices.length) boundCell.appendChild(element('span', '', '未绑定设备'));
                row.appendChild(boundCell);
                row.appendChild(element('td', '', node.policy_id));
                var actions = element('td', 'pp-v2-actions');
                actions.appendChild(actionButton('绑定设备', function() { openBindingEditor(node); }, node.delete_pending));
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
                cell.colSpan = 11;
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
                row.appendChild(element('td', '', ingressLabel(device.ingress)));
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
                row.appendChild(element('td', 'pp-v2-state pp-v2-state-queued', pending.error_code ? errorLabel(pending.error_code) : pending.state));
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
                card.appendChild(element('strong', '', jobKindLabel(job.kind) + ' · ' + job.id));
                card.appendChild(element('span', 'pp-v2-job-summary', jobSummary(job)));
                target.appendChild(card);
            });
            if (!state.jobs.length) target.appendChild(element('div', 'pp-v2-empty', '暂无后台任务。'));
        }

        function renderDiagnostics() {
            var target = document.getElementById('pp-v2-diagnostics-status');
            var create = document.getElementById('pp-v2-diagnostics-create');
            target.textContent = '';
            var model = diagnosticViewModel(diagnosticStatus);
            create.disabled = model.state === 'queued' || model.state === 'running';
            if (model.state === 'idle') {
                target.appendChild(element('span', '', '尚未生成诊断包。诊断内容会自动脱敏并在 15 分钟后过期。'));
                return;
            }
            target.appendChild(element('strong', '', '诊断包：' + stateLabel('diagnostic', model.state)));
            if (model.error_code) target.appendChild(element('span', 'pp-v2-job-summary', errorLabel(model.error_code)));
            if (model.can_download) {
                var detail = model.filename + ' · ' + model.size + ' 字节';
                target.appendChild(element('span', 'pp-v2-job-summary', detail));
                target.appendChild(actionButton('下载一次', function() {
                    var form = document.getElementById('pp-v2-diagnostics-download-form');
                    document.getElementById('pp-v2-diagnostics-artifact').value = model.artifact_id;
                    saveDiagnosticJob('');
                    diagnosticStatus = null;
                    form.submit();
                    renderDiagnostics();
                }));
            }
        }

        function render() {
            renderSummary();
            renderNodes();
            renderDevices();
            renderJobs();
            renderDiagnostics();
        }

        function poll() {
            if (stopped) return Promise.resolve();
            var generation = ++pollGeneration;
            if (pollController) pollController.abort();
            pollController = typeof environment.AbortController === 'function' ? new environment.AbortController() : null;
            if (timer) environment.clearTimeout(timer);
            var calls = [
                apiCall('status', {}, false, pollController && pollController.signal),
                apiCall('devices', {}, false, pollController && pollController.signal),
                apiCall('jobs', {}, false, pollController && pollController.signal)
            ];
            if (diagnosticJobId) {
                calls.push(apiCall('diagnostics', { job_id: diagnosticJobId }, false, pollController && pollController.signal)
                    .catch(function(error) { return { diagnostic_error: error || { code: 'bad_gateway' } }; }));
            }
            return Promise.all(calls).then(function(values) {
                if (stopped || generation !== pollGeneration) return;
                state = reduceState(state, { type: 'status.received', value: values[0] });
                state = reduceState(state, { type: 'devices.received', value: values[1] });
                state = reduceState(state, { type: 'jobs.received', value: values[2] });
                if (values[3]) {
                    if (values[3].diagnostic_error) {
                        if (values[3].diagnostic_error.code === 'not_found') {
                            saveDiagnosticJob('');
                            diagnosticStatus = null;
                        }
                    } else {
                        diagnosticStatus = values[3];
                        if (diagnosticStatus.state === 'failed' || diagnosticViewModel(diagnosticStatus).state === 'expired') saveDiagnosticJob('');
                    }
                }
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
                note: form.elements.note.value,
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
        document.getElementById('pp-v2-binding-search').addEventListener('input', renderBindingEditor);
        document.getElementById('pp-v2-binding-cancel').addEventListener('click', closeBindingEditor);
        document.getElementById('pp-v2-binding-save').addEventListener('click', function() {
            if (!bindingNodeID || bindingSaving) return;
            var replacement = buildBindingReplacement(bindingRows, selectedBindingIDs(), bindingRevision);
            if (!replacement.changed) {
                closeBindingEditor();
                return;
            }
            var migrationLines = replacement.migrations.map(function(item) { return item.device_name + '：' + item.from_node + ' → ' + item.to_node; });
            if (migrationLines.length && !environment.confirm('确认迁移以下设备：\n' + migrationLines.join('\n') + '\n迁移时会先从原节点移除，再绑定到当前节点。')) return;
            bindingSaving = true;
            renderBindingEditor();
            showError(null);
            apiCall('bindings_replace', {
                node_id: bindingNodeID,
                device_ids_json: JSON.stringify(replacement.device_ids),
                expected_revision: bindingRevision
            }, true).then(function(result) {
                var targetNodeID = bindingNodeID;
                bindingSaving = false;
                closeBindingEditor();
                return trackMutation(result, { node_id: targetNodeID });
            }).catch(function(error) {
                bindingSaving = false;
                renderBindingEditor();
                showError(error);
            });
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
        document.getElementById('pp-v2-diagnostics-download-form').action = diagnosticsDownload;
        document.getElementById('pp-v2-diagnostics-create').addEventListener('click', function() {
            showError(null);
            diagnosticStatus = { state: 'queued' };
            renderDiagnostics();
            apiCall('diagnostics_create', {}, true).then(function(result) {
                diagnosticStatus = result;
                saveDiagnosticJob(result && result.job_id);
                renderDiagnostics();
                return poll();
            }).catch(function(error) {
                diagnosticStatus = { state: 'failed', error_code: String(error && error.code || 'collection_failed') };
                saveDiagnosticJob('');
                renderDiagnostics();
                showError(error);
            });
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
        stateLabel: stateLabel,
        errorLabel: errorLabel,
        jobKindLabel: jobKindLabel,
        jobStepLabel: jobStepLabel,
        formatError: formatError,
        formatBytes: formatBytes,
        formatRate: formatRate,
        retryCountdown: retryCountdown,
        jobSummary: jobSummary,
        validateNodeForm: validateNodeForm,
        loadTrackedJobIDs: loadTrackedJobIDs,
        saveTrackedJobIDs: saveTrackedJobIDs,
        boundDevicesByNode: boundDevicesByNode,
        deviceBindingRows: deviceBindingRows,
        buildBindingReplacement: buildBindingReplacement,
        presentNodes: presentNodes,
        visibleJobs: visibleJobs,
        buildImportPreviewRequest: buildImportPreviewRequest,
        importPreviewModel: importPreviewModel,
        buildImportCommitRequest: buildImportCommitRequest,
        pendingBindingRows: pendingBindingRows,
        sanitizedExport: sanitizedExport,
        diagnosticViewModel: diagnosticViewModel,
        boot: boot
    };
});
