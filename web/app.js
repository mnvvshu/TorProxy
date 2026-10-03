/* ============================================================
   TorProxyManager — Dashboard client
   Vanilla JS, no dependencies. Talks to the local REST API and
   receives live updates over Server-Sent Events (/api/events).
   ============================================================ */
'use strict';

(() => {
    // ---------- State ----------
    const S = {
        status: null,
        version: '',
        instances: new Map(),      // id -> status
        filter: 'all',
        search: '',
        sortKey: 'id',
        sortAsc: true,
        view: localStorage.getItem('tpm.view') || 'table',
        tab: 'endpoints',
        logs: [],
        logLevel: 'ALL',
        logSearch: '',
        lastLogSeq: 0,
        unseenLogs: 0,
        drawerId: null,
        config: null,
        browsers: [],
        connected: false,
        exited: false,
    };
    const MAX_LOGS = 3000;
    const rowCache = new Map();    // id -> { tr, sig }
    let lastOrder = '';
    let renderQueued = false;
    let es = null;

    // ---------- Helpers ----------
    const $ = (id) => document.getElementById(id);
    const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
    const fmtNum = (n) => Number(n || 0).toLocaleString();
    const fmtMB = (mb) => mb >= 1024 ? `${(mb / 1024).toFixed(2)} GB` : `${Math.round(mb || 0)} MB`;
    const fmtMs = (ms) => !ms ? '—' : ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
    const fmtDuration = (sec) => {
        sec = Math.floor(sec || 0);
        if (sec <= 0) return '—';
        const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
        if (d) return `${d}d ${h}h`;
        if (h) return `${h}h ${m}m`;
        if (m) return `${m}m ${s}s`;
        return `${s}s`;
    };
    const fmtTime = (iso) => {
        if (!iso) return '—';
        const d = new Date(iso);
        return isNaN(d) ? '—' : d.toLocaleTimeString();
    };
    const ago = (iso) => {
        if (!iso) return '—';
        const s = (Date.now() - new Date(iso).getTime()) / 1000;
        if (s < 5) return 'just now';
        if (s < 60) return `${Math.floor(s)}s ago`;
        if (s < 3600) return `${Math.floor(s / 60)}m ago`;
        return `${Math.floor(s / 3600)}h ago`;
    };
    const cap = (s) => s ? s.charAt(0).toUpperCase() + s.slice(1) : '';

    const ICON = {
        copy: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg>',
        test: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><polyline points="9 12 11 14 15 10"/></svg>',
        newnym: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 2v6h-6"/><path d="M3 12a9 9 0 0115-6.7L21 8"/><path d="M3 22v-6h6"/><path d="M21 12a9 9 0 01-15 6.7L3 16"/></svg>',
        restart: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 4v6h6"/><path d="M3.51 15a9 9 0 102.13-9.36L1 10"/></svg>',
        stop: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="6" y="6" width="12" height="12" rx="2"/></svg>',
        play: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="5 3 19 12 5 21 5 3"/></svg>',
        globe: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 014 10 15.3 15.3 0 01-4 10 15.3 15.3 0 01-4-10 15.3 15.3 0 014-10z"/></svg>',
        ok: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><polyline points="20 6 9 17 4 12"/></svg>',
        err: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="12" cy="12" r="10"/><path d="M15 9l-6 6m0-6l6 6"/></svg>',
        info: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></svg>',
        warn: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/></svg>',
    };

    // ---------- API ----------
    async function api(path, { method = 'GET', body, raw = false } = {}) {
        const opts = { method, headers: {} };
        if (method !== 'GET') opts.headers['X-TPM-Request'] = '1';
        if (body !== undefined) {
            opts.headers['Content-Type'] = 'application/json';
            opts.body = JSON.stringify(body);
        }
        const res = await fetch(path, opts);
        if (raw) {
            if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
            return res.text();
        }
        let data = null;
        try { data = await res.json(); } catch { /* empty body */ }
        if (!res.ok) {
            const err = new Error((data && (data.error || (data.errors && data.errors[0]))) || `${res.status} ${res.statusText}`);
            err.errors = data && data.errors;
            err.status = res.status;
            throw err;
        }
        return data;
    }

    // ---------- Toasts ----------
    function toast(message, type = 'info', timeout = 4500) {
        const el = document.createElement('div');
        el.className = `toast ${type}`;
        el.innerHTML = `<span class="toast-icon">${ICON[type === 'success' ? 'ok' : type === 'error' ? 'err' : type === 'warning' ? 'warn' : 'info'] || ''}</span><span>${esc(message)}</span>`;
        $('toast-container').appendChild(el);
        setTimeout(() => {
            el.style.transition = 'opacity 300ms, transform 300ms';
            el.style.opacity = '0';
            el.style.transform = 'translateX(20px)';
            setTimeout(() => el.remove(), 320);
        }, timeout);
    }

    // ---------- Confirm ----------
    function confirmDialog(title, text, okLabel = 'Confirm') {
        return new Promise((resolve) => {
            $('confirm-title').textContent = title;
            $('confirm-text').textContent = text;
            $('confirm-ok').textContent = okLabel;
            const modal = $('confirm-modal');
            modal.classList.remove('hidden');
            const done = (v) => {
                modal.classList.add('hidden');
                $('confirm-ok').onclick = $('confirm-cancel').onclick = null;
                resolve(v);
            };
            $('confirm-ok').onclick = () => done(true);
            $('confirm-cancel').onclick = () => done(false);
            $('confirm-ok').focus();
        });
    }

    async function withLoading(btn, fn) {
        if (btn) btn.classList.add('loading');
        try { return await fn(); } finally { if (btn) btn.classList.remove('loading'); }
    }

    // ---------- Live connection (SSE) ----------
    function connect() {
        if (es) es.close();
        es = new EventSource('/api/events');
        es.onopen = () => setConnected(true);
        es.onerror = () => { setConnected(false); };
        es.onmessage = (ev) => {
            let msg;
            try { msg = JSON.parse(ev.data); } catch { return; }
            switch (msg.type) {
                case 'snapshot':
                    S.version = msg.version;
                    S.instances.clear();
                    (msg.instances || []).forEach((i) => S.instances.set(i.instance_id, i));
                    rowCache.clear();
                    $('instances-tbody').innerHTML = '';
                    lastOrder = '';
                    applyStatus(msg.status);
                    break;
                case 'status':
                    applyStatus(msg.status);
                    break;
                case 'instances':
                    (msg.instances || []).forEach((i) => S.instances.set(i.instance_id, i));
                    queueRender();
                    break;
                case 'log':
                    addLog(msg.entry);
                    break;
                case 'event':
                    handleEvent(msg.event);
                    break;
            }
        };
    }

    function setConnected(on) {
        S.connected = on;
        $('conn-indicator').classList.toggle('live', on);
        $('conn-indicator').title = on ? 'Live connection established' : 'Disconnected — retrying…';
        if (!on && !S.exited) {
            const badge = $('manager-state-badge');
            badge.className = 'status-badge disconnected';
            $('manager-state-text').textContent = 'Disconnected';
        }
    }

    function handleEvent(evt) {
        if (!evt) return;
        switch (evt.type) {
            case 'started':
                toast(evt.message, (evt.data && evt.data.failed > 0) ? 'warning' : 'success', 6000);
                refreshInstances();
                break;
            case 'stopped': toast(evt.message, 'info'); break;
            case 'error': toast(evt.message, 'error', 8000); break;
            case 'info': toast(evt.message, 'info'); break;
        }
    }

    // ---------- Status ----------
    function applyStatus(st) {
        if (!st) return;
        S.status = st;
        queueRender();
    }

    async function refreshAll() {
        try {
            const [st, inst] = await Promise.all([api('/api/status'), api('/api/instances')]);
            S.version = st.version;
            S.instances.clear();
            (inst || []).forEach((i) => S.instances.set(i.instance_id, i));
            applyStatus(st.status);
        } catch (e) {
            setConnected(false);
        }
    }

    async function refreshInstances() {
        try {
            const inst = await api('/api/instances');
            (inst || []).forEach((i) => S.instances.set(i.instance_id, i));
            queueRender();
        } catch { /* ignore */ }
    }

    // ---------- Rendering ----------
    function queueRender() {
        if (renderQueued) return;
        renderQueued = true;
        requestAnimationFrame(() => {
            renderQueued = false;
            render();
        });
    }

    function render() {
        renderHeader();
        renderStats();
        renderProgress();
        renderButtons();
        renderAlert();
        if (S.view === 'table') renderTable(); else renderHeatmap();
        renderDrawer();
    }

    function renderHeader() {
        const st = S.status;
        $('app-version').textContent = 'v' + (S.version || '1.0.0');
        if (!st) return;
        $('tor-version').textContent = st.tor_version ? 'Tor ' + st.tor_version.split(' ')[0] : 'Tor —';
        $('footer-tor-path').textContent = st.tor_path ? st.tor_path : '';
        if (S.connected) {
            const state = st.manager_state;
            const badge = $('manager-state-badge');
            badge.className = 'status-badge ' + state;
            let label = cap(state);
            if (state === 'running') label = `Running · ${st.running_ports}/${st.total_ports} ports`;
            if (state === 'starting') label = `Starting · ${st.start_progress}/${st.start_total}`;
            $('manager-state-text').textContent = label;
        }
        document.title = st.manager_state === 'running'
            ? `(${st.running_ports}/${st.total_ports}) TorProxyManager`
            : 'TorProxyManager — Dashboard';
    }

    function renderStats() {
        const st = S.status;
        if (!st) return;
        $('running-count').textContent = fmtNum(st.running_ports);
        $('healthy-count').textContent = fmtNum(st.healthy_instances);
        $('starting-count').textContent = fmtNum((st.starting_instances || 0) + (st.bootstrapping_instances || 0));
        $('failed-count').textContent = fmtNum(st.failed_instances);
        $('total-count').textContent = `${fmtNum(st.total_ports)} ports / ${fmtNum(st.total_instances)} tor`;
        $('port-range').textContent = `${st.port_range_start}–${st.port_range_end}`;
        $('memory-total').textContent = fmtMB(st.total_memory_mb);
        $('cpu-total').textContent = `${(st.total_cpu_percent || 0).toFixed(1)}%`;
        $('uptime').textContent = fmtDuration(st.uptime_seconds);
        $('avg-startup').textContent = fmtMs(st.avg_startup_ms);
        const pct = st.total_ports ? (st.running_ports / st.total_ports) * 100 : 0;
        $('running-ring').style.setProperty('--pct', pct.toFixed(1));
        $('tab-endpoints-count').textContent = fmtNum(S.instances.size || st.total_instances);
    }

    function renderProgress() {
        const st = S.status;
        const sec = $('progress-section');
        if (!st || st.manager_state !== 'starting') { sec.classList.add('hidden'); return; }
        sec.classList.remove('hidden');
        const pct = st.start_total ? (st.start_progress / st.start_total) * 100 : 0;
        $('progress-fill').style.width = pct.toFixed(1) + '%';
        $('progress-text').textContent = `${st.start_progress} / ${st.start_total}`;
        const boot = st.bootstrapping_instances || 0;
        $('progress-label').textContent = st.start_progress === 0 && boot <= 1
            ? 'Bootstrapping seed instance and downloading the Tor consensus…'
            : `Starting endpoints · ${boot} bootstrapping · ${st.running_instances} ready · ${st.failed_instances} failed`;
    }

    function renderButtons() {
        const st = S.status;
        const state = st ? st.manager_state : 'stopped';
        const busy = state === 'starting' || state === 'stopping';
        $('btn-start').disabled = !S.connected || state !== 'stopped';
        $('btn-stop').disabled = !S.connected || state === 'stopped' || state === 'stopping';
        $('btn-restart-failed').disabled = !S.connected || state !== 'running' || !(st && st.failed_instances > 0);
        $('btn-newnym-all').disabled = !S.connected || !(st && st.running_instances > 0);
        $('btn-test-all').disabled = !S.connected || !(st && st.running_instances > 0) || busy;
        $('btn-copy-proxies').disabled = !S.connected;
    }

    function renderAlert() {
        const st = S.status;
        const el = $('alert-banner');
        const action = $('alert-action');
        if (!st) { el.classList.add('hidden'); return; }
        if (st.manager_state === 'stopped' && st.last_error) {
            el.className = 'alert error';
            $('alert-text').textContent = st.last_error;
            action.textContent = 'Open settings';
            action.dataset.target = 'settings';
            action.classList.remove('hidden');
        } else if (st.config_pending_restart) {
            el.className = 'alert';
            $('alert-text').textContent = 'Configuration changed — restart all endpoints to apply the new settings.';
            action.textContent = 'Restart now';
            action.dataset.target = 'restart-all';
            action.classList.remove('hidden');
        } else {
            el.className = 'alert hidden';
        }
    }

    // --- table ---
    const STATE_ORDER = { failed: 0, starting: 1, bootstrapping: 2, stopping: 3, running: 4, stopped: 5 };

    function visibleInstances() {
        const q = S.search.trim().toLowerCase();
        let list = [...S.instances.values()];
        if (S.filter !== 'all') {
            list = list.filter((i) => {
                if (S.filter === 'bootstrapping') return i.state === 'bootstrapping' || i.state === 'starting';
                if (S.filter === 'stopped') return i.state === 'stopped' || i.state === 'stopping';
                return i.state === S.filter;
            });
        }
        if (q) {
            list = list.filter((i) =>
                String(i.socks_port).includes(q) || String(i.instance_id) === q ||
                (i.exit_ip || '').includes(q) || i.state.includes(q) || i.health.includes(q) ||
                (i.last_error || '').toLowerCase().includes(q));
        }
        const dir = S.sortAsc ? 1 : -1;
        const key = {
            id: (i) => i.instance_id,
            port: (i) => i.socks_port,
            state: (i) => STATE_ORDER[i.state] ?? 9,
            health: (i) => i.health,
            exit: (i) => i.exit_ip || '~',
            mem: (i) => i.memory_mb || 0,
            startup: (i) => i.startup_ms || 0,
        }[S.sortKey] || ((i) => i.instance_id);
        list.sort((a, b) => {
            const ka = key(a), kb = key(b);
            return (ka > kb ? 1 : ka < kb ? -1 : a.instance_id - b.instance_id) * dir;
        });
        return list;
    }

    function rowSig(i) {
        return [i.state, i.health, i.bootstrap_progress, i.exit_ip, i.is_tor, Math.round(i.memory_mb || 0),
            i.startup_ms, i.last_error, i.pid, i.retry_count].join('|');
    }

    function rowHTML(i) {
        const endpoint = i.port_count > 1 ? `127.0.0.1:${i.socks_port}\u2013${i.socks_port_end}` : `127.0.0.1:${i.socks_port}`;
        const boot = i.bootstrap_progress || 0;
        const isActive = ['running', 'starting', 'bootstrapping'].includes(i.state);
        const showBoot = i.state !== 'stopped';
        const exit = i.exit_ip
            ? `<span class="mono ${i.is_tor === false ? 'tor-no' : 'tor-yes'}" title="${i.is_tor === false ? 'NOT a Tor exit' : 'Verified Tor exit'}">${esc(i.exit_ip)}</span>`
            : '<span class="muted">—</span>';
        const err = i.last_error
            ? `<div class="error-text" title="${esc(i.last_error)}">${esc(i.last_error)}</div>`
            : (i.retry_count ? `<span class="muted">retry ${i.retry_count}</span>` : '<span class="muted">—</span>');
        return `
            <td><span class="mono muted">${i.instance_id}</span></td>
            <td><span class="endpoint-cell"><span class="mono">${endpoint}</span>
                <button class="copy-btn" data-row-action="copy" title="Copy socks5h://${endpoint}">${ICON.copy}</button></span></td>
            <td><span class="pill pill-${i.state}"><span class="pill-dot"></span>${cap(i.state)}</span></td>
            <td>${showBoot ? `<span class="mini-bar ${boot >= 100 ? 'done' : ''}"><i data-w="${boot}"></i></span><span class="mini-pct">${boot}%</span>` : '<span class="muted">—</span>'}</td>
            <td><span class="pill pill-${i.health}"><span class="pill-dot"></span>${cap(i.health)}</span></td>
            <td>${exit}</td>
            <td class="num"><span class="mono">${i.memory_mb ? Math.round(i.memory_mb) + ' MB' : '—'}</span></td>
            <td class="num"><span class="mono">${fmtMs(i.startup_ms)}</span></td>
            <td>${err}</td>
            <td><div class="actions">
                <button class="icon-btn" data-row-action="test" title="Verify Tor exit" ${i.state !== 'running' ? 'disabled' : ''}>${ICON.test}</button>
                <button class="icon-btn" data-row-action="newnym" title="New identity" ${i.state !== 'running' ? 'disabled' : ''}>${ICON.newnym}</button>
                <button class="icon-btn" data-row-action="restart" title="Restart">${ICON.restart}</button>
                ${isActive
                    ? `<button class="icon-btn danger" data-row-action="stop" title="Stop">${ICON.stop}</button>`
                    : `<button class="icon-btn" data-row-action="start" title="Start">${ICON.play}</button>`}
                <button class="icon-btn" data-row-action="browser" title="Open browser via this endpoint" ${i.state !== 'running' ? 'disabled' : ''}>${ICON.globe}</button>
            </div></td>`;
    }

    function renderTable() {
        const tbody = $('instances-tbody');
        const list = visibleInstances();

        for (const i of list) {
            const sig = rowSig(i);
            let entry = rowCache.get(i.instance_id);
            if (!entry) {
                const tr = document.createElement('tr');
                tr.dataset.id = i.instance_id;
                entry = { tr, sig: '' };
                rowCache.set(i.instance_id, entry);
            }
            if (entry.sig !== sig) {
                const changedState = entry.sig && entry.sig.split('|')[0] !== i.state;
                entry.tr.innerHTML = rowHTML(i);
                const bar = entry.tr.querySelector('.mini-bar > i');
                if (bar) bar.style.width = bar.dataset.w + '%';
                entry.sig = sig;
                if (changedState) {
                    entry.tr.classList.remove('flash');
                    void entry.tr.offsetWidth;
                    entry.tr.classList.add('flash');
                }
            }
        }

        const order = list.map((i) => i.instance_id).join(',');
        if (order !== lastOrder) {
            const frag = document.createDocumentFragment();
            list.forEach((i) => frag.appendChild(rowCache.get(i.instance_id).tr));
            tbody.replaceChildren(frag);
            lastOrder = order;
        }

        const empty = $('empty-state');
        if (list.length === 0) {
            empty.classList.add('show');
            if (S.instances.size === 0) {
                $('empty-title').textContent = 'No endpoints running';
                const total = S.status ? S.status.total_instances : 200;
                $('empty-text').innerHTML = `Press <strong>Start All</strong> to launch ${esc(fmtNum(total))} Tor SOCKS5 endpoints.`;
            } else {
                $('empty-title').textContent = 'No matching endpoints';
                $('empty-text').textContent = 'Try a different filter or search term.';
            }
        } else {
            empty.classList.remove('show');
        }

        document.querySelectorAll('th.sortable').forEach((th) => {
            th.classList.toggle('sorted', th.dataset.sort === S.sortKey);
            th.classList.toggle('asc', th.dataset.sort === S.sortKey && S.sortAsc);
        });
    }

    // --- heatmap ---
    function renderHeatmap() {
        const hm = $('heatmap');
        const all = [...S.instances.values()].sort((a, b) => a.instance_id - b.instance_id);
        const visible = new Set(visibleInstances().map((i) => i.instance_id));
        if (hm.childElementCount !== all.length) {
            hm.innerHTML = all.map((i) => `<div class="hm-cell" data-id="${i.instance_id}"></div>`).join('');
        }
        const cells = hm.children;
        all.forEach((i, idx) => {
            const cell = cells[idx];
            let cls = i.state;
            if (i.state === 'running' && i.health !== 'healthy' && i.health !== 'unknown') cls = 'unhealthy';
            cell.className = `hm-cell ${cls}${visible.has(i.instance_id) ? '' : ' dim'}`;
        });
        const empty = $('empty-state');
        if (all.length === 0) {
            empty.classList.add('show');
            $('table-container').classList.remove('hidden');
        } else {
            empty.classList.remove('show');
        }
    }

    let tooltip;
    function showTooltip(e, id) {
        const i = S.instances.get(id);
        if (!i) return;
        if (!tooltip) {
            tooltip = document.createElement('div');
            tooltip.className = 'hm-tooltip';
            document.body.appendChild(tooltip);
        }
        tooltip.innerHTML = `<strong>#${i.instance_id}</strong> · <code>127.0.0.1:${i.socks_port}</code><br>` +
            `${esc(cap(i.state))} · ${esc(i.health)} · boot ${i.bootstrap_progress}%` +
            (i.exit_ip ? `<br>Exit ${esc(i.exit_ip)}` : '');
        tooltip.style.display = 'block';
        const x = Math.min(e.clientX + 14, window.innerWidth - tooltip.offsetWidth - 10);
        tooltip.style.left = x + 'px';
        tooltip.style.top = (e.clientY + 14) + 'px';
    }
    function hideTooltip() { if (tooltip) tooltip.style.display = 'none'; }

    function setView(view) {
        S.view = view;
        localStorage.setItem('tpm.view', view);
        document.querySelectorAll('.view-btn').forEach((b) => b.classList.toggle('active', b.dataset.view === view));
        const grid = view === 'grid';
        $('heatmap').classList.toggle('hidden', !grid);
        $('heatmap-legend').classList.toggle('hidden', !grid);
        $('table-container').classList.toggle('hidden', grid && S.instances.size > 0);
        lastOrder = '';
        queueRender();
    }

    // ---------- Logs ----------
    function addLog(entry) {
        if (!entry || entry.seq <= S.lastLogSeq) return;
        S.lastLogSeq = entry.seq;
        S.logs.push(entry);
        if (S.logs.length > MAX_LOGS) S.logs.splice(0, S.logs.length - MAX_LOGS);
        if (S.tab !== 'logs') {
            S.unseenLogs++;
            $('tab-logs-count').textContent = S.unseenLogs > 999 ? '999+' : S.unseenLogs;
        } else if (logMatches(entry)) {
            appendLogLine(entry);
        }
    }

    function logMatches(e) {
        if (S.logLevel !== 'ALL') {
            const rank = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 };
            if ((rank[e.level] ?? 1) < rank[S.logLevel]) return false;
        }
        if (S.logSearch) {
            const q = S.logSearch.toLowerCase();
            if (!e.message.toLowerCase().includes(q) && !e.component.toLowerCase().includes(q)) return false;
        }
        return true;
    }

    function logLineEl(e) {
        const div = document.createElement('div');
        div.className = `log-line log-${e.level}`;
        const t = new Date(e.time);
        div.innerHTML = `<span class="log-time">${t.toLocaleTimeString([], { hour12: false })}</span>` +
            `<span class="log-level">${esc(e.level)}</span><span class="log-comp" title="${esc(e.component)}">${esc(e.component)}</span>` +
            `<span class="log-msg">${esc(e.message)}</span>`;
        return div;
    }

    function appendLogLine(e) {
        const view = $('log-view');
        view.appendChild(logLineEl(e));
        while (view.childElementCount > MAX_LOGS) view.firstChild.remove();
        if ($('log-autoscroll').checked) view.scrollTop = view.scrollHeight;
    }

    function renderLogs() {
        const view = $('log-view');
        const frag = document.createDocumentFragment();
        S.logs.filter(logMatches).forEach((e) => frag.appendChild(logLineEl(e)));
        view.replaceChildren(frag);
        if ($('log-autoscroll').checked) view.scrollTop = view.scrollHeight;
    }

    async function loadLogs() {
        try {
            const entries = await api('/api/logs?limit=1000');
            (entries || []).forEach(addLog);
        } catch { /* ignore */ }
    }

    function setTab(tab) {
        S.tab = tab;
        document.querySelectorAll('.tab').forEach((t) => {
            const on = t.dataset.tab === tab;
            t.classList.toggle('active', on);
            t.setAttribute('aria-selected', on);
        });
        $('panel-endpoints').classList.toggle('hidden', tab !== 'endpoints');
        $('panel-logs').classList.toggle('hidden', tab !== 'logs');
        $('view-toggle').classList.toggle('hidden', tab !== 'endpoints');
        if (tab === 'logs') {
            S.unseenLogs = 0;
            $('tab-logs-count').textContent = fmtNum(S.logs.length);
            renderLogs();
        }
    }

    // ---------- Actions ----------
    async function startAll() {
        await withLoading($('btn-start'), async () => {
            try {
                await api('/api/start', { method: 'POST' });
                toast('Starting endpoints…', 'info');
            } catch (e) { toast(e.message, 'error', 8000); }
        });
    }

    async function stopAll() {
        const st = S.status;
        const n = st ? st.running_instances + st.bootstrapping_instances + st.starting_instances : 0;
        if (!(await confirmDialog('Stop all endpoints?', `This will shut down ${n} Tor process(es). Applications using these proxies will lose connectivity.`, 'Stop All'))) return;
        try {
            await api('/api/stop', { method: 'POST' });
            toast('Stopping all endpoints…', 'info');
        } catch (e) { toast(e.message, 'error'); }
    }

    async function restartAll() {
        try {
            await api('/api/stop', { method: 'POST' });
            toast('Restarting all endpoints with the new configuration…', 'info');
            const waitStopped = async () => {
                for (let t = 0; t < 120; t++) {
                    await new Promise((r) => setTimeout(r, 500));
                    const st = await api('/api/status');
                    if (st.status.manager_state === 'stopped') return true;
                }
                return false;
            };
            if (await waitStopped()) await api('/api/start', { method: 'POST' });
        } catch (e) { toast(e.message, 'error'); }
    }

    async function restartFailed() {
        try {
            const r = await api('/api/restart-failed', { method: 'POST' });
            toast(r.count ? `Restarting ${r.count} failed endpoint(s)…` : 'No failed endpoints', r.count ? 'info' : 'success');
        } catch (e) { toast(e.message, 'error'); }
    }

    async function newnymAll() {
        await withLoading($('btn-newnym-all'), async () => {
            try {
                const r = await api('/api/newnym-all', { method: 'POST' });
                toast(`New identity requested on ${r.succeeded} endpoint(s)` + (r.failed ? `, ${r.failed} failed` : ''), r.failed ? 'warning' : 'success');
            } catch (e) { toast(e.message, 'error'); }
        });
    }

    async function testAll() {
        const st = S.status;
        const n = st ? st.running_instances : 0;
        if (n > 20 && !(await confirmDialog('Verify all endpoints?', `This sends one HTTPS request per endpoint (${n}) to check.torproject.org through Tor. It may take a few minutes.`, 'Verify'))) return;
        await withLoading($('btn-test-all'), async () => {
            toast(`Verifying ${n} endpoint(s) through Tor…`, 'info');
            try {
                const r = await api('/api/test-all', { method: 'POST' });
                const type = r.failed === 0 ? 'success' : r.via_tor === 0 ? 'error' : 'warning';
                toast(`Verification finished: ${r.via_tor}/${r.tested} exit through Tor` + (r.failed ? ` · ${r.failed} failed` : ''), type, 9000);
                refreshInstances();
            } catch (e) { toast(e.message, 'error'); }
        });
    }

    async function copyText(text, label) {
        try {
            await navigator.clipboard.writeText(text);
        } catch {
            const ta = document.createElement('textarea');
            ta.value = text;
            ta.style.position = 'fixed';
            ta.style.opacity = '0';
            document.body.appendChild(ta);
            ta.select();
            document.execCommand('copy');
            ta.remove();
        }
        toast(label || 'Copied to clipboard', 'success', 2500);
    }

    async function copyProxies() {
        try {
            const running = S.status && S.status.running_instances > 0;
            const text = await api(`/api/proxies?format=url${running ? '&running=1' : ''}`, { raw: true });
            const count = text.trim() ? text.trim().split(/\r?\n/).length : 0;
            if (!count) { toast('No endpoints to copy', 'warning'); return; }
            await copyText(text, `Copied ${count} ${running ? 'running ' : ''}endpoint(s) as socks5h:// URLs`);
        } catch (e) { toast(e.message, 'error'); }
    }

    async function instanceAction(id, action) {
        const i = S.instances.get(id);
        if (!i) return;
        const endpoint = `127.0.0.1:${i.socks_port}`;
        try {
            switch (action) {
                case 'copy':
                    await copyText(`socks5h://${endpoint}`, `Copied socks5h://${endpoint}`);
                    break;
                case 'test': {
                    toast(`Verifying ${endpoint} through Tor…`, 'info', 2500);
                    const r = await api(`/api/instances/${id}/test`, { method: 'POST' });
                    if (r.is_tor) toast(`${endpoint} → exit ${r.exit_ip} (Tor ✓, ${r.duration})`, 'success', 7000);
                    else toast(`${endpoint}: ${r.error || 'not routed through Tor'}`, 'error', 9000);
                    break;
                }
                case 'newnym':
                    await api(`/api/instances/${id}/newnym`, { method: 'POST' });
                    toast(`New identity requested for ${endpoint}`, 'success', 3000);
                    break;
                case 'restart':
                    await api(`/api/instances/${id}/restart`, { method: 'POST' });
                    toast(`Restarting ${endpoint}…`, 'info', 3000);
                    break;
                case 'stop':
                    await api(`/api/instances/${id}/stop`, { method: 'POST' });
                    toast(`Stopping ${endpoint}…`, 'info', 3000);
                    break;
                case 'start':
                    await api(`/api/instances/${id}/start`, { method: 'POST' });
                    toast(`Starting ${endpoint}…`, 'info', 3000);
                    break;
                case 'toggle':
                    return instanceAction(id, ['running', 'starting', 'bootstrapping'].includes(i.state) ? 'stop' : 'start');
                case 'browser':
                    openBrowserModal(i.socks_port);
                    break;
            }
        } catch (e) { toast(e.message, 'error', 7000); }
    }

    async function quit() {
        if (!(await confirmDialog('Quit TorProxyManager?', 'All Tor endpoints will be stopped and the application will exit.', 'Quit'))) return;
        try { await api('/api/shutdown', { method: 'POST' }); } catch { /* connection drops */ }
        S.exited = true;
        if (es) es.close();
        document.body.innerHTML = `<div class="exit-screen">
            <div><div class="empty-orb"></div><h1>TorProxyManager has exited</h1>
            <p>All endpoints were stopped. You can close this tab.</p></div></div>`;
    }

    // ---------- Drawer ----------
    function openDrawer(id) {
        S.drawerId = id;
        $('drawer').classList.add('open');
        $('drawer').setAttribute('aria-hidden', 'false');
        $('drawer-scrim').classList.add('open');
        $('drawer-log').textContent = 'Loading…';
        renderDrawer();
        loadInstanceLog();
    }

    function closeDrawer() {
        S.drawerId = null;
        $('drawer').classList.remove('open');
        $('drawer').setAttribute('aria-hidden', 'true');
        $('drawer-scrim').classList.remove('open');
    }

    function renderDrawer() {
        if (S.drawerId == null) return;
        const i = S.instances.get(S.drawerId);
        if (!i) return;
        $('drawer-title').textContent = `Endpoint #${i.instance_id}`;
        $('drawer-endpoint').textContent = `socks5h://127.0.0.1:${i.socks_port}`;
        const rows = [
            ['State', cap(i.state)],
            ['Health', cap(i.health)],
            ['Bootstrap', `${i.bootstrap_progress}%${i.bootstrap_summary ? ' — ' + i.bootstrap_summary : ''}`],
            ['SOCKS5 port', i.socks_port],
            ['Control port', i.control_port],
            ['PID', i.pid || '—'],
            ['Exit IP', i.exit_ip ? `${i.exit_ip} ${i.is_tor ? '(Tor ✓)' : '(NOT Tor)'}` : '—'],
            ['Last exit check', i.last_exit_check ? ago(i.last_exit_check) : '—'],
            ['New identity', i.last_new_identity ? ago(i.last_new_identity) : '—'],
            ['Started', fmtTime(i.start_time)],
            ['Uptime', fmtDuration(i.uptime_seconds)],
            ['Startup time', fmtMs(i.startup_ms)],
            ['Memory / CPU', i.memory_mb ? `${i.memory_mb.toFixed(1)} MB / ${(i.cpu_percent || 0).toFixed(1)}%` : '—'],
            ['Last health check', i.last_health_check ? ago(i.last_health_check) : '—'],
            ['Retries / restarts', `${i.retry_count} / ${i.total_restarts}`],
            ['Last error', i.last_error || '—'],
        ];
        $('drawer-kv').innerHTML = rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');
        const active = ['running', 'starting', 'bootstrapping'].includes(i.state);
        const toggle = document.querySelector('[data-inst-action="toggle"]');
        toggle.textContent = active ? 'Stop' : 'Start';
        document.querySelectorAll('[data-inst-action="test"],[data-inst-action="newnym"],[data-inst-action="browser"]')
            .forEach((b) => { b.disabled = i.state !== 'running'; });
    }

    async function loadInstanceLog() {
        if (S.drawerId == null) return;
        try {
            const text = await api(`/api/instances/${S.drawerId}/log`, { raw: true });
            const pre = $('drawer-log');
            pre.textContent = text.trim() || '(log is empty)';
            pre.scrollTop = pre.scrollHeight;
        } catch {
            $('drawer-log').textContent = '(no log available yet)';
        }
    }

    // ---------- Settings ----------
    async function openSettings() {
        try {
            const [cfg, br] = await Promise.all([api('/api/config'), api('/api/browsers').catch(() => ({ detected: [] }))]);
            S.config = cfg;
            S.browsers = br.detected || [];
            fillSettings(cfg.settings);
            $('settings-file').textContent = cfg.file || '';
            $('browser-list').innerHTML = S.browsers.map((b) => `<option value="${esc(b.path)}">${esc(b.name)}</option>`).join('');
            $('settings-errors').classList.add('hidden');
            $('settings-modal').classList.remove('hidden');
            $('cfg-start-port').focus();
        } catch (e) { toast('Could not load configuration: ' + e.message, 'error'); }
    }

    function closeSettings() { $('settings-modal').classList.add('hidden'); }

    function fillSettings(s) {
        document.querySelectorAll('#settings-form [data-key]').forEach((el) => {
            const v = s[el.dataset.key];
            if (el.type === 'checkbox') el.checked = !!v;
            else el.value = v ?? '';
            el.classList.remove('invalid');
        });
        $('cfg-endpoint-slider').value = s.endpoint_count;
        updatePreview();
    }

    function readSettings() {
        const out = {};
        document.querySelectorAll('#settings-form [data-key]').forEach((el) => {
            const t = el.dataset.type;
            let v;
            if (t === 'bool') v = el.checked;
            else if (t === 'int') v = parseInt(el.value, 10);
            else if (t === 'float') v = parseFloat(el.value);
            else v = el.value.trim();
            out[el.dataset.key] = v;
        });
        return out;
    }

    function updatePreview() {
        const s = readSettings();
        const ok = (n) => Number.isFinite(n);
        const count = s.endpoint_count;
        const socksEnd = s.start_port + count - 1;
        const ctrlEnd = s.control_port_start + count - 1;
        $('rp-socks').textContent = ok(socksEnd) ? `${s.start_port}–${socksEnd}` : '—';
        $('rp-control').textContent = ok(ctrlEnd) ? `${s.control_port_start}–${ctrlEnd}` : '—';
        $('rp-ram').textContent = ok(count) ? `≈ ${fmtMB(count * 28)}` : '—';

        const overlap = ok(socksEnd) && ok(ctrlEnd) && s.start_port <= ctrlEnd && s.control_port_start <= socksEnd;
        const webIn = (a, b) => s.web_ui_port >= a && s.web_ui_port <= b;
        $('cfg-start-port').classList.toggle('invalid', !(s.start_port >= 1024 && socksEnd <= 65535) || overlap);
        $('cfg-control-port').classList.toggle('invalid', !(s.control_port_start >= 1024 && ctrlEnd <= 65535) || overlap);
        $('cfg-endpoint-count').classList.toggle('invalid', !(count >= 1 && count <= 500));
        $('cfg-web-port').classList.toggle('invalid', !(s.web_ui_port >= 1024 && s.web_ui_port <= 65535) || webIn(s.start_port, socksEnd) || webIn(s.control_port_start, ctrlEnd));
    }

    async function saveConfig() {
        const body = readSettings();
        const errBox = $('settings-errors');
        await withLoading($('btn-save-config'), async () => {
            try {
                const r = await api('/api/config', { method: 'POST', body });
                errBox.classList.add('hidden');
                closeSettings();
                if (r.web_port_changed) {
                    toast(`Saved. The dashboard will move to port ${r.settings.web_ui_port} after TorProxyManager restarts.`, 'warning', 9000);
                } else if (r.restart_required) {
                    toast('Configuration saved — restart endpoints to apply.', 'warning', 6000);
                } else {
                    toast('Configuration saved', 'success');
                }
                refreshAll();
            } catch (e) {
                const errs = e.errors || [e.message];
                errBox.innerHTML = `<strong>Please fix the following:</strong><ul>${errs.map((x) => `<li>${esc(x)}</li>`).join('')}</ul>`;
                errBox.classList.remove('hidden');
                errBox.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
            }
        });
    }

    async function resetDefaults() {
        if (!(await confirmDialog('Reset to defaults?', 'All fields in this form will be reset to factory defaults. Nothing is saved until you press Save.', 'Reset'))) return;
        try {
            const d = await api('/api/config/defaults');
            fillSettings(d);
            toast('Defaults loaded — press Save to apply', 'info');
        } catch (e) { toast(e.message, 'error'); }
    }

    // ---------- Browser ----------
    async function openBrowserModal(port) {
        const running = [...S.instances.values()].filter((i) => i.state === 'running').sort((a, b) => a.socks_port - b.socks_port);
        const sel = $('browser-port');
        if (!running.length) { toast('Start at least one endpoint first', 'warning'); return; }
        const opts = [];
        running.forEach((i) => { for (let p = i.socks_port; p < i.socks_port + (i.port_count || 1); p++) opts.push(p); });
        sel.innerHTML = opts.map((p) => `<option value="${p}">127.0.0.1:${p}</option>`).join('');
        sel.value = String(port && opts.includes(port) ? port : opts[0]);
        $('browser-port-display').textContent = sel.value;
        try {
            const br = await api('/api/browsers');
            const chosen = br.configured || ((br.detected || []).find((b) => b.kind === 'firefox' || b.kind === 'chromium') || {}).path;
            $('browser-name').textContent = chosen ? chosen.split(/[\\/]/).pop() : 'None found — set in Settings';
        } catch { /* ignore */ }
        $('browser-modal').classList.remove('hidden');
    }

    async function launchBrowser() {
        const port = parseInt($('browser-port').value, 10);
        await withLoading($('btn-launch-browser'), async () => {
            try {
                const r = await api('/api/browser/launch', { method: 'POST', body: { port } });
                toast(r.message, 'success', 6000);
                $('browser-modal').classList.add('hidden');
            } catch (e) { toast(e.message, 'error', 8000); }
        });
    }

    // ---------- Event wiring ----------
    const ACTIONS = {
        'start-all': startAll,
        'stop-all': stopAll,
        'restart-failed': restartFailed,
        'newnym-all': newnymAll,
        'test-all': testAll,
        'copy-proxies': copyProxies,
        'open-settings': openSettings,
        'close-settings': closeSettings,
        'save-config': saveConfig,
        'reset-defaults': resetDefaults,
        'open-browser-modal': () => openBrowserModal(),
        'close-browser-modal': () => $('browser-modal').classList.add('hidden'),
        'launch-browser': launchBrowser,
        'close-drawer': closeDrawer,
        'refresh-instance-log': loadInstanceLog,
        'clear-logs': () => { $('log-view').innerHTML = ''; },
        'quit': quit,
        'toggle-export': (btn) => {
            const menu = $('export-menu');
            const open = menu.classList.toggle('hidden') === false;
            btn.setAttribute('aria-expanded', open);
        },
        'alert-action': (btn) => {
            if (btn.dataset.target === 'settings') openSettings();
            if (btn.dataset.target === 'restart-all') restartAll();
        },
    };

    document.addEventListener('click', (e) => {
        const actionEl = e.target.closest('[data-action]');
        if (actionEl && ACTIONS[actionEl.dataset.action]) {
            e.preventDefault();
            ACTIONS[actionEl.dataset.action](actionEl);
            return;
        }

        if (!e.target.closest('.dropdown')) $('export-menu').classList.add('hidden');
        if (e.target.closest('#export-menu a')) $('export-menu').classList.add('hidden');

        const rowBtn = e.target.closest('[data-row-action]');
        if (rowBtn) {
            e.stopPropagation();
            const tr = rowBtn.closest('tr');
            if (tr && !rowBtn.disabled) instanceAction(Number(tr.dataset.id), rowBtn.dataset.rowAction);
            return;
        }

        const instBtn = e.target.closest('[data-inst-action]');
        if (instBtn && S.drawerId != null) {
            instanceAction(S.drawerId, instBtn.dataset.instAction);
            return;
        }

        const tr = e.target.closest('#instances-tbody tr');
        if (tr) { openDrawer(Number(tr.dataset.id)); return; }

        const cell = e.target.closest('.hm-cell');
        if (cell) { openDrawer(Number(cell.dataset.id)); return; }

        const th = e.target.closest('th.sortable');
        if (th) {
            if (S.sortKey === th.dataset.sort) S.sortAsc = !S.sortAsc;
            else { S.sortKey = th.dataset.sort; S.sortAsc = true; }
            queueRender();
            return;
        }

        const chip = e.target.closest('#state-filter .chip');
        if (chip) {
            S.filter = chip.dataset.filter;
            document.querySelectorAll('#state-filter .chip').forEach((c) => c.classList.toggle('active', c === chip));
            queueRender();
            return;
        }

        const lchip = e.target.closest('#log-level-filter .chip');
        if (lchip) {
            S.logLevel = lchip.dataset.level;
            document.querySelectorAll('#log-level-filter .chip').forEach((c) => c.classList.toggle('active', c === lchip));
            renderLogs();
            return;
        }

        const tab = e.target.closest('.tab');
        if (tab) { setTab(tab.dataset.tab); return; }

        const vb = e.target.closest('.view-btn');
        if (vb) { setView(vb.dataset.view); return; }

        if (e.target.classList.contains('modal-overlay') && e.target.id !== 'confirm-modal') {
            e.target.classList.add('hidden');
        }
    });

    $('heatmap').addEventListener('mousemove', (e) => {
        const cell = e.target.closest('.hm-cell');
        if (cell) showTooltip(e, Number(cell.dataset.id)); else hideTooltip();
    });
    $('heatmap').addEventListener('mouseleave', hideTooltip);

    let searchTimer;
    $('search-input').addEventListener('input', (e) => {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(() => { S.search = e.target.value; queueRender(); }, 120);
    });
    $('log-search').addEventListener('input', (e) => {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(() => { S.logSearch = e.target.value; renderLogs(); }, 150);
    });
    $('browser-port').addEventListener('change', (e) => { $('browser-port-display').textContent = e.target.value; });

    $('settings-form').addEventListener('input', (e) => {
        if (e.target.id === 'cfg-endpoint-slider') $('cfg-endpoint-count').value = e.target.value;
        if (e.target.id === 'cfg-endpoint-count') $('cfg-endpoint-slider').value = e.target.value;
        updatePreview();
    });
    $('settings-form').addEventListener('submit', (e) => { e.preventDefault(); saveConfig(); });

    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
            if (!$('confirm-modal').classList.contains('hidden')) { $('confirm-cancel').click(); return; }
            document.querySelectorAll('.modal-overlay:not(.hidden)').forEach((m) => m.classList.add('hidden'));
            closeDrawer();
            $('export-menu').classList.add('hidden');
        }
        if (e.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) {
            e.preventDefault();
            (S.tab === 'logs' ? $('log-search') : $('search-input')).focus();
        }
    });

    // Keep relative timestamps fresh in the drawer and uptime counters.
    setInterval(() => {
        if (S.drawerId != null) renderDrawer();
        if (S.status && S.status.manager_state === 'running') {
            S.status.uptime_seconds += 1;
            $('uptime').textContent = fmtDuration(S.status.uptime_seconds);
        }
    }, 1000);

    // ---------- Boot ----------
    setView(S.view);
    refreshAll();
    loadLogs();
    connect();
})();
