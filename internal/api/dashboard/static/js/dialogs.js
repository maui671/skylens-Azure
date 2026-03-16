/* ═══════════════════════════════════════════════════════════════
   SKYLENS AIRSPACE MONITOR – Dialogs & Modals
   Settings, injection, zones, flight history, export, tags
   ═══════════════════════════════════════════════════════════════ */

var NzDialogs = (function () {

    var _histMap = null;

    /* ── HTML escape helper ───────────────── */
    var _escDiv = document.createElement('div');
    function esc(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    /* ── modal open/close ───────────────── */
    function openModal(id) {
        var bg = document.getElementById('modal-bg');
        var modal = document.getElementById(id);
        if (bg) bg.style.display = '';
        if (modal) modal.style.display = '';
    }

    function closeModal(id) {
        var bg = document.getElementById('modal-bg');
        var modal = document.getElementById(id);
        if (bg) bg.style.display = 'none';
        if (modal) modal.style.display = 'none';
    }

    function closeAll() {
        var bg = document.getElementById('modal-bg');
        if (bg) bg.style.display = 'none';
        document.querySelectorAll('.modal').forEach(function (m) { m.style.display = 'none'; });
        var tp = document.getElementById('tag-popup');
        if (tp) tp.style.display = 'none';
    }

    /* ── init ────────────────────────────── */
    function init() {
        // Close buttons
        document.querySelectorAll('[data-close]').forEach(function (btn) {
            btn.addEventListener('click', closeAll);
        });

        // Backdrop close
        document.getElementById('modal-bg').addEventListener('click', closeAll);

        // Settings tabs
        initTabs('modal-settings', 'data-st', 'data-sp');
        // Inject tabs
        initTabs('modal-inject', 'data-it', 'data-ip');

        // Settings trail slider
        var trailSlider = document.getElementById('set-trail');
        if (trailSlider) trailSlider.addEventListener('input', function () {
            var v = document.getElementById('set-trail-v');
            if (v) v.textContent = trailSlider.value;
        });

        // Settings save
        var saveBtn = document.getElementById('set-save');
        if (saveBtn) saveBtn.addEventListener('click', saveSettings);

        // Settings defaults
        var defBtn = document.getElementById('set-defaults');
        if (defBtn) defBtn.addEventListener('click', resetDefaults);

        // Injection buttons
        document.getElementById('inj-one').addEventListener('click', injectSingle);
        document.getElementById('inj-swarm').addEventListener('click', injectSwarm);
        document.getElementById('inj-clear').addEventListener('click', injectClear);

        // Presets
        document.querySelectorAll('.pbtn[data-preset]').forEach(function (btn) {
            btn.addEventListener('click', function () { injectSingle(btn.dataset.preset); });
        });

        // Zone buttons
        document.getElementById('zone-start').addEventListener('click', startZoneDraw);
        document.getElementById('zone-clear-all').addEventListener('click', function () {
            NzMap.clearAllZones();
            updateZoneList();
        });

        // Export buttons
        document.getElementById('exp-uav-csv').addEventListener('click', function () { exportData('uav', 'csv'); });
        document.getElementById('exp-uav-json').addEventListener('click', function () { exportData('uav', 'json'); });
        document.getElementById('exp-alerts-csv').addEventListener('click', function () { exportData('alerts', 'csv'); });
        document.getElementById('exp-alerts-json').addEventListener('click', function () { exportData('alerts', 'json'); });
        document.getElementById('exp-sensors').addEventListener('click', function () { exportData('sensors', 'json'); });
        document.getElementById('exp-geojson').addEventListener('click', function () { exportGeoJSON(); });
        document.getElementById('exp-all').addEventListener('click', function () { exportData('all', 'json'); });

        // History export
        document.getElementById('hist-csv').addEventListener('click', function () { exportHistory('csv'); });
        document.getElementById('hist-json').addEventListener('click', function () { exportHistory('json'); });
        document.getElementById('hist-kml').addEventListener('click', function () { exportHistory('kml'); });

        // Alert tabs
        document.getElementById('alert-tabs').addEventListener('click', function (e) {
            var btn = e.target.closest('.aptab');
            if (!btn) return;
            document.querySelectorAll('.aptab').forEach(function (t) { t.classList.remove('on'); });
            btn.classList.add('on');
            App.state.alertFilter = btn.dataset.pri;
            NzUI.renderAlerts(App.state);
        });

        // Tag popup — save to DB via API
        document.getElementById('tag-popup').addEventListener('click', function (e) {
            var btn = e.target.closest('.tag-o');
            if (!btn) return;
            var tag = btn.dataset.tag;
            if (App._tagTarget) {
                setTag(App._tagTarget, tag);
                // Optimistic local update for instant feedback
                (App.state.uavs || []).forEach(function (u) {
                    if ((u.identifier || u.mac) === App._tagTarget) {
                        u.tag = tag || null;
                    }
                });
            }
            document.getElementById('tag-popup').style.display = 'none';
            App.refreshUI();
        });

        // Range ring manager
        _initRangeManager();

        // Load settings
        loadSettings();
        loadTags();
    }

    function initTabs(modalId, tabAttr, paneAttr) {
        var modal = document.getElementById(modalId);
        if (!modal) return;
        modal.addEventListener('click', function (e) {
            var tab = e.target.closest('.m-tab');
            if (!tab || !tab.hasAttribute(tabAttr)) return;
            var val = tab.getAttribute(tabAttr);
            modal.querySelectorAll('.m-tab').forEach(function (t) {
                if (t.hasAttribute(tabAttr)) t.classList.toggle('on', t.getAttribute(tabAttr) === val);
            });
            modal.querySelectorAll('.m-pane').forEach(function (p) {
                if (p.hasAttribute(paneAttr)) p.classList.toggle('on', p.getAttribute(paneAttr) === val);
            });
        });
    }

    /* ── settings ────────────────────────── */
    var DEFAULTS = {
        tileStyle: 'satellite', trailLength: 100, pollInterval: 1000,
        threatRadius: 500, compact: false, hideRight: false,
        sound: true, alertNew: true, alertThreat: true, alertLost: true,
        showControllers: false
    };

    function loadSettings() {
        try {
            var s = JSON.parse(localStorage.getItem('nz_settings') || '{}');
            var settings = Object.assign({}, DEFAULTS, s);
            applySettings(settings);
        } catch (e) { applySettings(DEFAULTS); }
    }

    function applySettings(s) {
        var el;
        el = document.getElementById('set-style'); if (el) el.value = s.tileStyle || 'satellite';
        el = document.getElementById('set-trail'); if (el) el.value = s.trailLength || 100;
        el = document.getElementById('set-trail-v'); if (el) el.textContent = s.trailLength || 100;
        el = document.getElementById('set-poll'); if (el) el.value = s.pollInterval || 2000;
        el = document.getElementById('set-ring'); if (el) el.value = s.threatRadius || 500;
        el = document.getElementById('set-compact'); if (el) el.checked = !!s.compact;
        el = document.getElementById('set-hide-right'); if (el) el.checked = !!s.hideRight;
        el = document.getElementById('set-sound'); if (el) el.checked = s.sound !== false;
        el = document.getElementById('set-alert-new'); if (el) el.checked = s.alertNew !== false;
        el = document.getElementById('set-alert-threat'); if (el) el.checked = s.alertThreat !== false;
        el = document.getElementById('set-alert-lost'); if (el) el.checked = s.alertLost !== false;
        el = document.getElementById('set-show-controllers'); if (el) el.checked = !!s.showControllers;
        el = document.getElementById('tb-show-controllers'); if (el) el.classList.toggle('on', !!s.showControllers);

        // Apply to map
        NzMap.updateSettings({ tileStyle: s.tileStyle, maxTrail: s.trailLength, threatRadius: s.threatRadius });
        NzMap.setShowControllers(!!s.showControllers);

        // Apply to app state
        if (typeof App !== 'undefined') {
            App.state.compactView = !!s.compact;
            App.state.soundEnabled = s.sound !== false;
            App.state.pollInterval = s.pollInterval || 2000;
            App.state.showControllers = !!s.showControllers;
        }
    }

    function saveSettings() {
        var s = {
            tileStyle: document.getElementById('set-style').value,
            trailLength: parseInt(document.getElementById('set-trail').value) || 100,
            pollInterval: parseInt(document.getElementById('set-poll').value) || 2000,
            threatRadius: parseInt(document.getElementById('set-ring').value) || 500,
            compact: document.getElementById('set-compact').checked,
            hideRight: document.getElementById('set-hide-right').checked,
            sound: document.getElementById('set-sound').checked,
            alertNew: document.getElementById('set-alert-new').checked,
            alertThreat: document.getElementById('set-alert-threat').checked,
            alertLost: document.getElementById('set-alert-lost').checked,
            showControllers: document.getElementById('set-show-controllers').checked
        };
        localStorage.setItem('nz_settings', JSON.stringify(s));
        // Sync to skylens_* keys for dashboard/fleet/settings pages
        try {
            localStorage.setItem('skylens_map_style', JSON.stringify(s.tileStyle));
            localStorage.setItem('skylens_trail_length', JSON.stringify(s.trailLength));
            localStorage.setItem('skylens_range_ring_radius', JSON.stringify(s.threatRadius));
            localStorage.setItem('skylens_poll_interval', JSON.stringify(s.pollInterval / 1000));
            localStorage.setItem('skylens_sound_alerts', JSON.stringify(!!s.sound));
            localStorage.setItem('skylens_show_controllers', JSON.stringify(!!s.showControllers));
        } catch(e) {}
        if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferencesNow) SkylensAuth.savePreferencesNow();
        applySettings(s);
        closeAll();
        // Restart poll with new interval
        if (typeof App !== 'undefined') App.restartPoll();
    }

    function resetDefaults() {
        localStorage.removeItem('nz_settings');
        applySettings(DEFAULTS);
    }

    /* ── tags (DB-backed) ────────────────── */
    function loadTags() {
        // Tags now come from the API on each UAV object (u.tag).
        // No localStorage needed.
    }

    function saveTags() {
        // No-op — tags are saved via POST /api/uav/<id>/tag
    }

    function setTag(droneId, tag) {
        fetch('/api/uav/' + encodeURIComponent(droneId) + '/tag', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ tag: tag || null })
        }).then(function (r) { return r.json(); }).then(function (d) {
            if (d.ok) {
                App.showToast(tag ? 'Tagged ' + droneId + ' as ' + tag.toUpperCase() : 'Tag cleared for ' + droneId, 'success');
            } else {
                App.showToast('Tag failed: ' + (d.error || 'unknown error'), 'error');
            }
            App.poll();
        }).catch(function (e) {
            console.error('Tag failed:', e);
            App.showToast('Tag failed: ' + e.message, 'error');
        });
    }

    /* ── injection ───────────────────────── */
    function injectSingle(preset) {
        var out = document.getElementById('inj-out-1');
        if (preset) out = document.getElementById('inj-out-p') || out;
        if (out) out.textContent = 'injecting' + (preset ? ' ' + preset : '') + '...';
        var opts = { method: 'POST', headers: { 'Content-Type': 'application/json' } };
        if (preset) opts.body = JSON.stringify({ preset: preset });
        fetch('/api/test/drone', opts).then(function (r) { return r.json(); }).then(function (d) {
            if (d.ok && out) out.innerHTML = 'Injected: <b>' + esc(d.identifier) + '</b>' + (d.designation ? ' (' + esc(d.designation) + ')' : '');
            else if (out) out.textContent = 'Failed';
        }).catch(function () { if (out) out.textContent = 'Error'; });
    }

    function injectSwarm() {
        var n = parseInt(document.getElementById('swarm-n').value) || 5;
        var out = document.getElementById('inj-out-s');
        if (out) out.textContent = 'injecting ' + n + ' drones...';
        var presets = ['dji-mavic', 'dji-mini', 'parrot-anafi', 'autel-evo', 'skydio-2'];
        var ok = 0, done = 0;
        for (var i = 0; i < n; i++) {
            var preset = presets[i % presets.length];
            fetch('/api/test/drone', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ preset: preset })
            }).then(function (r) { return r.json(); }).then(function (d) { if (d.ok) ok++; }).catch(function () {}).finally(function () {
                done++;
                if (done >= n && out) out.textContent = 'Injected ' + ok + '/' + n + ' drones';
            });
        }
    }

    function injectClear() {
        var out = document.getElementById('inj-out-m');
        if (out) out.textContent = 'clearing...';
        fetch('/api/test/clear', { method: 'POST' }).then(function (r) { return r.json(); }).then(function (d) {
            if (out) out.textContent = 'Cleared ' + (d.removed || 0) + ' test drones';
        }).catch(function () { if (out) out.textContent = 'Error'; });
    }

    /* ── zones ───────────────────────────── */
    function updateZoneList() {
        var el = document.getElementById('zone-list');
        if (!el) return;
        var zones = NzMap.getZones();
        if (!zones.length) { el.innerHTML = '<div class="empty">No zones defined</div>'; return; }
        var colors = { restricted: '#FF1744', alert: '#FF6D00', warning: '#FFB300', safe: '#00E676' };
        var h = '';
        zones.forEach(function (z) {
            h += '<div class="zone-item">';
            h += '<div class="zone-color" style="background:' + (colors[z.type] || '#FFB300') + '"></div>';
            h += '<span class="zone-nm">' + z.name + '</span>';
            h += '<span class="zone-tp">' + z.type + '</span>';
            h += '<button class="zone-del" onclick="NzDialogs.deleteZone(\'' + z.id + '\')">\u2715</button>';
            h += '</div>';
        });
        el.innerHTML = h;
    }

    function deleteZone(id) {
        NzMap.removeZone(id);
        updateZoneList();
    }

    function startZoneDraw() {
        var name = document.getElementById('zone-name').value || 'Zone';
        var type = document.getElementById('zone-type').value || 'warning';
        closeAll();
        NzMap.startZoneDraw(name, type, function () {
            updateZoneList();
        });
    }

    /* ── flight history (DB-backed) ──────── */
    var _histDroneId = null;
    var _histData = null;  // cached API response for export
    var _histReplay = null;  // replay state: { playing, speed, index, animId, marker, trail }
    var _histLine = null;    // the path polyline on mini-map
    var _histStartMarker = null;
    var _histEndMarker = null;

    function showHistory(droneId) {
        _histDroneId = droneId;
        _histData = null;
        _histCurrentPositions = null;
        _histFlightsByDay = null;
        _histSelectedDay = null;
        _stopHistReplay();  // Clean up any previous replay

        openModal('modal-history');
        var nameEl = document.getElementById('hist-name');
        if (nameEl) nameEl.textContent = droneId;
        document.getElementById('hist-stats').innerHTML = '<div class="empty">Loading flight history...</div>';

        // Clear replay section and day selector
        var replayEl = document.getElementById('hist-replay-section');
        if (replayEl) replayEl.innerHTML = '';
        var daySelectorEl = document.getElementById('hist-day-selector');
        if (daySelectorEl) daySelectorEl.innerHTML = '';

        // Fetch from database via API
        fetch('/api/uav/' + encodeURIComponent(droneId) + '/history')
            .then(function (r) { return r.json(); })
            .then(function (data) {
                if (data.error || !data.positions || !data.positions.length) {
                    document.getElementById('hist-stats').innerHTML = '<div class="empty">No flight data recorded for this drone</div>';
                    return;
                }
                _histData = data;
                _renderHistory(data);
            })
            .catch(function (e) {
                document.getElementById('hist-stats').innerHTML = '<div class="empty">Failed to load history: ' + esc(e.message) + '</div>';
            });
    }

    var _histFlightsByDay = null;  // Grouped flights by date
    var _histSelectedDay = null;   // Currently selected day
    var _histCurrentPositions = null;  // Currently displayed positions (for replay)

    // Compute stats from an array of positions (haversine distance, duration, max alt/speed, count)
    function _computeStats(positions) {
        var stats = { duration_s: 0, total_distance_m: 0, max_altitude_m: 0, max_speed_ms: 0, position_count: positions.length, first_seen: null, last_seen: null };
        if (!positions.length) return stats;

        var prevLat = 0, prevLng = 0;
        for (var i = 0; i < positions.length; i++) {
            var p = positions[i];
            if (p.altitude != null && p.altitude > stats.max_altitude_m) stats.max_altitude_m = p.altitude;
            if (p.speed != null && p.speed > stats.max_speed_ms) stats.max_speed_ms = p.speed;
            if (prevLat !== 0 && prevLng !== 0 && p.lat && p.lng) {
                stats.total_distance_m += _haversine(prevLat, prevLng, p.lat, p.lng);
            }
            if (p.lat && p.lng) { prevLat = p.lat; prevLng = p.lng; }
        }

        var first = positions[0].time ? new Date(positions[0].time) : null;
        var last = positions[positions.length - 1].time ? new Date(positions[positions.length - 1].time) : null;
        if (first && last) {
            stats.duration_s = (last - first) / 1000;
            stats.first_seen = first;
            stats.last_seen = last;
        }
        return stats;
    }

    function _haversine(lat1, lng1, lat2, lng2) {
        var R = 6371000;
        var dLat = (lat2 - lat1) * Math.PI / 180;
        var dLng = (lng2 - lng1) * Math.PI / 180;
        var a = Math.sin(dLat / 2) * Math.sin(dLat / 2) +
            Math.cos(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) *
            Math.sin(dLng / 2) * Math.sin(dLng / 2);
        return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
    }

    function _renderStatsHTML(s) {
        var dur = s.duration_s || 0;
        var dist = s.total_distance_m || 0;
        var maxAlt = s.max_altitude_m || 0;
        var maxSpd = s.max_speed_ms || 0;
        var nPts = s.position_count || 0;

        var timeInfo = '';
        if (s.first_seen && s.last_seen) {
            var fmt = function(d) { return d.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }); };
            timeInfo = '<div class="hist-stat"><div class="hist-stat-v">' + fmt(s.first_seen) + ' - ' + fmt(s.last_seen) + '</div><div class="hist-stat-l">Flight Time</div></div>';
        }

        return timeInfo +
            '<div class="hist-stat"><div class="hist-stat-v">' + (dur > 60 ? (dur / 60).toFixed(1) + 'm' : dur.toFixed(0) + 's') + '</div><div class="hist-stat-l">Duration</div></div>' +
            '<div class="hist-stat"><div class="hist-stat-v">' + (dist >= 1000 ? (dist / 1000).toFixed(2) + 'km' : dist.toFixed(0) + 'm') + '</div><div class="hist-stat-l">Distance</div></div>' +
            '<div class="hist-stat"><div class="hist-stat-v">' + maxAlt.toFixed(0) + 'm</div><div class="hist-stat-l">Max Alt</div></div>' +
            '<div class="hist-stat"><div class="hist-stat-v">' + maxSpd.toFixed(1) + 'm/s</div><div class="hist-stat-l">Max Speed</div></div>' +
            '<div class="hist-stat"><div class="hist-stat-v">' + nPts + '</div><div class="hist-stat-l">Points</div></div>';
    }

    function _renderHistory(data) {
        var statsEl = document.getElementById('hist-stats');
        var daySelectorEl = document.getElementById('hist-day-selector');

        // Group positions by date
        var allPositions = data.positions || [];
        _histFlightsByDay = {};
        allPositions.forEach(function(p) {
            if (!p.time) return;
            var date = p.time.split('T')[0]; // Extract YYYY-MM-DD
            if (!_histFlightsByDay[date]) {
                _histFlightsByDay[date] = [];
            }
            _histFlightsByDay[date].push(p);
        });

        var days = Object.keys(_histFlightsByDay).sort().reverse(); // Most recent first
        _histSelectedDay = days[0] || null;

        // Render day selector if multiple days
        if (days.length > 1 && daySelectorEl) {
            var h = '<select id="hist-day-select">';
            h += '<option value="all">All Days (' + allPositions.length + ' points)</option>';
            days.forEach(function(day) {
                var pts = _histFlightsByDay[day].length;
                var label = new Date(day + 'T12:00:00').toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' });
                h += '<option value="' + day + '">' + label + ' (' + pts + ' points)</option>';
            });
            h += '</select>';
            daySelectorEl.innerHTML = h;
            document.getElementById('hist-day-select').addEventListener('change', function() {
                _histSelectedDay = this.value;
                var positions = _histSelectedDay === 'all' ? allPositions : (_histFlightsByDay[_histSelectedDay] || []);
                _histCurrentPositions = positions;
                _stopHistReplay();  // Reset replay when day changes
                // Recalculate stats for selected day
                var dayStats = _computeStats(positions);
                statsEl.innerHTML = _renderStatsHTML(dayStats);
                _updateHistoryMap(positions, data.operator);
                _renderHistReplayControls(positions);
            });
        } else if (daySelectorEl) {
            daySelectorEl.innerHTML = '';
        }

        // Use most recent day's positions for initial display
        var positions = _histSelectedDay && _histSelectedDay !== 'all' && _histFlightsByDay[_histSelectedDay]
            ? _histFlightsByDay[_histSelectedDay]
            : allPositions;
        _histCurrentPositions = positions;

        // Compute per-day stats for initial display
        var displayStats = _computeStats(positions);
        statsEl.innerHTML = _renderStatsHTML(displayStats);

        // Initialize map and render
        setTimeout(function () {
            _initHistoryMap();
            _updateHistoryMap(positions, data.operator);
        }, 300);

        // Render replay controls
        _renderHistReplayControls(positions);

        // Canvas chart
        setTimeout(function () {
            drawHistChart({
                positions: positions,
                maxAlt: maxAlt || 10,
                maxSpeed: maxSpd || 1,
            });
        }, 400);
    }

    var _histOperatorMarker = null;

    function _initHistoryMap() {
        var mapEl = document.getElementById('hist-map');
        if (!mapEl) return;
        if (_histMap) { _histMap.remove(); _histMap = null; }
        _histMap = L.map(mapEl, { zoomControl: false, attributionControl: false });
        L.tileLayer('https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}', { maxZoom: 19 }).addTo(_histMap);
    }

    function _updateHistoryMap(positions, operator) {
        if (!_histMap) return;

        // Clear existing layers
        if (_histLine) { _histMap.removeLayer(_histLine); _histLine = null; }
        if (_histStartMarker) { _histMap.removeLayer(_histStartMarker); _histStartMarker = null; }
        if (_histEndMarker) { _histMap.removeLayer(_histEndMarker); _histEndMarker = null; }
        if (_histOperatorMarker) { _histMap.removeLayer(_histOperatorMarker); _histOperatorMarker = null; }

        if (!positions || positions.length === 0) return;

        var pts = positions.map(function (p) { return [p.lat, p.lng]; });

        // Draw path
        if (pts.length > 1) {
            _histLine = L.polyline(pts, { color: '#AA00FF', weight: 3, opacity: 0.8 }).addTo(_histMap);
            // Fit bounds with appropriate zoom - use maxZoom to prevent over-zooming on small paths
            _histMap.fitBounds(_histLine.getBounds().pad(0.15), { maxZoom: 16, animate: false });
        } else if (pts.length === 1) {
            _histMap.setView(pts[0], 15, { animate: false });
        }

        // Start/end markers
        if (pts.length > 0) {
            _histStartMarker = L.circleMarker(pts[0], {
                radius: 8, color: '#00E676', fillColor: '#00E676', fillOpacity: 1, weight: 2
            }).addTo(_histMap).bindTooltip('Start', { direction: 'top' });

            if (pts.length > 1) {
                _histEndMarker = L.circleMarker(pts[pts.length - 1], {
                    radius: 8, color: '#FF1744', fillColor: '#FF1744', fillOpacity: 1, weight: 2
                }).addTo(_histMap).bindTooltip('End', { direction: 'top' });
            }
        }

        // Operator marker
        var opLat = null, opLng = null;
        if (operator && operator.lat != null && operator.lng != null) {
            opLat = operator.lat;
            opLng = operator.lng;
        } else {
            // Try to find operator position in position records
            for (var i = positions.length - 1; i >= 0; i--) {
                var pos = positions[i];
                if (pos.op_lat && pos.op_lng && pos.op_lat !== 0 && pos.op_lng !== 0) {
                    opLat = pos.op_lat;
                    opLng = pos.op_lng;
                    break;
                }
            }
        }

        if (opLat != null && opLng != null && opLat !== 0 && opLng !== 0) {
            var opIcon = L.divIcon({
                className: 'history-operator-marker',
                html: '<div class="history-operator"></div>',
                iconSize: [18, 18],
                iconAnchor: [9, 9]
            });
            _histOperatorMarker = L.marker([opLat, opLng], {
                icon: opIcon,
                interactive: true
            }).addTo(_histMap).bindTooltip('Operator', { direction: 'top' });

            // Extend map bounds to include operator
            if (_histLine) {
                var bounds = _histLine.getBounds().extend([opLat, opLng]);
                _histMap.fitBounds(bounds.pad(0.15), { maxZoom: 16, animate: false });
            } else if (pts.length === 1) {
                var bounds = L.latLngBounds([pts[0], [opLat, opLng]]);
                _histMap.fitBounds(bounds.pad(0.2), { maxZoom: 16, animate: false });
            }
        }
    }

    function _renderHistReplayControls(positions) {
        var replayEl = document.getElementById('hist-replay-section');
        if (!replayEl || positions.length < 1) return;

        // Show message if only 1 position
        if (positions.length === 1) {
            replayEl.innerHTML = '<div class="hist-single-point">Single position recorded - no path to replay</div>';
            return;
        }

        var h = '<div class="hist-replay">';
        h += '<div class="hist-replay-row">';
        h += '<button class="hist-replay-btn" id="hist-play-btn" title="Play">&#9654;</button>';
        h += '<button class="hist-replay-btn" id="hist-pause-btn" title="Pause">&#10074;&#10074;</button>';
        h += '<button class="hist-replay-btn" id="hist-stop-btn" title="Stop">&#9632;</button>';
        h += '<div class="hist-replay-timeline" id="hist-timeline"><div class="hist-replay-progress" id="hist-progress"></div></div>';
        h += '<select class="hist-replay-speed" id="hist-speed">';
        h += '<option value="1" selected>1x</option><option value="2">2x</option><option value="5">5x</option>';
        h += '<option value="10">10x</option><option value="20">20x</option></select>';
        h += '<span class="hist-replay-time" id="hist-time">0:00 / 0:00</span>';
        h += '</div>';
        h += '<div class="hist-replay-stats">';
        h += '<div class="hist-replay-stat"><div class="hist-replay-stat-v" id="hist-cur-alt">--</div><div class="hist-replay-stat-l">Altitude</div></div>';
        h += '<div class="hist-replay-stat"><div class="hist-replay-stat-v" id="hist-cur-spd">--</div><div class="hist-replay-stat-l">Speed</div></div>';
        h += '<div class="hist-replay-stat"><div class="hist-replay-stat-v" id="hist-cur-hdg">--</div><div class="hist-replay-stat-l">Heading</div></div>';
        h += '<div class="hist-replay-stat"><div class="hist-replay-stat-v" id="hist-cur-pos">--</div><div class="hist-replay-stat-l">Position</div></div>';
        h += '</div></div>';
        replayEl.innerHTML = h;

        // Bind event handlers
        document.getElementById('hist-play-btn').addEventListener('click', _playHistReplay);
        document.getElementById('hist-pause-btn').addEventListener('click', _pauseHistReplay);
        document.getElementById('hist-stop-btn').addEventListener('click', _stopHistReplay);
        document.getElementById('hist-speed').addEventListener('change', function() {
            if (_histReplay) _histReplay.speed = parseFloat(this.value) || 5;
        });
        document.getElementById('hist-timeline').addEventListener('click', _seekHistReplay);

        // Initial time display
        var dur = _histData && _histData.stats ? _histData.stats.duration_s : 0;
        document.getElementById('hist-time').textContent = '0:00 / ' + _formatTime(dur);
    }

    function _formatTime(secs) {
        if (secs < 60) return '0:' + Math.floor(secs).toString().padStart(2, '0');
        var m = Math.floor(secs / 60);
        var s = Math.floor(secs % 60);
        return m + ':' + s.toString().padStart(2, '0');
    }

    function _playHistReplay() {
        if (!_histCurrentPositions || _histCurrentPositions.length < 2) return;
        if (!_histMap) return;

        var positions = _histCurrentPositions;

        if (!_histReplay) {
            _histReplay = {
                playing: false,
                speed: parseFloat(document.getElementById('hist-speed').value) || 1,
                index: 0,
                animId: null,
                marker: null,
                operatorMarker: null,
                linkLine: null,
                trail: null,
                trailCoords: []
            };
        }

        if (_histReplay.playing) return;

        // Create drone replay marker if needed
        if (!_histReplay.marker) {
            var droneIcon = L.divIcon({
                className: 'replay-marker',
                html: '<div class="replay-drone"></div>',
                iconSize: [20, 20],
                iconAnchor: [10, 10]
            });
            _histReplay.marker = L.marker([positions[0].lat, positions[0].lng], {
                icon: droneIcon,
                zIndexOffset: 1000
            }).addTo(_histMap);
        }

        // Create operator marker - use position data or fallback to histData.operator
        var p0 = positions[0];
        var opData = null;
        if (p0.operator_lat != null && p0.operator_lng != null) {
            opData = { lat: p0.operator_lat, lng: p0.operator_lng };
        } else if (_histData.operator) {
            opData = { lat: _histData.operator.lat, lng: _histData.operator.lng };
        }

        if (opData && !_histReplay.operatorMarker) {
            var opIcon = L.divIcon({
                className: 'replay-operator-marker',
                html: '<div class="replay-operator"></div>',
                iconSize: [16, 16],
                iconAnchor: [8, 8]
            });
            _histReplay.operatorMarker = L.marker([opData.lat, opData.lng], {
                icon: opIcon,
                zIndexOffset: 900
            }).addTo(_histMap);

            // Create link line between drone and operator
            _histReplay.linkLine = L.polyline([[p0.lat, p0.lng], [opData.lat, opData.lng]], {
                color: '#FFB300',
                weight: 2,
                opacity: 0.6,
                dashArray: '5, 5'
            }).addTo(_histMap);
        }

        // Store operator reference for tick updates
        _histReplay.operatorData = opData;

        // Create trail if needed
        if (!_histReplay.trail) {
            _histReplay.trail = L.polyline([], { color: '#00E676', weight: 3, opacity: 0.9 }).addTo(_histMap);
            _histReplay.trailCoords = [];
        }

        _histReplay.playing = true;
        document.getElementById('hist-play-btn').classList.add('playing');
        _tickHistReplay();
    }

    function _tickHistReplay() {
        if (!_histReplay || !_histReplay.playing || !_histCurrentPositions) return;

        var positions = _histCurrentPositions;
        if (_histReplay.index >= positions.length) {
            _stopHistReplay();
            return;
        }

        var p = positions[_histReplay.index];
        var dronePos = [p.lat, p.lng];

        // Move drone marker
        _histReplay.marker.setLatLng(dronePos);

        // Determine operator position - prefer per-position data, fallback to UAV record
        var opPos = null;
        if (p.operator_lat != null && p.operator_lng != null) {
            opPos = [p.operator_lat, p.operator_lng];
        } else if (_histReplay.operatorData) {
            // Use static operator position from UAV record
            opPos = [_histReplay.operatorData.lat, _histReplay.operatorData.lng];
        }

        // Update operator marker and link line if operator data exists
        if (opPos) {
            if (!_histReplay.operatorMarker) {
                var opIcon = L.divIcon({
                    className: 'replay-operator-marker',
                    html: '<div class="replay-operator"></div>',
                    iconSize: [16, 16],
                    iconAnchor: [8, 8]
                });
                _histReplay.operatorMarker = L.marker(opPos, { icon: opIcon, zIndexOffset: 900 }).addTo(_histMap);
            } else {
                _histReplay.operatorMarker.setLatLng(opPos);
            }

            if (!_histReplay.linkLine) {
                _histReplay.linkLine = L.polyline([dronePos, opPos], {
                    color: '#FFB300',
                    weight: 2,
                    opacity: 0.6,
                    dashArray: '5, 5'
                }).addTo(_histMap);
            } else {
                _histReplay.linkLine.setLatLngs([dronePos, opPos]);
            }
        }

        // Extend trail
        _histReplay.trailCoords.push(dronePos);
        _histReplay.trail.setLatLngs(_histReplay.trailCoords);

        // Update progress bar
        var pct = (_histReplay.index / (positions.length - 1)) * 100;
        document.getElementById('hist-progress').style.width = pct + '%';

        // Update time
        var curTime = p.time ? (new Date(p.time) - new Date(positions[0].time)) / 1000 : 0;
        var totalTime = _histData.stats ? _histData.stats.duration_s : 0;
        document.getElementById('hist-time').textContent = _formatTime(curTime) + ' / ' + _formatTime(totalTime);

        // Update live stats
        document.getElementById('hist-cur-alt').textContent = p.altitude != null ? p.altitude.toFixed(0) + 'm' : '--';
        document.getElementById('hist-cur-spd').textContent = p.speed != null ? p.speed.toFixed(1) + 'm/s' : '--';
        document.getElementById('hist-cur-hdg').textContent = p.heading != null ? p.heading + '°' : '--';
        var posLabel = (_histReplay.index + 1) + '/' + positions.length;
        if (opPos) posLabel += ' +OP';
        document.getElementById('hist-cur-pos').textContent = posLabel;

        // Update chart highlight
        _updateChartHighlight(_histReplay.index, positions.length);

        // Pan map to follow drone (every few points)
        if (_histReplay.index % 2 === 0) {
            _histMap.panTo(dronePos, { animate: true, duration: 0.2 });
        }

        _histReplay.index++;

        // Schedule next tick
        var delay = Math.max(30, 400 / _histReplay.speed);
        _histReplay.animId = setTimeout(_tickHistReplay, delay);
    }

    function _pauseHistReplay() {
        if (!_histReplay) return;
        _histReplay.playing = false;
        if (_histReplay.animId) {
            clearTimeout(_histReplay.animId);
            _histReplay.animId = null;
        }
        document.getElementById('hist-play-btn').classList.remove('playing');
    }

    function _stopHistReplay() {
        if (!_histReplay) return;

        _histReplay.playing = false;
        if (_histReplay.animId) {
            clearTimeout(_histReplay.animId);
            _histReplay.animId = null;
        }

        // Remove drone marker and trail from map
        if (_histReplay.marker && _histMap) {
            _histMap.removeLayer(_histReplay.marker);
        }
        if (_histReplay.trail && _histMap) {
            _histMap.removeLayer(_histReplay.trail);
        }

        // Remove operator marker and link line
        if (_histReplay.operatorMarker && _histMap) {
            _histMap.removeLayer(_histReplay.operatorMarker);
        }
        if (_histReplay.linkLine && _histMap) {
            _histMap.removeLayer(_histReplay.linkLine);
        }

        _histReplay = null;

        // Reset UI
        var playBtn = document.getElementById('hist-play-btn');
        if (playBtn) playBtn.classList.remove('playing');
        var progEl = document.getElementById('hist-progress');
        if (progEl) progEl.style.width = '0%';
        var timeEl = document.getElementById('hist-time');
        if (timeEl && _histData && _histData.stats) {
            timeEl.textContent = '0:00 / ' + _formatTime(_histData.stats.duration_s);
        }

        // Reset stats
        var altEl = document.getElementById('hist-cur-alt'); if (altEl) altEl.textContent = '--';
        var spdEl = document.getElementById('hist-cur-spd'); if (spdEl) spdEl.textContent = '--';
        var hdgEl = document.getElementById('hist-cur-hdg'); if (hdgEl) hdgEl.textContent = '--';
        var posEl = document.getElementById('hist-cur-pos'); if (posEl) posEl.textContent = '--';

        // Clear chart highlight
        _updateChartHighlight(-1, 0);

        // Reset map view
        if (_histMap && _histLine) {
            _histMap.fitBounds(_histLine.getBounds().pad(0.1));
        }
    }

    function _seekHistReplay(event) {
        if (!_histCurrentPositions || !_histCurrentPositions.length) return;

        var timeline = event.currentTarget;
        var rect = timeline.getBoundingClientRect();
        var pct = Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width));

        var positions = _histCurrentPositions;
        var newIndex = Math.floor(pct * (positions.length - 1));

        if (!_histReplay) {
            _histReplay = {
                playing: false,
                speed: parseFloat(document.getElementById('hist-speed').value) || 1,
                index: 0,
                animId: null,
                marker: null,
                operatorMarker: null,
                linkLine: null,
                trail: null,
                trailCoords: []
            };
        }

        _histReplay.index = newIndex;

        // Rebuild trail up to this point
        _histReplay.trailCoords = positions.slice(0, newIndex + 1).map(function(p) { return [p.lat, p.lng]; });

        var p = positions[newIndex];
        var dronePos = [p.lat, p.lng];

        // Update or create drone marker
        if (!_histReplay.marker && _histMap) {
            var droneIcon = L.divIcon({
                className: 'replay-marker',
                html: '<div class="replay-drone"></div>',
                iconSize: [20, 20],
                iconAnchor: [10, 10]
            });
            _histReplay.marker = L.marker(dronePos, { icon: droneIcon, zIndexOffset: 1000 }).addTo(_histMap);
        } else if (_histReplay.marker) {
            _histReplay.marker.setLatLng(dronePos);
        }

        // Update operator marker and link line if operator data exists
        if (p.operator_lat != null && p.operator_lng != null && _histMap) {
            var opPos = [p.operator_lat, p.operator_lng];

            if (!_histReplay.operatorMarker) {
                var opIcon = L.divIcon({
                    className: 'replay-operator-marker',
                    html: '<div class="replay-operator"></div>',
                    iconSize: [16, 16],
                    iconAnchor: [8, 8]
                });
                _histReplay.operatorMarker = L.marker(opPos, { icon: opIcon, zIndexOffset: 900 }).addTo(_histMap);
            } else {
                _histReplay.operatorMarker.setLatLng(opPos);
            }

            if (!_histReplay.linkLine) {
                _histReplay.linkLine = L.polyline([dronePos, opPos], {
                    color: '#FFB300',
                    weight: 2,
                    opacity: 0.6,
                    dashArray: '5, 5'
                }).addTo(_histMap);
            } else {
                _histReplay.linkLine.setLatLngs([dronePos, opPos]);
            }
        }

        // Update or create trail
        if (!_histReplay.trail && _histMap) {
            _histReplay.trail = L.polyline(_histReplay.trailCoords, { color: '#00E676', weight: 3, opacity: 0.9 }).addTo(_histMap);
        } else if (_histReplay.trail) {
            _histReplay.trail.setLatLngs(_histReplay.trailCoords);
        }

        // Update UI
        document.getElementById('hist-progress').style.width = (pct * 100) + '%';

        var curTime = p.time ? (new Date(p.time) - new Date(positions[0].time)) / 1000 : 0;
        var totalTime = _histData.stats ? _histData.stats.duration_s : 0;
        document.getElementById('hist-time').textContent = _formatTime(curTime) + ' / ' + _formatTime(totalTime);

        document.getElementById('hist-cur-alt').textContent = p.altitude != null ? p.altitude.toFixed(0) + 'm' : '--';
        document.getElementById('hist-cur-spd').textContent = p.speed != null ? p.speed.toFixed(1) + 'm/s' : '--';
        document.getElementById('hist-cur-hdg').textContent = p.heading != null ? p.heading + '°' : '--';
        var posLabel = (newIndex + 1) + '/' + positions.length;
        if (p.operator_lat != null) posLabel += ' +OP';
        document.getElementById('hist-cur-pos').textContent = posLabel;

        _updateChartHighlight(newIndex, positions.length);

        if (_histMap) _histMap.panTo(dronePos, { animate: true, duration: 0.2 });
    }

    function _updateChartHighlight(index, total) {
        var canvas = document.getElementById('hist-canvas');
        if (!canvas || !_histCurrentPositions) return;

        // Calculate max values from current positions
        var maxAlt = 10, maxSpd = 1;
        _histCurrentPositions.forEach(function(p) {
            if (p.altitude > maxAlt) maxAlt = p.altitude;
            if (p.speed > maxSpd) maxSpd = p.speed;
        });

        // Redraw chart with highlight line
        drawHistChart({
            positions: _histCurrentPositions,
            maxAlt: maxAlt,
            maxSpeed: maxSpd,
            highlightIndex: index
        });
    }

    function drawHistChart(hist) {
        var canvas = document.getElementById('hist-canvas');
        if (!canvas) return;
        var ctx = canvas.getContext('2d');
        canvas.width = canvas.offsetWidth * 2;
        canvas.height = (canvas.offsetHeight || 100) * 2;
        var w = canvas.width, h = canvas.height;
        ctx.clearRect(0, 0, w, h);

        var pts = hist.positions;
        if (pts.length < 2) return;

        var highlightIndex = hist.highlightIndex != null ? hist.highlightIndex : -1;

        // Draw altitude chart
        var maxAlt = Math.max(hist.maxAlt, 10);
        var pad = 30;
        var cw = w - pad * 2, ch = h / 2 - pad;

        ctx.fillStyle = '#4A7055';
        ctx.font = '16px Consolas';
        ctx.fillText('ALTITUDE', pad, 16);

        ctx.strokeStyle = '#2A3832';
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(pad, pad); ctx.lineTo(pad, pad + ch);
        ctx.lineTo(pad + cw, pad + ch);
        ctx.stroke();

        ctx.strokeStyle = '#4CAF50';
        ctx.lineWidth = 2;
        ctx.beginPath();
        pts.forEach(function (p, i) {
            var x = pad + (i / (pts.length - 1)) * cw;
            var y = pad + ch - ((p.altitude || 0) / maxAlt) * ch;
            if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
        });
        ctx.stroke();

        // Draw speed chart
        var yOff = h / 2;
        var maxSpd = Math.max(hist.maxSpeed, 1);

        ctx.fillStyle = '#4A7055';
        ctx.fillText('SPEED', pad, yOff + 16);

        ctx.strokeStyle = '#2A3832';
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(pad, yOff + pad); ctx.lineTo(pad, yOff + pad + ch);
        ctx.lineTo(pad + cw, yOff + pad + ch);
        ctx.stroke();

        ctx.strokeStyle = '#FFB300';
        ctx.lineWidth = 2;
        ctx.beginPath();
        pts.forEach(function (p, i) {
            var x = pad + (i / (pts.length - 1)) * cw;
            var y = yOff + pad + ch - (p.speed / maxSpd) * ch;
            if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
        });
        ctx.stroke();

        // Draw highlight line for current replay position
        if (highlightIndex >= 0 && highlightIndex < pts.length) {
            var hx = pad + (highlightIndex / (pts.length - 1)) * cw;
            ctx.strokeStyle = '#00E676';
            ctx.lineWidth = 2;
            ctx.setLineDash([4, 2]);
            ctx.beginPath();
            ctx.moveTo(hx, pad);
            ctx.lineTo(hx, pad + ch);
            ctx.moveTo(hx, yOff + pad);
            ctx.lineTo(hx, yOff + pad + ch);
            ctx.stroke();
            ctx.setLineDash([]);

            // Draw position dot on altitude chart
            var altY = pad + ch - ((pts[highlightIndex].altitude || 0) / maxAlt) * ch;
            ctx.fillStyle = '#00E676';
            ctx.beginPath();
            ctx.arc(hx, altY, 5, 0, Math.PI * 2);
            ctx.fill();

            // Draw position dot on speed chart
            var spdY = yOff + pad + ch - (pts[highlightIndex].speed / maxSpd) * ch;
            ctx.beginPath();
            ctx.arc(hx, spdY, 5, 0, Math.PI * 2);
            ctx.fill();
        }
    }

    /* ── export ──────────────────────────── */
    function exportData(type, format) {
        var st = (typeof App !== 'undefined') ? App.state : {};
        var data, filename;

        if (type === 'uav') {
            // Use filtered UAV list if available (from fleet page), otherwise all
            data = (typeof filteredUavs !== 'undefined' && filteredUavs.length > 0) ? filteredUavs
                 : (window.FleetPage && window.FleetPage.getFilteredUavs) ? window.FleetPage.getFilteredUavs()
                 : st.uavs || [];
            filename = 'skylens_uavs_' + new Date().toISOString().slice(0, 10);
        } else if (type === 'alerts') {
            data = st.alerts || [];
            filename = 'skylens_alerts_' + new Date().toISOString().slice(0, 10);
        } else if (type === 'sensors') {
            data = st.taps || [];
            filename = 'skylens_sensors_' + new Date().toISOString().slice(0, 10);
        } else {
            data = { uavs: st.uavs, taps: st.taps, alerts: st.alerts, stats: st.stats, uptime: st.uptime };
            filename = 'skylens_snapshot_' + new Date().toISOString().slice(0, 10);
        }

        var content, mime;
        if (format === 'csv' && Array.isArray(data) && data.length > 0) {
            var keys = Object.keys(data[0]);
            var rows = [keys.join(',')];
            data.forEach(function (d) {
                rows.push(keys.map(function (k) {
                    var v = d[k]; if (v == null) return '';
                    if (typeof v === 'object') return '"' + JSON.stringify(v).replace(/"/g, '""') + '"';
                    return '"' + String(v).replace(/"/g, '""') + '"';
                }).join(','));
            });
            content = rows.join('\n');
            mime = 'text/csv';
            filename += '.csv';
        } else {
            content = JSON.stringify(data, null, 2);
            mime = 'application/json';
            filename += '.json';
        }

        download(content, filename, mime);
        var out = document.getElementById('exp-out');
        if (out) out.textContent = 'Exported ' + filename;
    }

    function exportHistory(format) {
        if (!_histData || !_histData.positions || !_histData.positions.length) return;
        var positions = _histData.positions;
        var filename = 'flight_' + (_histDroneId || 'unknown') + '_' + new Date().toISOString().slice(0, 10);

        if (format === 'csv') {
            var rows = ['time,lat,lng,altitude,speed,heading'];
            positions.forEach(function (p) {
                rows.push([p.time, p.lat, p.lng, p.altitude, p.speed, p.heading].join(','));
            });
            download(rows.join('\n'), filename + '.csv', 'text/csv');
        } else if (format === 'kml') {
            var coords = positions.map(function (p) { return p.lng + ',' + p.lat + ',' + (p.altitude || 0); }).join(' ');
            var kml = '<?xml version="1.0" encoding="UTF-8"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>' +
                '<name>' + _histDroneId + '</name><Placemark><name>Flight Path</name><LineString><coordinates>' +
                coords + '</coordinates></LineString></Placemark></Document></kml>';
            download(kml, filename + '.kml', 'application/vnd.google-earth.kml+xml');
        } else {
            download(JSON.stringify(_histData, null, 2), filename + '.json', 'application/json');
        }
    }

    function exportGeoJSON() {
        var st = (typeof App !== 'undefined') ? App.state : {};
        var uavs = st.uavs || [];
        var features = uavs.filter(function (u) { return u.latitude != null && u.longitude != null; }).map(function (u) {
            var props = {};
            Object.keys(u).forEach(function (k) {
                if (k !== 'latitude' && k !== 'longitude') props[k] = u[k];
            });
            var feature = {
                type: 'Feature',
                geometry: { type: 'Point', coordinates: [u.longitude, u.latitude, u.altitude_geodetic || 0] },
                properties: props
            };
            // Add operator as secondary geometry in properties
            if (u.operator_latitude != null && u.operator_longitude != null) {
                props._operator_geometry = { type: 'Point', coordinates: [u.operator_longitude, u.operator_latitude, u.operator_altitude || 0] };
            }
            return feature;
        });
        var geojson = { type: 'FeatureCollection', features: features };
        var filename = 'skylens_uavs_' + new Date().toISOString().slice(0, 10) + '.geojson';
        download(JSON.stringify(geojson, null, 2), filename, 'application/geo+json');
        var out = document.getElementById('exp-out');
        if (out) out.textContent = 'Exported ' + filename + ' (' + features.length + ' features)';
    }

    function download(content, filename, mime) {
        var blob = new Blob([content], { type: mime });
        var a = document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = filename;
        a.click();
        setTimeout(function () { URL.revokeObjectURL(a.href); }, 10000);
    }

    /* ── tag popup ───────────────────────── */
    function showTagPopup(event, droneId) {
        var popup = document.getElementById('tag-popup');
        App._tagTarget = droneId;
        popup.style.display = '';
        popup.style.left = event.clientX + 'px';
        popup.style.top = event.clientY + 'px';
        // Keep in viewport
        var rect = popup.getBoundingClientRect();
        if (rect.right > window.innerWidth) popup.style.left = (window.innerWidth - rect.width - 4) + 'px';
        if (rect.bottom > window.innerHeight) popup.style.top = (window.innerHeight - rect.height - 4) + 'px';

        // Click-outside dismiss
        setTimeout(function() {
            document.addEventListener('click', function dismissTag(e) {
                if (!popup.contains(e.target)) {
                    popup.style.display = 'none';
                    document.removeEventListener('click', dismissTag);
                }
            });
        }, 0);
    }

    /* ── range ring manager ──────────────── */
    var _rangeSelectedColor = '#00E676';
    var RANGE_COLORS = ['#00E676','#00B0FF','#AA00FF','#FF4081','#FFD740','#64FFDA','#FF6E40','#7C4DFF'];

    function updateRangeList() {
        var el = document.getElementById('range-list');
        if (!el) return;
        var ranges = NzMap.getCustomRanges();
        if (!ranges.length) {
            el.innerHTML = '<div class="empty">No range rings defined</div>';
        } else {
            var h = '';
            ranges.forEach(function (r) {
                var distLabel = r.distance >= 1000 ? (r.distance / 1000).toFixed(1) + ' km' : r.distance + ' m';
                h += '<div class="zone-item">';
                h += '<div class="zone-color" style="background:' + esc(r.color) + '"></div>';
                h += '<span class="zone-nm">' + esc(r.name || distLabel) + '</span>';
                h += '<span class="zone-tp">' + esc(r.tapId) + ' \u2022 ' + distLabel + '</span>';
                h += '<button class="zone-del" onclick="NzDialogs.deleteRange(\'' + esc(r.id) + '\')">\u2715</button>';
                h += '</div>';
            });
            el.innerHTML = h;
        }

        // Always populate TAP select
        _populateRangeTapSelect();
    }

    function _populateRangeTapSelect() {
        var sel = document.getElementById('range-tap');
        if (!sel) return;
        var taps = (App.state && App.state.taps) || [];
        var current = sel.value;
        sel.innerHTML = '';
        taps.forEach(function (t) {
            if (!t.tap_uuid) return;
            var opt = document.createElement('option');
            opt.value = t.tap_uuid;
            opt.textContent = t.tap_name || t.tap_uuid;
            sel.appendChild(opt);
        });
        if (current && sel.querySelector('option[value="' + current + '"]')) {
            sel.value = current;
        }
    }

    function deleteRange(id) {
        NzMap.removeCustomRange(id);
        updateRangeList();
    }

    function _initRangeManager() {
        var addBtn = document.getElementById('range-add');
        if (addBtn) addBtn.addEventListener('click', function () {
            var tapId = document.getElementById('range-tap').value;
            var distKm = parseFloat(document.getElementById('range-dist').value);
            var name = document.getElementById('range-name').value.trim();
            if (!tapId || !distKm || distKm <= 0) return;
            var rng = {
                id: 'rng-' + Date.now(),
                name: name || '',
                tapId: tapId,
                distance: Math.round(distKm * 1000),
                color: _rangeSelectedColor,
                enabled: true
            };
            NzMap.addCustomRange(rng);
            document.getElementById('range-dist').value = '';
            document.getElementById('range-name').value = '';
            updateRangeList();
        });

        var clearBtn = document.getElementById('range-clear-all');
        if (clearBtn) clearBtn.addEventListener('click', function () {
            NzMap.clearCustomRanges();
            updateRangeList();
        });

        // Build color swatches
        var grid = document.getElementById('range-colors');
        if (grid) {
            RANGE_COLORS.forEach(function (c) {
                var sw = document.createElement('div');
                sw.className = 'range-swatch' + (c === _rangeSelectedColor ? ' sel' : '');
                sw.style.background = c;
                sw.dataset.color = c;
                sw.addEventListener('click', function () {
                    _rangeSelectedColor = c;
                    grid.querySelectorAll('.range-swatch').forEach(function (s) { s.classList.toggle('sel', s.dataset.color === c); });
                });
                grid.appendChild(sw);
            });
        }
    }

    /* ── Sightings modal ───────────────── */
    function showSightings(droneId) {
        var body = document.getElementById('sightings-body');
        if (!body) return;
        body.innerHTML = '<div style="padding:20px;color:var(--t2)">Loading...</div>';
        openModal('sightings-modal');

        fetch('/api/uav/' + encodeURIComponent(droneId) + '/sightings', { credentials: 'same-origin' })
            .then(function (r) { return r.json(); })
            .then(function (data) {
                var list = data.sightings || [];
                if (!list.length) {
                    body.innerHTML = '<div style="padding:20px;color:var(--t2)">No sighting history found.</div>';
                    return;
                }

                // Count unique days
                var days = {};
                list.forEach(function (s) { days[s.date] = true; });
                var dayCount = Object.keys(days).length;

                var h = '<div class="sight-count">' + list.length + ' sighting' + (list.length !== 1 ? 's' : '') +
                    ' across ' + dayCount + ' day' + (dayCount !== 1 ? 's' : '') + '</div>';

                h += '<table class="sight-tbl"><thead><tr>' +
                    '<th>Date</th><th>Time</th><th>Duration</th><th>Det.</th>' +
                    '<th>Max Alt</th><th>Max Spd</th><th>Signal</th><th>TAPs</th>' +
                    '</tr></thead><tbody>';

                list.forEach(function (s) {
                    var st = new Date(s.start_time);
                    var dur = s.duration_s;
                    var durStr = dur < 60 ? Math.round(dur) + 's' :
                        Math.floor(dur / 60) + 'm ' + Math.round(dur % 60) + 's';
                    var timeStr = st.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

                    h += '<tr>' +
                        '<td>' + esc(s.date) + '</td>' +
                        '<td>' + esc(timeStr) + '</td>' +
                        '<td>' + esc(durStr) + '</td>' +
                        '<td>' + s.detections + '</td>' +
                        '<td>' + (s.max_alt_m > 0 ? s.max_alt_m.toFixed(1) + ' m' : '--') + '</td>' +
                        '<td>' + (s.max_speed_ms > 0 ? s.max_speed_ms.toFixed(1) + ' m/s' : '--') + '</td>' +
                        '<td>' + s.avg_rssi + ' dBm</td>' +
                        '<td>' + (s.tap_ids ? s.tap_ids.join(', ') : '--') + '</td>' +
                        '</tr>';
                });

                h += '</tbody></table>';
                body.innerHTML = h;
            })
            .catch(function () {
                body.innerHTML = '<div style="padding:20px;color:var(--critical)">Failed to load sightings.</div>';
            });
    }

    return {
        init: init,
        openModal: openModal,
        closeModal: closeModal,
        closeAll: closeAll,
        showHistory: showHistory,
        showSightings: showSightings,
        showTagPopup: showTagPopup,
        setTag: setTag,
        updateZoneList: updateZoneList,
        deleteZone: deleteZone,
        updateRangeList: updateRangeList,
        deleteRange: deleteRange,
        saveTags: saveTags,
        loadSettings: loadSettings
    };
})();
