/* ═══════════════════════════════════════════════════════════════
   SKYLENS AIRSPACE MONITOR – Application Controller
   State management, polling, events, keyboard shortcuts, tabs
   Drone history persistence: lost contacts stay on map until dismissed
   ═══════════════════════════════════════════════════════════════ */

var App = {

    state: {
        stats: {}, taps: [], uavs: [], alerts: [],
        uptime: '--', flightLogs: 0, connected: false,
        selectedDrone: null, activeTab: 'contacts',
        events: [],
        prevTaps: {}, prevUavs: {}, prevAlertCount: 0,
        msgRate: '0', _prevMsgC: 0, _prevMsgT: Date.now(), _firstPoll: true,
        searchQuery: '', activeFilters: {}, sortMode: 'time',
        compactView: false, alertFilter: 'all',
        soundEnabled: true, pollInterval: 30000,  // Reduced: only fallback poll every 30s
        showLost: true,  // show lost contacts by default
        showControllers: false  // hide DJI RC controllers by default
    },

    _pollTimer: null,
    _pollInFlight: false,
    _consecutivePollFails: 0,
    _cleanupTimer: null,
    _uiRefreshScheduled: false,
    _lastUiRefresh: 0,
    _cachedFilteredUavs: null,
    _cachedFilteredUavsKey: null,

    /* ── Map update throttle ─────────────── */
    _mapDirty: false,
    _mapRefreshScheduled: false,
    _lastMapRefresh: 0,

    /* ── rAF coalescing for WS messages ─── */
    _wsQueue: [],          // raw parsed events waiting for next frame
    _wsRafId: null,        // requestAnimationFrame handle

    /* ── WebSocket state ──────────────────── */
    _ws: null,
    _wsReconnectAttempts: 0,
    _wsReconnectTimer: null,
    _wsConnected: false,
    _audioCtx: null,
    _lastSoundTime: 0,
    _tagTarget: null,
    userLocation: null,
    _geoWatchId: null,
    _geoDenied: false,
    _notifyPermission: false,
    _zoneViolationCache: {},
    _activityLog: [],
    _escDiv: null,

    /* ── html escape ────────────────────── */
    _esc: function (s) {
        if (!this._escDiv) this._escDiv = document.createElement('div');
        this._escDiv.textContent = s == null ? '' : String(s);
        return this._escDiv.innerHTML;
    },

    /* ── init ────────────────────────────── */
    init: function () {
        var self = this;

        // Map
        NzMap.init(function (id) { self.selectDrone(id); });

        // Dialogs
        NzDialogs.init();

        // Left panel tabs
        document.querySelector('.pl-hdr').addEventListener('click', function () {
            self.cycleTab();
        });

        // UAV list click → select drone
        document.getElementById('uav-list').addEventListener('click', function (e) {
            // Dismiss button
            var dismissBtn = e.target.closest('.uc-btn-dismiss');
            if (dismissBtn) {
                e.stopPropagation();
                var did = dismissBtn.getAttribute('data-dismiss-id');
                if (did) self.dismissDrone(did);
                return;
            }
            if (e.target.closest('.uc-btn')) return;
            var card = e.target.closest('.uc');
            if (!card) return;
            self.selectDrone(card.getAttribute('data-id'));
        });

        // Search (debounced 300ms)
        var _searchTimer = null;
        document.getElementById('uav-search').addEventListener('input', function () {
            self.state.searchQuery = this.value;
            if (_searchTimer) clearTimeout(_searchTimer);
            _searchTimer = setTimeout(function () { self.refreshUI(); }, 300);
        });

        // Filters
        document.getElementById('filter-chips').addEventListener('click', function (e) {
            var chip = e.target.closest('.fchip');
            if (!chip || chip.id === 'filter-clear') return;
            var f = chip.dataset.filter;
            if (self.state.activeFilters[f]) delete self.state.activeFilters[f];
            else self.state.activeFilters[f] = true;
            chip.classList.toggle('on');
            var hasActive = Object.keys(self.state.activeFilters).length > 0;
            var clearBtn = document.getElementById('filter-clear');
            if (clearBtn) clearBtn.style.display = hasActive ? '' : 'none';
            self.refreshUI();
        });
        document.getElementById('filter-clear').addEventListener('click', function () {
            self.state.activeFilters = {};
            document.querySelectorAll('.fchip.on').forEach(function (c) { c.classList.remove('on'); });
            this.style.display = 'none';
            self.refreshUI();
        });

        // Sort
        document.getElementById('uav-sort').addEventListener('change', function () {
            self.state.sortMode = this.value;
            self.refreshUI();
        });

        // View toggles
        document.getElementById('view-detailed').addEventListener('click', function () {
            self.state.compactView = false;
            document.getElementById('view-detailed').classList.add('on');
            document.getElementById('view-compact').classList.remove('on');
            self.refreshUI();
        });
        document.getElementById('view-compact').addEventListener('click', function () {
            self.state.compactView = true;
            document.getElementById('view-compact').classList.add('on');
            document.getElementById('view-detailed').classList.remove('on');
            self.refreshUI();
        });

        // Toolbar buttons
        document.getElementById('tb-refresh').addEventListener('click', function () { self.poll(); });
        document.getElementById('tb-fit').addEventListener('click', function () { NzMap.fitAll(); });

        // Layer toggles
        ['trails', 'zones', 'threats', 'operators', 'vectors'].forEach(function (layer) {
            document.getElementById('tb-' + layer).addEventListener('click', function () {
                var on = NzMap.toggleLayer(layer);
                this.classList.toggle('on', on);
            });
        });

        // ── shared helper: apply showLost state across UI + map + DB ──
        function applyShowLost(on) {
            self.state.showLost = on;
            // Sync both UI controls
            if (lostBtn) lostBtn.classList.toggle('on', on);
            var cb = document.getElementById('show-lost-cb');
            if (cb) cb.checked = on;
            // Map visibility
            NzMap.setShowLost(on);
            // When showing lost, unhide any DB-hidden contacts so the
            // API returns them again on the next poll.  When hiding,
            // mark them hidden in the DB so the API stops returning them.
            if (on) {
                SkylensAuth.fetch('/api/uavs/unhide-all', { method: 'POST' }).catch(function () {});
            }
            self.refreshUI();
        }

        // Show Lost toggle (toolbar button)
        var lostBtn = document.getElementById('tb-show-lost');
        if (lostBtn) {
            lostBtn.addEventListener('click', function () {
                applyShowLost(!self.state.showLost);
            });
        }

        // Show Lost toggle (sidebar checkbox)
        var lostCb = document.getElementById('show-lost-cb');
        if (lostCb) {
            lostCb.checked = self.state.showLost;
            lostCb.addEventListener('change', function () {
                applyShowLost(this.checked);
            });
        }

        // ── shared helper: apply showControllers state across UI + map ──
        function applyShowControllers(on) {
            self.state.showControllers = on;
            var ctrlBtn = document.getElementById('tb-show-controllers');
            if (ctrlBtn) ctrlBtn.classList.toggle('on', on);
            var ctrlCb = document.getElementById('set-show-controllers');
            if (ctrlCb) ctrlCb.checked = on;
            NzMap.setShowControllers(on);
            self._invalidateUavCache();
            self.refreshUI();
        }

        // Show Controllers toggle (toolbar button)
        var ctrlBtn = document.getElementById('tb-show-controllers');
        if (ctrlBtn) {
            ctrlBtn.addEventListener('click', function () {
                applyShowControllers(!self.state.showControllers);
            });
        }

        // Dismiss All Lost (toolbar button)
        var dismissAllBtn = document.getElementById('tb-dismiss-lost');
        if (dismissAllBtn) {
            dismissAllBtn.addEventListener('click', function () { self.dismissAllLost(); });
        }

        // Hide All Lost (sidebar button)
        var hideAllLostBtn = document.getElementById('hide-all-lost');
        if (hideAllLostBtn) {
            hideAllLostBtn.addEventListener('click', function () { self.dismissAllLost(); });
        }

        // Measure
        var measuring = false;
        document.getElementById('tb-measure').addEventListener('click', function () {
            if (!measuring) {
                NzMap.startMeasure();
                document.getElementById('measure-bar').style.display = '';
                measuring = true;
            } else {
                NzMap.stopMeasure();
                document.getElementById('measure-bar').style.display = 'none';
                measuring = false;
            }
        });
        document.getElementById('measure-close').addEventListener('click', function () {
            NzMap.stopMeasure();
            document.getElementById('measure-bar').style.display = 'none';
            measuring = false;
        });
        document.getElementById('measure-clear').addEventListener('click', function () {
            NzMap.stopMeasure();
            NzMap.startMeasure();
            document.getElementById('measure-val').textContent = '0 m';
        });

        // Draw
        document.getElementById('tb-draw').addEventListener('click', function () {
            var tb = document.getElementById('draw-toolbar');
            tb.style.display = tb.style.display === 'none' ? '' : 'none';
        });
        document.getElementById('draw-toolbar').addEventListener('click', function (e) {
            var btn = e.target.closest('.dtb[data-draw]');
            if (btn) { NzMap.startDraw(btn.dataset.draw); return; }
        });
        document.getElementById('draw-clear').addEventListener('click', function () { NzMap.clearDrawings(); });
        document.getElementById('draw-close').addEventListener('click', function () { document.getElementById('draw-toolbar').style.display = 'none'; });

        // Fullscreen map toggle
        var _fullscreen = false;
        document.getElementById('tb-fullscreen').addEventListener('click', function () {
            _fullscreen = !_fullscreen;
            document.getElementById('panel-left').classList.toggle('hidden', _fullscreen);
            document.getElementById('panel-right').classList.toggle('hidden', _fullscreen);
            document.getElementById('alert-panel').classList.toggle('hidden', _fullscreen);
            this.classList.toggle('on', _fullscreen);
            setTimeout(function () { NzMap.getMap().invalidateSize(); }, 250);
        });

        // Modal openers
        document.getElementById('tb-export').addEventListener('click', function () { NzDialogs.openModal('modal-export'); });
        document.getElementById('tb-zone-mgr').addEventListener('click', function () { NzDialogs.updateZoneList(); NzDialogs.openModal('modal-zones'); });
        document.getElementById('tb-inject').addEventListener('click', function () { NzDialogs.openModal('modal-inject'); });
        document.getElementById('tb-settings').addEventListener('click', function () { NzDialogs.openModal('modal-settings'); });
        document.getElementById('tb-shortcuts').addEventListener('click', function () { NzDialogs.openModal('modal-shortcuts'); });

        // Range Rings toggle (single-click: toggle visibility, double-click: open manager)
        var rangeBtn = document.getElementById('tb-ranges');
        if (rangeBtn) {
            var rangeClickTimer = null;
            rangeBtn.addEventListener('click', function () {
                if (rangeClickTimer) { clearTimeout(rangeClickTimer); rangeClickTimer = null; return; }
                rangeClickTimer = setTimeout(function () {
                    rangeClickTimer = null;
                    var vis = NzMap.toggleCustomRanges();
                    rangeBtn.classList.toggle('on', vis);
                }, 250);
            });
            rangeBtn.addEventListener('dblclick', function (e) {
                e.preventDefault();
                if (rangeClickTimer) { clearTimeout(rangeClickTimer); rangeClickTimer = null; }
                NzDialogs.updateRangeList();
                NzDialogs.openModal('modal-ranges');
            });
            // Set initial state from persisted visibility
            try {
                var vis = localStorage.getItem('skylens_range_rings_visible');
                if (vis !== null) {
                    rangeBtn.classList.toggle('on', JSON.parse(vis));
                } else if (NzMap.getCustomRanges && NzMap.getCustomRanges().length > 0) {
                    rangeBtn.classList.add('on');
                }
            } catch(e) {
                if (NzMap.getCustomRanges && NzMap.getCustomRanges().length > 0) {
                    rangeBtn.classList.add('on');
                }
            }
        }


        // Alert actions
        document.getElementById('alert-sound').addEventListener('click', function () {
            self.state.soundEnabled = !self.state.soundEnabled;
            this.textContent = self.state.soundEnabled ? 'Sound On' : 'Sound Off';
            this.classList.toggle('on', self.state.soundEnabled);
        });
        document.getElementById('alert-ack').addEventListener('click', function () { self.ackAllAlerts(); });
        document.getElementById('alert-exp').addEventListener('click', function () { NzDialogs.openModal('modal-export'); });

        // Keyboard shortcuts
        document.addEventListener('keydown', function (e) { self.handleKey(e); });

        // Clock
        setInterval(function () {
            var el = document.getElementById('hdr-clock');
            if (el) el.textContent = new Date().toLocaleTimeString('en-US', { hour12: false });
        }, 1000);

        // Browser geolocation (watch for updates)
        this._startGeolocation();

        // Browser notifications
        if ('Notification' in window) {
            if (Notification.permission === 'granted') {
                self._notifyPermission = true;
            } else if (Notification.permission === 'default') {
                Notification.requestPermission().then(function (perm) {
                    self._notifyPermission = perm === 'granted';
                });
            }
        }

        // Auto-follow toggle
        var afBtn = document.getElementById('tb-autofollow');
        if (afBtn) {
            afBtn.addEventListener('click', function () {
                var on = !NzMap.isAutoFollow();
                NzMap.setAutoFollow(on);
                afBtn.classList.toggle('on', on);
            });
        }

        // Handle URL query parameters (e.g., from fleet page redirect)
        var params = new URLSearchParams(window.location.search);
        var focusDrone = params.get('focus');
        var showPath = params.get('showPath') === '1';
        if (focusDrone) {
            // After first poll, select and optionally show path
            setTimeout(function () {
                self.selectDrone(focusDrone);
                if (showPath && typeof NzMap !== 'undefined' && NzMap.showFlightPath) {
                    NzMap.showFlightPath(focusDrone);
                }
                // Clean up URL
                if (history.replaceState) {
                    history.replaceState(null, '', window.location.pathname);
                }
            }, 1500);
        }

        // Start WebSocket connection for real-time updates
        self.initWebSocket();

        // Start polling as fallback -- fire first poll immediately
        self.poll();
        self.startPoll();

        // Periodic memory cleanup (every 60 seconds)
        self._cleanupTimer = setInterval(function () {
            self._performCleanup();
        }, 60000);
    },

    /* Periodic memory cleanup */
    _performCleanup: function () {
        /* Invalidate caches */
        this._invalidateUavCache();
        /* Clean up map memory */
        if (typeof NzMap !== 'undefined' && NzMap.cleanup) {
            NzMap.cleanup();
        }
        /* Trim activity log */
        if (this._activityLog && this._activityLog.length > 100) {
            this._activityLog.splice(0, this._activityLog.length - 100);
        }
        /* Trim events */
        if (this.state.events && this.state.events.length > 40) {
            this.state.events.length = 40;
        }
    },

    /* ═══════════════════════════════════════
       WEBSOCKET — Real-time updates
       Connects to ws://host:8081/ws
       Handles: drone_new, drone_update, drone_lost, tap_status
       ═══════════════════════════════════════ */

    initWebSocket: function () {
        var self = this;

        // Determine WebSocket URL (same host, port 8081)
        var protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        var host = window.location.hostname;

        // Try ticket-based auth first (preferred: avoids JWT in URL/logs).
        // Falls back to plain connection (server checks cookie).
        var connectWs = function (query) {
            var wsUrl = protocol + '//' + host + ':8081/ws' + (query || '');
            try {
                self._ws = new WebSocket(wsUrl);
            } catch (e) {
                console.error('Skylens: WebSocket creation failed:', e);
                self._scheduleWsReconnect();
                return;
            }
            self._bindWsEvents();
        };

        // Request one-time ticket from server
        fetch('/api/auth/ws-ticket', { method: 'POST', credentials: 'same-origin' })
            .then(function (res) {
                if (res.ok) return res.json();
                throw new Error('ticket request failed');
            })
            .then(function (data) {
                connectWs('?ticket=' + encodeURIComponent(data.ticket));
            })
            .catch(function () {
                // Auth disabled or ticket fetch failed - connect without token
                connectWs('');
            });
    },

    _bindWsEvents: function () {
        var self = this;

        self._ws.onopen = function () {
            self._wsConnected = true;
            self._wsReconnectAttempts = 0;
            self.state.connected = true;
            self.addEvent('WebSocket connected');
            NzUI.hideConnectionError();

            // Clear any pending reconnect timer
            if (self._wsReconnectTimer) {
                clearTimeout(self._wsReconnectTimer);
                self._wsReconnectTimer = null;
            }

            self.refreshUI();
        };

        self._ws.onclose = function (e) {
            self._wsConnected = false;
            console.warn('Skylens: WebSocket closed', e.code, e.reason);
            self._scheduleWsReconnect();
        };

        self._ws.onerror = function (e) {
            console.error('Skylens: WebSocket error', e);
            // onclose will be called after this
        };

        self._ws.onmessage = function (e) {
            self._handleWsMessage(e.data);
        };
    },

    _scheduleWsReconnect: function () {
        var self = this;

        if (self._wsReconnectTimer) return;

        self._wsReconnectAttempts++;

        // Exponential backoff: 1s, 2s, 4s, 8s, 16s, max 30s
        var delay = Math.min(1000 * Math.pow(2, self._wsReconnectAttempts - 1), 30000);

        if (self._wsReconnectAttempts === 1) {
            self.addEvent('WebSocket disconnected, reconnecting...');
        }

        if (self._wsReconnectAttempts >= 3) {
            NzUI.showConnectionError('WebSocket disconnected, retrying...', delay);
        }

        self._wsReconnectTimer = setTimeout(function () {
            self._wsReconnectTimer = null;
            console.log('Skylens: WebSocket reconnecting (attempt ' + self._wsReconnectAttempts + ')...');
            self.initWebSocket();
        }, delay);
    },

    _handleWsMessage: function (data) {
        var self = this;
        var msg;

        try {
            msg = JSON.parse(data);
        } catch (e) {
            console.warn('Skylens: Invalid WebSocket message', e);
            return;
        }

        // Flatten batched messages into individual events
        if (msg.type === 'batch' && Array.isArray(msg.events)) {
            for (var i = 0; i < msg.events.length; i++) {
                self._wsQueue.push(msg.events[i]);
            }
        } else {
            self._wsQueue.push(msg);
        }

        // Schedule one rAF to process all queued events and render once
        if (!self._wsRafId) {
            self._wsRafId = requestAnimationFrame(function () {
                self._wsRafId = null;
                self._flushWsQueue();
            });
        }
    },

    /* Process all queued WS events in one shot, then render once */
    _flushWsQueue: function () {
        var self = this;
        var queue = self._wsQueue;
        self._wsQueue = [];
        if (!queue.length) return;

        var hadUpdate = false;
        var hadNew = false;
        var hadLost = false;
        var hadTap = false;
        var hadAlert = false;

        for (var i = 0; i < queue.length; i++) {
            var msg = queue[i];
            var type = msg.type;
            var payload = msg.data;

            switch (type) {
                case 'drone_update':
                    self._applyDroneUpdate(payload);
                    hadUpdate = true;
                    break;
                case 'drone_new':
                    self._handleDroneNew(payload);
                    hadNew = true;
                    break;
                case 'drone_lost':
                    self._handleDroneLost(payload);
                    hadLost = true;
                    break;
                case 'tap_status':
                    self._handleTapStatus(payload);
                    hadTap = true;
                    break;
                case 'alert':
                    self._handleAlert(payload);
                    hadAlert = true;
                    break;
                case 'system_refresh':
                    console.log('Skylens: System refresh event, re-fetching data');
                    self.addEvent('Server reconnected after resume');
                    NzUI.hideConnectionError();
                    self._fetchData();
                    break;
            }
        }

        // Single render pass for all accumulated state changes
        if (hadUpdate || hadNew || hadLost) {
            self._invalidateUavCache();
            self._scheduleMapRefresh();
            self.scheduleUiRefresh();
        } else if (hadTap || hadAlert) {
            self.scheduleUiRefresh();
        }
    },

    /* Apply drone update to local state WITHOUT triggering renders (rAF batched) */
    _applyDroneUpdate: function (drone) {
        var id = drone.identifier || drone.mac;
        if (!id) return;

        for (var i = 0; i < this.state.uavs.length; i++) {
            var u = this.state.uavs[i];
            if ((u.identifier || u.mac) === id) {
                Object.assign(this.state.uavs[i], drone);
                return;
            }
        }
        // Not found — treat as new
        this.state.uavs.push(drone);
    },

    _processSingleMessage: function (msg) {
        var self = this;
        var type = msg.type;
        var payload = msg.data;

        switch (type) {
            case 'drone_new':
                self._handleDroneNew(payload);
                break;

            case 'drone_update':
                self._handleDroneUpdate(payload);
                break;

            case 'drone_lost':
                self._handleDroneLost(payload);
                break;

            case 'tap_status':
                self._handleTapStatus(payload);
                break;

            case 'alert':
                self._handleAlert(payload);
                break;

            case 'system_refresh':
                // Server resumed after suspend — force full data refresh
                console.log('Skylens: System refresh event, re-fetching data');
                self.addEvent('Server reconnected after resume');
                NzUI.hideConnectionError();
                self._fetchData();
                break;

            default:
                // Unknown message type, log for debugging
                console.debug('Skylens: Unknown WS message type:', type, payload);
        }
    },

    _handleDroneNew: function (drone) {
        var self = this;
        var id = drone.identifier || drone.mac;
        if (!id) return;

        // Add to local state if not already present
        var exists = self.state.uavs.some(function (u) {
            return (u.identifier || u.mac) === id;
        });

        if (!exists) {
            self.state.uavs.push(drone);
            self._invalidateUavCache();
            var isCtrl = drone.is_controller === true;
            if (!isCtrl || self.state.showControllers) {
                self.addEvent('Contact acquired: <b>' + self._esc(drone.designation || id) + '</b>');
                self.playSound();
                self.sendNotification('New Contact', drone.designation || id, 'new-' + id);
            }
        } else {
            // Update existing (state-only, render handled by rAF flush)
            self._applyDroneUpdate(drone);
        }
        // Render scheduling handled by _flushWsQueue
    },

    _handleDroneUpdate: function (drone) {
        var self = this;
        var id = drone.identifier || drone.mac;
        if (!id) return;

        // Find and update in local state
        var found = false;
        for (var i = 0; i < self.state.uavs.length; i++) {
            var u = self.state.uavs[i];
            if ((u.identifier || u.mac) === id) {
                // Merge update into existing drone
                Object.assign(self.state.uavs[i], drone);
                found = true;
                break;
            }
        }

        if (!found) {
            // Treat as new
            self._handleDroneNew(drone);
            return;
        }

        // Invalidate cache and schedule THROTTLED map + UI refresh
        self._invalidateUavCache();
        self._scheduleMapRefresh();
        self.scheduleUiRefresh();
    },

    /* Throttled map refresh - coalesces rapid WebSocket updates into batched renders.
       Max ~3Hz (333ms) to prevent visual jitter from per-drone updates. */
    _scheduleMapRefresh: function () {
        var self = this;
        self._mapDirty = true;
        if (self._mapRefreshScheduled) return;

        var now = Date.now();
        var timeSinceLast = now - self._lastMapRefresh;
        var delay = Math.max(0, 333 - timeSinceLast);

        self._mapRefreshScheduled = true;
        setTimeout(function () {
            self._mapRefreshScheduled = false;
            if (self._mapDirty) {
                self._mapDirty = false;
                self._lastMapRefresh = Date.now();
                NzMap.updateDrones(self.state.uavs, self.state.selectedDrone);
            }
        }, delay);
    },

    /* Throttled UI refresh - coalesces rapid updates */
    scheduleUiRefresh: function () {
        var self = this;
        if (self._uiRefreshScheduled) return;

        var now = Date.now();
        var timeSinceLast = now - self._lastUiRefresh;
        var delay = Math.max(0, 200 - timeSinceLast);  // At least 200ms between refreshes

        self._uiRefreshScheduled = true;
        setTimeout(function () {
            self._uiRefreshScheduled = false;
            self._lastUiRefresh = Date.now();
            self.refreshUI();
        }, delay);
    },

    _handleDroneLost: function (payload) {
        var self = this;
        var id = payload.identifier || payload;

        // Mark drone as lost in local state
        for (var i = 0; i < self.state.uavs.length; i++) {
            var u = self.state.uavs[i];
            if ((u.identifier || u.mac) === id) {
                u._contactStatus = 'lost';
                var isCtrl = u.is_controller === true;
                if (!isCtrl || self.state.showControllers) {
                    var name = u.designation || id;
                    self.addEvent('Contact lost: <b>' + self._esc(name) + '</b>');
                    self.playSound();
                }
                break;
            }
        }
        // Render scheduling handled by _flushWsQueue
    },

    _handleTapStatus: function (tap) {
        var self = this;
        var id = tap.tap_uuid || tap.id;
        if (!id) return;

        // Find and update in local state
        var found = false;
        for (var i = 0; i < self.state.taps.length; i++) {
            if (self.state.taps[i].tap_uuid === id || self.state.taps[i].id === id) {
                Object.assign(self.state.taps[i], tap);
                found = true;
                break;
            }
        }

        if (!found) {
            self.state.taps.push(tap);
            self.addEvent('Sensor <b>' + self._esc(tap.tap_name || tap.name || 'unknown') + '</b> online');
            self.playSound();
        }

        NzMap.updateTaps(self.state.taps);
        // UI render scheduling handled by _flushWsQueue
    },

    _handleAlert: function (alert) {
        var self = this;

        // Add to alerts
        self.state.alerts.unshift(alert);

        // Keep alerts list bounded
        if (self.state.alerts.length > 100) {
            self.state.alerts.length = 100;
        }

        self.addEvent('Alert: <b>' + self._esc(alert.message || alert.type) + '</b>');
        self.playSound();

        if (alert.priority === 'CRITICAL' || alert.priority === 'HIGH') {
            self.sendNotification(alert.priority + ' Alert', alert.message || alert.type, 'alert-' + alert.id);
        }

        // Route through throttled UI refresh instead of immediate render
        self.scheduleUiRefresh();
    },

    /* ── geolocation with graceful error handling ── */
    _startGeolocation: function () {
        if (!navigator.geolocation) return;
        var self = this;
        this._geoWatchId = navigator.geolocation.watchPosition(
            function (pos) {
                self._geoDenied = false;
                self.userLocation = { lat: pos.coords.latitude, lng: pos.coords.longitude };
                NzMap.setUserLocation(pos.coords.latitude, pos.coords.longitude);
            },
            function (err) {
                if (err.code === 1) {
                    self._geoDenied = true;
                    self.userLocation = null;
                    if (self._geoWatchId != null) {
                        navigator.geolocation.clearWatch(self._geoWatchId);
                        self._geoWatchId = null;
                    }
                    console.warn('Skylens: Geolocation permission denied');
                } else if (err.code === 2) {
                    console.warn('Skylens: Geolocation position unavailable');
                } else if (err.code === 3) {
                    console.warn('Skylens: Geolocation timed out');
                }
            },
            { enableHighAccuracy: false, timeout: 10000, maximumAge: 30000 }
        );
    },

    /* ═══════════════════════════════════════
       DRONE DATA — API returns ALL drones from DB
       with _contactStatus: 'active' | 'lost'
       ═══════════════════════════════════════ */

    /* Filter UAV list for display based on showLost + showControllers toggles (cached) */
    _buildCombinedUAVs: function () {
        var uavs = this.state.uavs || [];
        var showLost = this.state.showLost;
        var showCtrl = this.state.showControllers;
        /* Build cache key from toggles + uav count + first/last ids for quick invalidation */
        var cacheKey = String(showLost) + ':' + String(showCtrl) + ':' + uavs.length;
        if (uavs.length > 0) {
            cacheKey += ':' + (uavs[0].identifier || uavs[0].mac || '');
            if (uavs.length > 1) cacheKey += ':' + (uavs[uavs.length - 1].identifier || uavs[uavs.length - 1].mac || '');
        }
        if (this._cachedFilteredUavsKey === cacheKey && this._cachedFilteredUavs) {
            return this._cachedFilteredUavs;
        }
        var result = uavs.filter(function (u) {
            if (!showLost && u._contactStatus === 'lost') return false;
            if (!showCtrl && u.is_controller === true) return false;
            return true;
        });
        this._cachedFilteredUavs = result;
        this._cachedFilteredUavsKey = cacheKey;
        return result;
    },

    /* Invalidate filtered UAV cache (call when data changes) */
    _invalidateUavCache: function () {
        this._cachedFilteredUavs = null;
        this._cachedFilteredUavsKey = null;
    },

    /* Get counts for display */
    getContactCounts: function () {
        var active = 0, lost = 0;
        (this.state.uavs || []).forEach(function (u) {
            if (u._contactStatus === 'lost') lost++;
            else active++;
        });
        return { active: active, lost: lost, dismissed: 0, total: active + lost };
    },

    /* Permanently delete a drone and all its data from the DB */
    deleteDrone: function (id) {
        if (!confirm('Permanently delete ' + id + ' and all associated data?\nThis cannot be undone.')) return;
        var self = this;
        SkylensAuth.fetch('/api/uav/' + encodeURIComponent(id) + '/delete', { method: 'POST' })
            .then(function () { self.poll(); })
            .catch(function (e) { console.error('Delete failed:', e); });
        // Immediately remove from local state and map
        if (this.state.selectedDrone === id) {
            this.state.selectedDrone = null;
            NzMap.selectDrone(null);
        }
        NzMap.removeDrone(id);
        this.state.uavs = (this.state.uavs || []).filter(function (u) {
            return (u.identifier || u.mac) !== id;
        });
        this._invalidateUavCache();
        this.refreshUI();
    },

    /* Hide a single drone (persisted in DB) */
    dismissDrone: function (id) {
        var self = this;
        SkylensAuth.fetch('/api/uav/' + encodeURIComponent(id) + '/hide', { method: 'POST' })
            .then(function () { self.poll(); })
            .catch(function (e) { console.error('Hide failed:', e); });
        // Immediately remove from local state and map
        if (this.state.selectedDrone === id) {
            this.state.selectedDrone = null;
            NzMap.selectDrone(null);
        }
        NzMap.removeDrone(id);
        this.state.uavs = (this.state.uavs || []).filter(function (u) {
            return (u.identifier || u.mac) !== id;
        });
        this._invalidateUavCache();
        this.refreshUI();
    },

    /* Hide all lost contacts — hides in DB and toggles showLost off.
       Toggling "Show Lost" back ON will call /api/uavs/unhide-all to
       restore them, making this a fully reversible action. */
    dismissAllLost: function () {
        var self = this;
        // Persist hide in DB
        SkylensAuth.fetch('/api/uavs/hide-lost', { method: 'POST' })
            .then(function () { self.poll(); })
            .catch(function (e) { console.error('Hide lost failed:', e); });
        // Toggle showLost OFF — map hides markers, UI hides list entries
        this.state.showLost = false;
        NzMap.setShowLost(false);
        // Sync UI controls
        var lostBtn = document.getElementById('tb-show-lost');
        if (lostBtn) lostBtn.classList.remove('on');
        var cb = document.getElementById('show-lost-cb');
        if (cb) cb.checked = false;
        // Deselect if selected drone was lost
        if (this.state.selectedDrone) {
            var sel = this.state.selectedDrone;
            var wasLost = (this.state.uavs || []).some(function (u) {
                return (u.identifier || u.mac) === sel && u._contactStatus === 'lost';
            });
            if (wasLost) {
                this.state.selectedDrone = null;
            }
        }
        this._invalidateUavCache();
        this.refreshUI();
    },

    /* Rebuild map from current data — map handles showLost internally
       so we just push the full dataset. */
    rebuildMapFromHistory: function () {
        NzMap.updateDrones(this.state.uavs || [], this.state.selectedDrone);
    },

    /* ── polling (fallback when WebSocket unavailable) ── */
    startPoll: function () {
        var self = this;
        if (this._pollTimer) clearInterval(this._pollTimer);
        // Polling is now a fallback mechanism - runs less frequently
        // Real-time updates come via WebSocket
        this._pollTimer = setInterval(function () { self.poll(); }, this.state.pollInterval);
    },

    restartPoll: function () { this.startPoll(); },

    poll: function () {
        if (this._pollInFlight) return;
        this._pollInFlight = true;
        var self = this, st = this.state;
        fetch('/api/status').then(function (r) {
            if (!r.ok) throw new Error('HTTP ' + r.status);
            return r.json();
        }).then(function (d) {
            // Hide error overlay on successful connection
            if (self._consecutivePollFails > 0) {
                NzUI.hideConnectionError();
            }
            self._consecutivePollFails = 0;
            st.stats = d.stats || {};
            st.uptime = d.uptime || '--';
            st.flightLogs = d.flight_logs || 0;
            st.connected = true;

            // If WebSocket is connected, only sync stats/uptime
            // Let WebSocket handle real-time drone/tap updates
            if (!self._wsConnected) {
                st.taps = d.taps || [];
                st.uavs = d.uavs || [];
                st.alerts = d.alerts_history || [];
                self._invalidateUavCache();
            } else {
                // WebSocket connected - only merge if poll has newer data
                // This handles cases where WebSocket missed an update
                var pollUavs = d.uavs || [];
                var pollTaps = d.taps || [];
                var pollAlerts = d.alerts_history || [];

                // Merge any UAVs not in current state
                var uavsAdded = false;
                pollUavs.forEach(function (pu) {
                    var id = pu.identifier || pu.mac;
                    if (!id) return;
                    var exists = st.uavs.some(function (u) {
                        return (u.identifier || u.mac) === id;
                    });
                    if (!exists) {
                        st.uavs.push(pu);
                        uavsAdded = true;
                    }
                });
                if (uavsAdded) self._invalidateUavCache();

                // Merge taps
                pollTaps.forEach(function (pt) {
                    var id = pt.tap_uuid || pt.id;
                    if (!id) return;
                    var exists = st.taps.some(function (t) {
                        return (t.tap_uuid || t.id) === id;
                    });
                    if (!exists) {
                        st.taps.push(pt);
                    }
                });

                // Sync alerts from server (authoritative)
                st.alerts = pollAlerts;
            }

            // Message rate
            var mc = (d.stats || {}).messages_received || 0;
            var now = Date.now(), dt = (now - st._prevMsgT) / 1000;
            if (dt > 10) { st._prevMsgC = mc; st._prevMsgT = now; }
            else if (dt > 0.5) { st.msgRate = ((mc - st._prevMsgC) / dt).toFixed(1); st._prevMsgC = mc; st._prevMsgT = now; }

            self.detectEvents(d);
            var wasFirstPoll = st._firstPoll;
            st._firstPoll = false;

            // Update taps FIRST so tap markers exist for range ring centering
            NzMap.updateTaps(st.taps);

            // After first poll, re-load custom ranges now that tap markers exist
            // (loadCustomRanges runs during NzMap.init but silently fails because
            // tap positions aren't available yet)
            if (wasFirstPoll && NzMap.loadCustomRanges) {
                NzMap.loadCustomRanges();
            }

            // Map receives ALL drones — it manages showLost visibility
            // internally via setShowLost(), so no client-side filtering
            // is needed here.  This fixes the bug where toggling showLost
            // off then back on would not re-show lost contacts.
            NzMap.updateDrones(st.uavs, st.selectedDrone);

            // Update Decision Support if a drone is selected
            if (typeof DecisionSupport !== 'undefined' && st.selectedDrone) {
                var selectedUav = st.uavs.find(function(u) {
                    return (u.identifier || u.mac) === st.selectedDrone;
                });
                if (selectedUav) {
                    DecisionSupport.analyze(selectedUav);
                }
            }

            self.refreshUI();

            // Zone violation detection (only active drones)
            var violations = NzMap.checkZoneViolations(st.uavs);
            var _activeViolKeys = {};
            violations.forEach(function (v) {
                var key = v.droneId + ':' + v.zoneName;
                _activeViolKeys[key] = true;
                if (!self._zoneViolationCache[key]) {
                    self._zoneViolationCache[key] = Date.now();
                    self.addEvent('<b>' + self._esc(v.designation) + '</b> entered ' + self._esc(v.zoneType) + ' zone <b>' + self._esc(v.zoneName) + '</b>');
                    self.playSound();
                    self.sendNotification('Zone Violation', v.designation + ' entered ' + v.zoneType + ' zone: ' + v.zoneName, 'zone-' + key);
                }
            });
            Object.keys(self._zoneViolationCache).forEach(function (k) {
                if (!_activeViolKeys[k]) delete self._zoneViolationCache[k];
            });

            // Desktop notifications for hostile contacts
            st.uavs.forEach(function (u) {
                var id = u.identifier || u.mac;
                if (!id) return;
                var threat = NzUI.classifyThreat(u);
                if (threat.cls === 'hostile' && !st.prevUavs[id]) {
                    self.sendNotification('Hostile Contact', (u.designation || id) + ' - Trust: ' + (u.trust_score || 0), 'hostile-' + id);
                }
            });

            // Activity log for chart
            self._activityLog.push({ t: Date.now(), n: st.uavs.length });
            if (self._activityLog.length > 150) self._activityLog.shift();

            if (typeof SkylensAuth !== 'undefined') SkylensAuth.revealPage();
        }).catch(function (err) {
            self._consecutivePollFails++;
            // Only mark disconnected if WebSocket is also down
            if (!self._wsConnected) {
                st.connected = false;
            }
            self.refreshUI();
            if (self._consecutivePollFails === 1 && !self._wsConnected) {
                self.addEvent('Connection lost: ' + (err && err.message ? err.message : 'fetch failed'));
            }
            // Show error overlay after 2 consecutive failures (only if WS also down)
            if (self._consecutivePollFails >= 2 && !self._wsConnected) {
                var backoff = Math.min(5000 * Math.pow(1.5, self._consecutivePollFails - 2), 30000);
                NzUI.showConnectionError('Connection to server lost', backoff);
            }
        }).finally(function () {
            self._pollInFlight = false;
        });
    },

    refreshUI: function () {
        var st = this.state;
        // Overwrite st.uavs with filtered list for UI components
        var display = this._buildCombinedUAVs();
        var origUavs = st.uavs;
        st.uavs = display;
        NzUI.updateThreatBar(st);
        if (st.activeTab === 'contacts') NzUI.renderUAVList(st);
        else if (st.activeTab === 'sensors') NzUI.renderSensors(st.taps);
        else if (st.activeTab === 'system') NzUI.renderSystem(st);
        NzUI.renderDetail(st);
        NzUI.renderAlerts(st);
        NzUI.updateStatusBar(st);
        NzUI.updateHUD();
        // Restore full list
        st.uavs = origUavs;
        // Update lost count badge
        var counts = this.getContactCounts();
        var lc = document.getElementById('lost-count');
        if (lc) lc.textContent = counts.lost;
        var nc = document.getElementById('n-contacts');
        if (nc) nc.textContent = counts.active + (counts.lost > 0 ? '+' + counts.lost : '');
    },

    /* ── left panel tab cycling ──────────── */
    cycleTab: function () {
        var tabs = ['contacts', 'sensors', 'system'];
        var idx = tabs.indexOf(this.state.activeTab);
        this.state.activeTab = tabs[(idx + 1) % tabs.length];
        var title = document.querySelector('.pl-t');
        if (title) title.textContent = this.state.activeTab.toUpperCase();
        var count = document.querySelector('.pl-n');
        if (count) {
            if (this.state.activeTab === 'contacts') {
                var c = this.getContactCounts();
                count.textContent = c.active + (c.lost > 0 ? '+' + c.lost : '');
            }
            else if (this.state.activeTab === 'sensors') count.textContent = (this.state.taps || []).filter(function (t) { var a = 0; try { a = Math.floor((Date.now() - new Date(t.timestamp).getTime()) / 1000); } catch (e) { a = 9999; } return a < 30; }).length;
            else count.textContent = '';
        }
        this.refreshUI();
    },

    /* ── event detection ─────────────────── */
    detectEvents: function (d) {
        var st = this.state, self = this;
        var taps = d.taps || [], uavs = d.uavs || [], s = d.stats || {};

        function uavId(u) { return u.identifier || u.mac || ''; }
        function uavName(u) {
            var id = uavId(u);
            return u.designation && u.designation !== 'UNKNOWN' ? u.designation : id;
        }

        if (st._firstPoll) {
            taps.forEach(function (t) { if (t.tap_uuid) st.prevTaps[t.tap_uuid] = t.tap_name || 'unknown'; });
            uavs.forEach(function (u) {
                var id = uavId(u);
                if (id) st.prevUavs[id] = uavName(u);
            });
            st.prevAlertCount = s.alerts ? s.alerts.alerts_generated || 0 : 0;
            self.addEvent('Monitor connected');
            return;
        }

        // Sensor online/offline
        taps.forEach(function (t) { if (t.tap_uuid && !st.prevTaps[t.tap_uuid]) { self.addEvent('Sensor <b>' + self._esc(t.tap_name || 'unknown') + '</b> online'); self.playSound(); } });
        Object.keys(st.prevTaps).forEach(function (id) { if (!taps.find(function (t) { return t.tap_uuid === id; })) { self.addEvent('Sensor <b>' + self._esc(st.prevTaps[id]) + '</b> offline'); self.playSound(); } });

        // Contact acquired/lost
        uavs.forEach(function (u) {
            var id = uavId(u);
            if (id && !st.prevUavs[id]) {
                self.addEvent('Contact acquired: <b>' + self._esc(uavName(u)) + '</b>');
                self.playSound();
            }
        });
        Object.keys(st.prevUavs).forEach(function (prevId) {
            if (!uavs.find(function (u) { return uavId(u) === prevId; })) {
                self.addEvent('Contact lost: <b>' + self._esc(st.prevUavs[prevId]) + '</b>');
                self.playSound();
            }
        });

        // Alert count change
        var ac = s.alerts ? s.alerts.alerts_generated || 0 : 0;
        if (ac > st.prevAlertCount && st.prevAlertCount > 0) { self.addEvent((ac - st.prevAlertCount) + ' new alert(s)'); self.playSound(); }

        // Rebuild prev state
        st.prevTaps = {};
        taps.forEach(function (t) { if (t.tap_uuid) st.prevTaps[t.tap_uuid] = t.tap_name || 'unknown'; });
        st.prevUavs = {};
        uavs.forEach(function (u) {
            var id = uavId(u);
            if (id) st.prevUavs[id] = uavName(u);
        });
        st.prevAlertCount = ac;
    },

    addEvent: function (msg) {
        var t = new Date().toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
        this.state.events.unshift({ t: t, msg: msg });
        if (this.state.events.length > 50) this.state.events.length = 50;
    },

    /* Flight history is now DB-backed — fetched on demand via
       /api/uav/<id>/history by NzDialogs.showHistory() */

    distanceTo: function (lat, lng) {
        if (!this.userLocation || lat == null || lng == null) return null;
        var R = 6371000;
        var dLat = (lat - this.userLocation.lat) * Math.PI / 180;
        var dLng = (lng - this.userLocation.lng) * Math.PI / 180;
        var a = Math.sin(dLat / 2) * Math.sin(dLat / 2) +
            Math.cos(this.userLocation.lat * Math.PI / 180) * Math.cos(lat * Math.PI / 180) *
            Math.sin(dLng / 2) * Math.sin(dLng / 2);
        return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
    },

    /* ── drone actions ───────────────────── */
    selectDrone: function (id) {
        this.state.selectedDrone = (this.state.selectedDrone === id) ? null : id;
        if (this.state.selectedDrone && this.state.activeTab !== 'contacts') {
            this.state.activeTab = 'contacts';
            var title = document.querySelector('.pl-t');
            if (title) title.textContent = 'CONTACTS';
        }
        NzMap.selectDrone(this.state.selectedDrone);
        this.refreshUI();
        if (this.state.selectedDrone) {
            var card = document.querySelector('.uc[data-id="' + this.state.selectedDrone + '"]');
            if (card) {
                /* Use requestAnimationFrame for smoother scroll timing */
                requestAnimationFrame(function() {
                    card.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
                });
            }
        }
    },

    locateDrone: function (id) {
        this.state.selectedDrone = id;
        NzMap.selectDrone(id);
        this.refreshUI();
    },

    showHistory: function (id) { NzDialogs.showHistory(id); },

    showTagPopup: function (event, id) { NzDialogs.showTagPopup(event, id); },

    copyDroneInfo: function (id) {
        var combined = this._buildCombinedUAVs();
        var u = combined.find(function (x) { return (x.identifier || x.mac) === id; });
        if (!u) return;
        var text = JSON.stringify(u, null, 2);
        navigator.clipboard.writeText(text).catch(function () {});
    },

    sendTelegram: function (id) {
        var self = this;
        SkylensAuth.fetch('/api/uav/' + encodeURIComponent(id) + '/telegram', {
            method: 'POST'
        }).then(function (res) { return res.json(); }).then(function (data) {
            if (data.ok) {
                self.showToast('Telegram report sent', 'success');
            } else {
                self.showToast('Telegram failed: ' + (data.error || 'unknown'), 'error');
            }
        }).catch(function (e) {
            self.showToast('Telegram failed: ' + e.message, 'error');
        });
    },

    /* ── alert actions (DB-backed) ────────── */
    ackAlert: function (alertId) {
        var self = this;
        SkylensAuth.fetch('/api/alert/' + encodeURIComponent(alertId) + '/ack', { method: 'POST' })
            .then(function () { self.poll(); })
            .catch(function (e) { console.error('Ack failed:', e); });
        // Optimistic local update
        (this.state.alerts || []).forEach(function (a) {
            if (a.id === alertId) a.acknowledged = true;
        });
        NzUI.renderAlerts(this.state);
    },

    ackAllAlerts: function () {
        var self = this;
        SkylensAuth.fetch('/api/alerts/ack-all', { method: 'POST' })
            .then(function () { self.poll(); })
            .catch(function (e) { console.error('Ack all failed:', e); });
        // Optimistic local update
        (this.state.alerts || []).forEach(function (a) {
            a.acknowledged = true;
        });
        NzUI.renderAlerts(this.state);
    },

    /* ── sound (throttled) ── */
    playSound: function () {
        if (!this.state.soundEnabled) return;
        var now = Date.now();
        if (now - this._lastSoundTime < 250) return;
        this._lastSoundTime = now;
        try {
            if (!this._audioCtx) {
                var AC = window.AudioContext || window.webkitAudioContext;
                if (!AC) return;
                this._audioCtx = new AC();
            }
            var ctx = this._audioCtx;
            if (ctx.state === 'suspended') { ctx.resume().catch(function () {}); return; }
            if (ctx.state === 'closed') { this._audioCtx = null; return; }
            var osc = ctx.createOscillator();
            var gain = ctx.createGain();
            osc.connect(gain); gain.connect(ctx.destination);
            osc.frequency.value = 880; osc.type = 'sine';
            gain.gain.value = 0.08;
            gain.gain.setValueAtTime(0.08, ctx.currentTime);
            gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + 0.12);
            osc.start(ctx.currentTime);
            osc.stop(ctx.currentTime + 0.13);
            osc.onended = function () {
                try { osc.disconnect(); gain.disconnect(); } catch (_) {}
            };
        } catch (e) {
            console.warn('Skylens: Sound unavailable:', e.message || e);
        }
    },

    /* ── keyboard shortcuts ──────────────── */
    handleKey: function (e) {
        if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT' || e.target.tagName === 'TEXTAREA') {
            if (e.key === 'Escape') e.target.blur();
            return;
        }

        var key = e.key, ctrl = e.ctrlKey, shift = e.shiftKey;

        if (key === 'Escape') {
            if (NzMap.cancelDraw()) return;
            NzDialogs.closeAll();
            if (this.state.selectedDrone) {
                this.state.selectedDrone = null;
                NzMap.selectDrone(null);
                this.refreshUI();
            }
            return;
        }

        if (ctrl && shift && key === 'F') { e.preventDefault(); document.getElementById('uav-search').focus(); return; }
        if (ctrl && key === 'f') { e.preventDefault(); NzMap.fitAll(); return; }
        if (ctrl && key === 'e') { e.preventDefault(); NzDialogs.openModal('modal-export'); return; }
        if (ctrl && key === ',') { e.preventDefault(); NzDialogs.openModal('modal-settings'); return; }

        if (key === 'F5') { e.preventDefault(); this.poll(); return; }
        if (key === 'F1') { e.preventDefault(); NzDialogs.openModal('modal-shortcuts'); return; }

        if (key === '?') { NzDialogs.openModal('modal-shortcuts'); return; }
        if (key === 't' || key === 'T') { document.getElementById('tb-trails').click(); return; }
        if (key === 'z' || key === 'Z') { document.getElementById('tb-zones').click(); return; }
        if (key === 'r' || key === 'R') { document.getElementById('tb-threats').click(); return; }
        if (key === 'o' || key === 'O') { document.getElementById('tb-operators').click(); return; }
        if (key === 'v' || key === 'V') { document.getElementById('tb-vectors').click(); return; }
        if (key === 'f') { var af = document.getElementById('tb-autofollow'); if (af) af.click(); return; }
        if (key === 'm' || key === 'M') { document.getElementById('tb-measure').click(); return; }
        if (key === 'g' || key === 'G') { document.getElementById('tb-fullscreen').click(); return; }
        if (key === 'a' || key === 'A') { this.ackAllAlerts(); return; }
        if (key === 's' || key === 'S') { document.getElementById('alert-sound').click(); return; }
        if (key === 'l' || key === 'L') { var lb = document.getElementById('tb-show-lost'); if (lb) lb.click(); return; }
        if (key === 'c' || key === 'C') { var cb2 = document.getElementById('tb-show-controllers'); if (cb2) cb2.click(); return; }
        if (key === 'd' || key === 'D') { if (this.state.selectedDrone) { this.dismissDrone(this.state.selectedDrone); return; } }

        if (key === '1') { document.getElementById('panel-left').classList.toggle('hidden'); return; }
        if (key === '2') { document.getElementById('panel-right').classList.toggle('hidden'); return; }
        if (key === '3') { document.getElementById('alert-panel').classList.toggle('hidden'); return; }

        if (key === 'ArrowDown' || key === 'ArrowUp') {
            e.preventDefault();
            var cards = document.querySelectorAll('.uc');
            if (!cards.length) return;
            var ids = []; cards.forEach(function (c) { ids.push(c.getAttribute('data-id')); });
            var idx = ids.indexOf(this.state.selectedDrone);
            if (key === 'ArrowDown') idx = Math.min(idx + 1, ids.length - 1);
            else idx = Math.max(idx - 1, 0);
            if (idx < 0) idx = 0;
            this.selectDrone(ids[idx]);
        }
    },

    /* ── desktop notifications ──────────── */
    sendNotification: function (title, body, tag) {
        if (!this._notifyPermission) return;
        try {
            new Notification('SKYLENS: ' + title, { body: body, tag: tag || '', silent: true });
        } catch (e) { /* ignore */ }
    },

    /* ── toast notifications ──────────── */
    showToast: function (msg, type) {
        var container = document.getElementById('nz-toast-container');
        if (!container) {
            container = document.createElement('div');
            container.id = 'nz-toast-container';
            container.className = 'nz-toast-container';
            document.body.appendChild(container);
        }
        var toast = document.createElement('div');
        toast.className = 'nz-toast ' + (type || 'success');
        toast.textContent = msg;
        container.appendChild(toast);
        setTimeout(function () {
            toast.classList.add('fade-out');
            setTimeout(function () { toast.remove(); }, 300);
        }, 3000);
    },

    bearingTo: function (lat1, lng1, lat2, lng2) {
        var dLng = (lng2 - lng1) * Math.PI / 180;
        var y = Math.sin(dLng) * Math.cos(lat2 * Math.PI / 180);
        var x = Math.cos(lat1 * Math.PI / 180) * Math.sin(lat2 * Math.PI / 180) -
            Math.sin(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) * Math.cos(dLng);
        var brng = Math.atan2(y, x) * 180 / Math.PI;
        return (brng + 360) % 360;
    }
};

document.addEventListener('DOMContentLoaded', function () {
    if (typeof SkylensAuth !== 'undefined' && SkylensAuth.whenReady) {
        SkylensAuth.whenReady().then(function () { App.init(); });
    } else {
        App.init();
    }
});
