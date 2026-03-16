/* ═══════════════════════════════════════════════════════════════
   SKYLENS AIRSPACE MONITOR – UI Rendering
   Threat bar, UAV list, detail panel, sensors, alerts, status bar
   ═══════════════════════════════════════════════════════════════ */

var NzUI = (function () {

    /* ── render caching ──────────────────── */
    var _listFingerprint = '';   // UAV list data fingerprint
    var _listGeneration = 0;    // forces periodic timestamp refresh
    var _detailFingerprint = ''; // detail panel data fingerprint
    var _cardFingerprints = {};  // id → fingerprint for incremental card updates
    var _detailDroneId = null;   // which drone the detail panel is showing
    var _detailRefs = null;      // cached DOM refs for in-place gauge updates

    /* ── formatters ─────────────────────── */
    function ago(ts) {
        if (!ts) return 'never';
        try {
            var s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000);
            if (s < 0) return 'now';
            if (s < 60) return s + 's';
            if (s < 3600) return Math.floor(s / 60) + 'm' + (s % 60) + 's';
            if (s < 86400) return Math.floor(s / 3600) + 'h' + Math.floor((s % 3600) / 60) + 'm';
            var days = Math.floor(s / 86400);
            var hours = Math.floor((s % 86400) / 3600);
            return days + 'd' + hours + 'h';
        } catch (e) { return ts; }
    }
    function tapAge(ts) { if (!ts) return 9999; try { return Math.floor((Date.now() - new Date(ts).getTime()) / 1000); } catch (e) { return 9999; } }
    function fmt(n) { return n != null ? n.toLocaleString() : '0'; }
    function fmtB(b) {
        if (b == null) return '--';
        if (b < 1024) return b + ' B'; if (b < 1048576) return (b / 1024).toFixed(0) + ' KB';
        if (b < 1073741824) return (b / 1048576).toFixed(0) + ' MB'; return (b / 1073741824).toFixed(1) + ' GB';
    }
    function fmtUp(s) { if (s == null) return '--'; if (s < 60) return Math.floor(s) + 's'; if (s < 3600) return Math.floor(s / 60) + 'm' + Math.floor(s % 60) + 's'; return Math.floor(s / 3600) + 'h' + Math.floor((s % 3600) / 60) + 'm'; }
    function nv(v, u, d) { if (v == null) return '--'; return (typeof v === 'number' ? v.toFixed(d || 0) : String(v)) + (u || ''); }
    function cardinal(d) { if (d == null) return ''; var c = ['N','NNE','NE','ENE','E','ESE','SE','SSE','S','SSW','SW','WSW','W','WNW','NW','NNW']; return c[Math.round(d / 22.5) % 16]; }
    function fmtBearing(deg) { if (deg == null) return ''; return deg.toFixed(0) + '\u00b0 ' + cardinal(deg); }
    function trustCls(s) { return s >= 80 ? 'hi' : s >= 50 ? 'md' : 'lo'; }
    function trustColor(s) { return s >= 80 ? '#00E676' : s >= 50 ? '#FFB300' : '#FF1744'; }
    function esc(s) { if (s == null) return ''; return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
    function escJs(s) { return String(s).replace(/\\/g,'\\\\').replace(/'/g,"\\'"); }

    /* ── sanitize UTM/session IDs (reject binary garbage) ── */
    function sanitizeID(s) {
        if (!s) return '--';
        // Check if string contains non-printable characters (binary data)
        for (var i = 0; i < s.length; i++) {
            var c = s.charCodeAt(i);
            // Allow printable ASCII (32-126), common extended chars, and basic control chars
            if (c < 32 && c !== 9 && c !== 10 && c !== 13) return '--';
            if (c > 126 && c < 160) return '--';  // Extended ASCII control chars
        }
        // If string is mostly garbled (high ratio of unusual chars), hide it
        var unusual = 0;
        for (var j = 0; j < s.length; j++) {
            var cc = s.charCodeAt(j);
            if (cc > 127 || cc < 32) unusual++;
        }
        if (s.length > 0 && unusual / s.length > 0.3) return '--';
        return s;
    }

    /* ── movement detection ──────────────── */
    function movementStatus(u) {
        // Lost drones have no current movement - show last known state
        if (u._contactStatus === 'lost') return 'last-seen';
        // Prefer server-side movement classification if available
        if (u.movement) return u.movement;
        var speed = u.speed != null ? u.speed : 0;
        var vspeed = u.vertical_speed != null ? u.vertical_speed : 0;
        if (speed < 0.3 && Math.abs(vspeed) < 0.3) return 'stationary';
        if (speed < 1.5 && Math.abs(vspeed) < 0.5) return 'hovering';
        if (speed >= 15) return 'fast';
        return 'moving';
    }

    /* ── threat classification ───────────── */
    /* NOTE: This classifies based on DATA QUALITY and ANOMALIES, not actual intent.
       We cannot know if a drone is truly hostile or friendly - only if its
       RemoteID data is complete, consistent, and free of anomalies. */
    function classifyThreat(u) {
        // Ensure trust_score is always a number (defensive against type confusion)
        var trust = parseInt(u.trust_score, 10);
        if (isNaN(trust)) trust = 100;
        var flags = u.spoof_flags || [];
        var serial = u.serial_number || '';
        var designation = u.designation || '';
        // Spoofing detected or multiple anomalies - needs immediate attention
        if (trust < 30 || flags.length > 1) return { cls: 'hostile', label: 'Flagged' };
        // Some anomalies or missing critical data
        if (trust < 50 || flags.length > 0 || (!serial && designation === 'UNKNOWN')) return { cls: 'suspicious', label: 'Anomaly' };
        // Incomplete RemoteID data
        if (trust < 70 || !serial) return { cls: 'unknown', label: 'Unverified' };
        // Complete RemoteID with high trust - compliant broadcast
        if (trust >= 90 && serial) return { cls: 'friendly', label: 'Compliant' };
        // Normal operation, no issues detected
        return { cls: 'neutral', label: 'Nominal' };
    }

    /* ── drone name builder ────────────── */
    // Build a useful display name from manufacturer + model/serial (NOT designation)
    // Optional uavList: when provided, controllers linked to a UAV show the UAV's TRK number
    function buildDroneName(u, uavList) {
        var mfg = u.manufacturer && u.manufacturer !== 'UNKNOWN' ? u.manufacturer : null;
        var model = u.model && u.model !== 'UNKNOWN' && u.model !== '' ? u.model.replace(' (Unknown)', '') : null;
        if (model && mfg && model === mfg) model = null; // avoid "DJI DJI"
        var serial = u.serial_number || null;
        var ident = u.identifier || 'Unknown';
        var trk = u.track_number > 0 ? 'TRK-' + String(u.track_number).padStart(3, '0') + ' ' : '';

        // Controller linked to a UAV: show linked UAV's TRK number
        if (u.is_controller && u.linked_uav_id && uavList) {
            var linked = uavList.find(function (x) { return (x.identifier || x.mac) === u.linked_uav_id; });
            if (linked && linked.track_number > 0) {
                var linkedTrk = 'TRK-' + String(linked.track_number).padStart(3, '0');
                return trk + (mfg || 'RC') + ' RC \u2192 ' + linkedTrk;
            }
        }

        // If we have manufacturer and model: "TRK-001 DJI Mavic 3"
        if (mfg && model) return trk + mfg + ' ' + model;
        // If we have manufacturer and serial: "TRK-001 DJI • 1581F163..."
        if (mfg && serial) {
            var shortSerial = serial.length > 10 ? serial.substring(0, 10) + '…' : serial;
            return trk + mfg + ' • ' + shortSerial;
        }
        // If we just have manufacturer: "TRK-001 DJI"
        if (mfg) return trk + mfg;
        // If we have serial: show it
        if (serial) return trk + (serial.length > 14 ? serial.substring(0, 14) + '…' : serial);
        // Fallback to identifier
        return trk + (ident.length > 14 ? ident.substring(0, 14) + '…' : ident);
    }

    /* ── distance formatting ─────────────── */
    function fmtDist(meters) {
        if (meters == null) return '';
        if (meters < 1000) return meters.toFixed(0) + 'm';
        return (meters / 1000).toFixed(1) + 'km';
    }

    /* ── signal bars ────────────────────── */
    function sigBars(rssi) {
        if (rssi == null) return '';
        var n = rssi >= -50 ? 5 : rssi >= -60 ? 4 : rssi >= -70 ? 3 : rssi >= -80 ? 2 : rssi >= -90 ? 1 : 0;
        var h = '<div class="sig-bars">';
        for (var i = 0; i < 5; i++) {
            var ht = 3 + i * 2.2;
            var cls = i < n ? (n >= 3 ? 'h' : n >= 2 ? 'm' : 'l') : '';
            h += '<div class="sig-b ' + cls + '" style="height:' + ht + 'px"></div>';
        }
        return h + '</div>';
    }

    /* ── SVG gauges ─────────────────────── */
    function svgGauge(value, max, color, label, sublabel) {
        var pct = Math.min(Math.max(value / max, 0), 1);
        var r = 32, cx = 40, cy = 40;
        var circ = 2 * Math.PI * r;
        var startAngle = 135, endAngle = 405;
        var arc = circ * (endAngle - startAngle) / 360;
        var offset = arc * (1 - pct);
        return '<svg class="gauge-svg" viewBox="0 0 80 80">' +
            '<circle class="gauge-bg" cx="' + cx + '" cy="' + cy + '" r="' + r + '" stroke-dasharray="' + arc + ' ' + circ + '" stroke-dashoffset="0" transform="rotate(' + startAngle + ' ' + cx + ' ' + cy + ')"/>' +
            '<circle class="gauge-fg" cx="' + cx + '" cy="' + cy + '" r="' + r + '" stroke="' + color + '" stroke-dasharray="' + arc + ' ' + circ + '" stroke-dashoffset="' + offset + '" transform="rotate(' + startAngle + ' ' + cx + ' ' + cy + ')"/>' +
            '<text class="gauge-text" x="' + cx + '" y="' + (cy + 2) + '">' + label + '</text>' +
            '<text class="gauge-sub" x="' + cx + '" y="' + (cy + 12) + '">' + sublabel + '</text>' +
            '</svg>';
    }

    function svgCompass(heading) {
        if (heading == null) heading = 0;
        var cx = 40, cy = 40, r = 32;
        var h = '<svg class="compass-svg" viewBox="0 0 80 80">';
        h += '<circle class="compass-ring" cx="' + cx + '" cy="' + cy + '" r="' + r + '"/>';
        // Tick marks
        for (var i = 0; i < 360; i += 30) {
            var rad = i * Math.PI / 180;
            var len = (i % 90 === 0) ? 6 : 3;
            var x1 = cx + (r - len) * Math.sin(rad), y1 = cy - (r - len) * Math.cos(rad);
            var x2 = cx + r * Math.sin(rad), y2 = cy - r * Math.cos(rad);
            h += '<line class="compass-tick" x1="' + x1 + '" y1="' + y1 + '" x2="' + x2 + '" y2="' + y2 + '"/>';
        }
        // Cardinals
        var cards = [['N', 0], ['E', 90], ['S', 180], ['W', 270]];
        cards.forEach(function (c) {
            var rad = c[1] * Math.PI / 180;
            var tx = cx + (r - 12) * Math.sin(rad), ty = cy - (r - 12) * Math.cos(rad) + 2.5;
            h += '<text class="compass-cardinal' + (c[0] === 'N' ? ' n' : '') + '" x="' + tx + '" y="' + ty + '">' + c[0] + '</text>';
        });
        // Arrow
        var rad2 = heading * Math.PI / 180;
        var ax = cx + 16 * Math.sin(rad2), ay = cy - 16 * Math.cos(rad2);
        var lx = cx + 5 * Math.sin(rad2 - 2.5), ly = cy - 5 * Math.cos(rad2 - 2.5);
        var rx2 = cx + 5 * Math.sin(rad2 + 2.5), ry = cy - 5 * Math.cos(rad2 + 2.5);
        h += '<polygon class="compass-arrow" points="' + ax + ',' + ay + ' ' + lx + ',' + ly + ' ' + rx2 + ',' + ry + '"/>';
        h += '<text class="compass-text" x="' + cx + '" y="' + (cy + 4) + '">' + heading.toFixed(0) + '\u00b0</text>';
        h += '</svg>';
        return h;
    }

    function signalMeter(rssi) {
        if (rssi == null) rssi = -100;
        var pct = Math.min(Math.max((rssi + 100) / 70 * 100, 0), 100);
        var color = pct > 60 ? '#00E676' : pct > 30 ? '#FFB300' : '#FF1744';
        return '<div class="sigmeter">' +
            '<div class="sigmeter-bar"><div class="sigmeter-fill" style="height:' + pct + '%;background:' + color + '"></div></div>' +
            '<div class="sigmeter-val">' + rssi + '</div></div>';
    }

    /* ── ASTM dots ──────────────────────── */
    function astmDots(types) {
        var labels = ['Basic', 'Loc', 'Auth', 'Self', 'Sys', 'OpID'];
        var seen = {}; (types || []).forEach(function (t) { seen[t] = true; });
        var h = '<div class="astm-dots">';
        labels.forEach(function (l, i) { h += '<div class="astm-d' + (seen[i] ? ' on' : '') + '" data-label="' + l + '"></div>'; });
        return h + '</div>';
    }

    /* ── detail row/section helpers ──────── */
    function dr(l, v, c) { return '<div class="det-row"><span class="det-rl">' + l + '</span><span class="det-rv' + (c ? ' ' + c : '') + '">' + v + '</span></div>'; }
    function drc(l, v, c) { return (v && v !== '--' && v !== '') ? dr(l, v, c) : ''; }
    function ds(t, body) { return '<div class="det-sec"><div class="det-sec-t">' + t + '</div><div class="det-grid">' + body + '</div></div>'; }
    function scr(l, v, c) { return '<div class="sc-row"><span class="sc-rl">' + l + '</span><span class="sc-rv' + (c ? ' ' + c : '') + '">' + v + '</span></div>'; }
    function gauge(label, pct, txt, color) {
        return '<div class="mg"><span class="mg-l">' + label + '</span><div class="mg-tk"><div class="mg-f" style="width:' + Math.min(pct, 100) + '%;background:' + color + '"></div></div><span class="mg-v">' + txt + '</span></div>';
    }

    /* ══════════════════════════════════════
       THREAT BAR
       ══════════════════════════════════════ */
    function updateThreatBar(st) {
        var uavs = st.uavs || [];
        var critical = 0, warning = 0;
        uavs.forEach(function (u) {
            var t = u.trust_score != null ? u.trust_score : 100;
            var flags = u.spoof_flags || [];
            if (t < 50 || flags.length > 0) critical++;
            else if (t < 80) warning++;
        });
        var taps = (st.taps || []).filter(function (t) { return tapAge(t.timestamp) < 60; });
        var el;
        el = document.getElementById('thb-critical'); if (el) el.textContent = critical;
        el = document.getElementById('thb-warning'); if (el) el.textContent = warning;
        var activeCount = uavs.filter(function (u) { return u._contactStatus !== 'lost'; }).length;
        var lostCount = uavs.length - activeCount;
        el = document.getElementById('thb-active'); if (el) el.textContent = activeCount + (lostCount > 0 ? '+' + lostCount : '');
        el = document.getElementById('thb-sensors'); if (el) el.textContent = taps.length + '/' + (st.taps || []).length;

        // Pulse critical counter
        var ctrEl = document.querySelector('.thb-ctr.critical');
        if (ctrEl) ctrEl.classList.toggle('has-val', critical > 0);

        // Connection
        el = document.getElementById('conn-dot'); if (el) el.className = 'thb-cdot' + (st.connected ? ' on' : '');
        el = document.getElementById('conn-text'); if (el) el.textContent = st.connected ? 'ONLINE' : 'OFFLINE';

        // Counts
        el = document.getElementById('n-contacts'); if (el) el.textContent = uavs.length;
        el = document.getElementById('alert-count'); if (el) el.textContent = (st.alerts || []).length;
    }

    /* ══════════════════════════════════════
       UAV LIST (left panel)
       ══════════════════════════════════════ */
    /* ── build a single card's HTML (shared by full and incremental render) ── */
    function buildCardHTML(u, allUavs, st) {
        var id = u.identifier || u.mac || '?';
        var name = buildDroneName(u, allUavs);
        var trust = u.trust_score != null ? u.trust_score : 100;
        var tc = trustCls(trust);
        var isSel = st.selectedDrone === id;
        var compact = st.compactView || false;
        var src = (u.detection_source || '?').replace('WiFi', 'Wi-Fi');
        var flags = u.spoof_flags || [];
        var tag = u.tag || '';

        var isLost = u._contactStatus === 'lost';
        var h = '<div class="uc-head">';

        // Row 1: name + tag + status badge + trust
        h += '<div class="uc-r1"><span class="uc-name">' + esc(name) + '</span>';
        if (isLost) h += '<span class="uc-lost-badge">LOST</span>';
        if (tag) {
            var tagColors = { friendly: '#00E676', suspicious: '#FF6D00', hostile: '#FF1744', monitored: '#AA00FF', vip: '#FF4081', ignored: '#78909C' };
            h += '<span class="uc-tag" style="--tc:' + (tagColors[tag] || '#78909C') + '" data-tag-btn="' + id + '">' + tag.toUpperCase() + '</span>';
        }
        h += '<span class="uc-trust ' + tc + '">TRUST ' + trust + '</span></div>';

        // Row 2: telemetry
        h += '<div class="uc-r2">';
        h += '<span>' + nv(u.altitude_geodetic, 'm', 0) + ' alt</span>';
        h += '<span>' + nv(u.speed, 'm/s', 1) + '</span>';
        if (u.ground_track != null) h += '<span>' + u.ground_track.toFixed(0) + '\u00b0' + cardinal(u.ground_track) + '</span>';
        if (u.uav_type) h += '<span>' + esc(u.uav_type) + '</span>';
        h += '<span>' + ago(u.timestamp) + '</span>';
        h += '</div>';

        // Row 3: signal + source + movement + taps seeing
        var mov = movementStatus(u);
        h += '<div class="uc-r3"><div class="uc-sig">' + sigBars(u.rssi) + ' ' + nv(u.rssi, 'dBm') + '</div>';
        h += '<span class="uc-mov ' + mov + '">' + mov + '</span>';
        h += '<span class="uc-src">' + esc(src) + '</span>';
        if (u._taps_seeing != null && u._taps_seeing > 0) h += '<span class="uc-src" style="background:var(--accent-a);color:var(--accent)">' + u._taps_seeing + ' tap' + (u._taps_seeing !== 1 ? 's' : '') + '</span>';
        h += '</div>';

        // Row 4: SSID + BSSID + Channel
        if (u.ssid || u.mac || u.channel != null) {
            h += '<div class="uc-r4" style="display:flex;gap:8px;flex-wrap:wrap;font-size:0.65em;margin-top:2px;color:var(--t2)">';
            if (u.ssid) h += '<span title="SSID"><b>SSID:</b> ' + esc(u.ssid) + '</span>';
            if (u.mac) h += '<span title="BSSID"><b>BSSID:</b> ' + esc(u.mac) + '</span>';
            if (u.channel != null) h += '<span title="WiFi Channel"><b>CH:</b> ' + u.channel + '</span>';
            h += '</div>';
        }
        // Distance estimate from RSSI tracker
        if (u.distance_est_m != null) {
            h += '<div style="margin-top:2px"><span class="uc-dist">' + fmtDist(u.distance_est_m) + ' est.</span></div>';
        }

        // Distance from user
        var dist = (typeof App !== 'undefined') ? App.distanceTo(u.latitude, u.longitude) : null;
        var tapPos = (typeof NzMap !== 'undefined') ? NzMap.getTapPosition() : null;
        if (dist != null || tapPos) {
            h += '<div style="margin-top:2px">';
            if (dist != null) h += '<span class="uc-dist">' + fmtDist(dist) + ' away</span>';
            if (tapPos && u.latitude != null && u.longitude != null) {
                var brng = (typeof App !== 'undefined') ? App.bearingTo(tapPos.lat, tapPos.lng, u.latitude, u.longitude) : null;
                if (brng != null) h += '<span class="uc-dist" style="margin-left:6px">' + fmtBearing(brng) + ' from sensor</span>';
            }
            h += '</div>';
        }

        // Fingerprint & classification
        var fpConf = u.fingerprint_confidence != null ? u.fingerprint_confidence : 0;
        var fpReasons = u.fingerprint_reasons || [];
        if (fpConf > 0 || fpReasons.length > 0) {
            h += '<div style="margin-top:2px;display:flex;gap:6px;flex-wrap:wrap;font-size:0.62em">';
            var fpClr = fpConf >= 70 ? '#00E676' : fpConf >= 40 ? '#FFB300' : '#FF6D00';
            h += '<span style="color:' + fpClr + '">FP:' + fpConf + '%</span>';
            if (fpReasons.length > 0) {
                h += '<span style="color:var(--t3)">' + fpReasons.length + ' signal' + (fpReasons.length !== 1 ? 's' : '') + '</span>';
            }
            // Quick classification
            var qcKw = {"Matrice":"ENT","Mavic":"PRO","Mini":"CON","Air":"CON","Phantom":"PRO",
                "FPV":"FPV","Anafi":"PRO","EVO":"PRO","Skydio":"ENT","Wing":"DEL","Agras":"AGR","Avata":"CON"};
            var qcLabel = "";
            if (u.designation && u.designation !== "UNKNOWN") {
                var dUp2 = u.designation.toUpperCase();
                for (var qk in qcKw) { if (dUp2.indexOf(qk.toUpperCase()) !== -1) { qcLabel = qcKw[qk]; break; } }
            }
            if (qcLabel) h += '<span style="color:var(--t3);text-transform:uppercase">' + qcLabel + '</span>';
            h += '</div>';
        }

        // Flags
        if (flags.length) { h += '<div class="uc-flags">'; flags.forEach(function (f) { h += '<span class="uc-flag">' + esc(f) + '</span>'; }); h += '</div>'; }

        // Last seen for lost contacts
        if (isLost && u.last_seen) {
            h += '<div class="uc-lost-info">Last seen: ' + ago(u.last_seen) + ' ago</div>';
        }

        // Actions
        h += '<div class="uc-acts">';
        if (u.latitude != null && u.longitude != null) {
            h += '<button class="uc-btn" onclick="event.stopPropagation();App.locateDrone(\'' + escJs(id) + '\')">LOCATE</button>';
        }
        h += '<button class="uc-btn" onclick="event.stopPropagation();App.selectDrone(\'' + escJs(id) + '\')">DETAIL</button>';
        h += '<button class="uc-btn" onclick="event.stopPropagation();App.showHistory(\'' + escJs(id) + '\')">HISTORY</button>';
        h += '<button class="uc-btn" onclick="event.stopPropagation();App.showTagPopup(event,\'' + escJs(id) + '\')">TAG</button>';
        h += '<button class="uc-btn uc-btn-hide" onclick="event.stopPropagation();App.dismissDrone(\'' + escJs(id) + '\')">HIDE</button>';
        h += '<button class="uc-btn uc-btn-del" onclick="event.stopPropagation();App.deleteDrone(\'' + escJs(id) + '\')">DELETE</button>';
        h += '</div>';

        h += '</div>'; // uc-head
        return { html: h, className: 'uc thr-' + tc + (isSel ? ' sel' : '') + (compact ? ' compact' : '') + (isLost ? ' lost' : '') };
    }

    /* ── per-card fingerprint (data that triggers a card rebuild) ── */
    function cardFingerprint(u, st) {
        var id = u.identifier || u.mac || '';
        return id + '|' + (u.trust_score || 0) + '|' + (u._contactStatus || '') +
               (u.speed != null ? Math.round(u.speed) : '') + '|' + (u.rssi || '') + '|' +
               (u.tag || '') + '|' + (st.selectedDrone === id ? '1' : '0') + '|' +
               (u.altitude_geodetic != null ? Math.round(u.altitude_geodetic) : '') + '|' +
               (u._taps_seeing || 0) + '|' + (u.ground_track != null ? Math.round(u.ground_track) : '') + '|' +
               (st.compactView ? 'c' : 'd');
    }

    function renderUAVList(st) {
        var el = document.getElementById('uav-list');
        if (!el) return;
        var uavs = (st.uavs || []).slice();
        var search = (st.searchQuery || '').toLowerCase();
        var filters = st.activeFilters || {};
        var sortMode = st.sortMode || 'threat';

        // Filter
        uavs = uavs.filter(function (u) {
            var id = u.identifier || u.mac || '';
            var name = buildDroneName(u, uavs);
            if (search && name.toLowerCase().indexOf(search) === -1 && id.toLowerCase().indexOf(search) === -1) return false;
            if (filters['no-rid'] && u.serial_number) return false;
            if (filters['high-threat'] && (u.trust_score == null || u.trust_score >= 50)) return false;
            if (filters['moving'] && (u.speed == null || u.speed < 0.5)) return false;
            if (filters['high-alt'] && (u.altitude_geodetic == null || u.altitude_geodetic < 100)) return false;
            return true;
        });

        // Sort
        uavs.sort(function (a, b) {
            var ta = a.trust_score != null ? a.trust_score : 100;
            var tb = b.trust_score != null ? b.trust_score : 100;
            switch (sortMode) {
                case 'threat': return ta !== tb ? ta - tb : (b.rssi || -999) - (a.rssi || -999);
                case 'time': return new Date(b.last_seen || b.timestamp || 0) - new Date(a.last_seen || a.timestamp || 0);
                case 'signal': return (b.rssi || -999) - (a.rssi || -999);
                case 'name': return buildDroneName(a, uavs).localeCompare(buildDroneName(b, uavs));
                case 'altitude': return (b.altitude_geodetic || 0) - (a.altitude_geodetic || 0);
                default: return ta - tb;
            }
        });

        _listGeneration++;

        // Empty state
        if (!uavs.length) {
            el.innerHTML = '<div class="empty">No contacts' + (search ? ' matching "' + esc(search) + '"' : '') + '</div>';
            _cardFingerprints = {};
            return;
        }

        // Build desired ID order and per-card fingerprints
        var desiredIds = [];
        var uavMap = {};
        for (var i = 0; i < uavs.length; i++) {
            var uid = uavs[i].identifier || uavs[i].mac || '?';
            desiredIds.push(uid);
            uavMap[uid] = uavs[i];
        }

        // Index existing DOM cards
        var existingCards = {};
        var children = el.children;
        for (var ci = 0; ci < children.length; ci++) {
            var did = children[ci].getAttribute('data-id');
            if (did) existingCards[did] = children[ci];
        }

        // Remove cards not in filtered list
        var newFps = {};
        for (var eid in existingCards) {
            if (!uavMap[eid]) {
                el.removeChild(existingCards[eid]);
                delete existingCards[eid];
            }
        }

        // Timestamp refresh: every 5 cycles, force all cards to update
        var forceTimestamp = (_listGeneration % 5 === 0);

        // Create/update cards in order
        var prevNode = null;
        for (var si = 0; si < desiredIds.length; si++) {
            var cardId = desiredIds[si];
            var u = uavMap[cardId];
            var fp = cardFingerprint(u, st);
            newFps[cardId] = fp;

            var card = existingCards[cardId];
            if (card) {
                // Card exists — check if data changed
                if (fp !== _cardFingerprints[cardId] || forceTimestamp) {
                    var built = buildCardHTML(u, uavs, st);
                    card.className = built.className;
                    card.innerHTML = built.html;
                }
                // Reorder: ensure card is in correct position
                var expectedNext = prevNode ? prevNode.nextSibling : el.firstChild;
                if (card !== expectedNext) {
                    el.insertBefore(card, expectedNext);
                }
            } else {
                // New card
                var built = buildCardHTML(u, uavs, st);
                card = document.createElement('div');
                card.className = built.className;
                card.setAttribute('data-id', cardId);
                card.innerHTML = built.html;
                var insertBefore = prevNode ? prevNode.nextSibling : el.firstChild;
                el.insertBefore(card, insertBefore);
                existingCards[cardId] = card;
            }
            prevNode = card;
        }

        _cardFingerprints = newFps;
    }

    /* ══════════════════════════════════════
       DETAIL PANEL (right)
       ══════════════════════════════════════ */
    /* ── try in-place gauge update (returns true if successful) ── */
    function updateGaugesInPlace(view, u) {
        if (!_detailRefs) return false;

        var trust = u.trust_score != null ? u.trust_score : 100;
        var tColor = trustColor(trust);
        var heading = u.ground_track != null ? u.ground_track : 0;
        var rssi = u.rssi != null ? u.rssi : -100;

        // Trust gauge: update arc offset + text
        var trustFg = _detailRefs.trustFg;
        var trustText = _detailRefs.trustText;
        if (trustFg && trustText) {
            var r = 32, circ = 2 * Math.PI * r;
            var arc = circ * (405 - 135) / 360;
            var offset = arc * (1 - Math.min(Math.max(trust / 100, 0), 1));
            trustFg.setAttribute('stroke', tColor);
            trustFg.setAttribute('stroke-dashoffset', String(offset));
            trustText.textContent = String(trust);
        }

        // Compass: rotate arrow + update text
        var compassArrow = _detailRefs.compassArrow;
        var compassText = _detailRefs.compassText;
        if (compassArrow) {
            var cx = 40, cy = 40;
            var rad2 = heading * Math.PI / 180;
            var ax = cx + 16 * Math.sin(rad2), ay = cy - 16 * Math.cos(rad2);
            var lx = cx + 5 * Math.sin(rad2 - 2.5), ly = cy - 5 * Math.cos(rad2 - 2.5);
            var rx2 = cx + 5 * Math.sin(rad2 + 2.5), ry = cy - 5 * Math.cos(rad2 + 2.5);
            compassArrow.setAttribute('points', ax + ',' + ay + ' ' + lx + ',' + ly + ' ' + rx2 + ',' + ry);
        }
        if (compassText) compassText.textContent = heading.toFixed(0) + '\u00b0';

        // Signal meter: update fill height + value text
        var sigFill = _detailRefs.sigFill;
        var sigVal = _detailRefs.sigVal;
        if (sigFill) {
            var pct = Math.min(Math.max((rssi + 100) / 70 * 100, 0), 100);
            var sColor = pct > 60 ? '#00E676' : pct > 30 ? '#FFB300' : '#FF1744';
            sigFill.style.height = pct + '%';
            sigFill.style.background = sColor;
        }
        if (sigVal) sigVal.textContent = String(rssi);

        return true;
    }

    /* ── cache DOM refs for gauge elements after full render ── */
    function cacheDetailRefs(view) {
        var gauges = view.querySelector('.det-gauges');
        if (!gauges) { _detailRefs = null; return; }
        var wraps = gauges.querySelectorAll('.gauge-wrap');
        _detailRefs = {
            trustFg: wraps[0] ? wraps[0].querySelector('.gauge-fg') : null,
            trustText: wraps[0] ? wraps[0].querySelector('.gauge-text') : null,
            compassArrow: wraps[1] ? wraps[1].querySelector('.compass-arrow') : null,
            compassText: wraps[1] ? wraps[1].querySelector('.compass-text') : null,
            sigFill: wraps[2] ? wraps[2].querySelector('.sigmeter-fill') : null,
            sigVal: wraps[2] ? wraps[2].querySelector('.sigmeter-val') : null
        };
    }

    function renderDetail(st) {
        var empty = document.getElementById('detail-empty');
        var view = document.getElementById('detail-view');
        if (!st.selectedDrone) {
            _detailFingerprint = '';
            _detailDroneId = null;
            _detailRefs = null;
            if (empty) empty.style.display = '';
            if (view) view.style.display = 'none';
            return;
        }
        var u = (st.uavs || []).find(function (x) { return (x.identifier || x.mac) === st.selectedDrone; });
        if (!u) {
            _detailFingerprint = '';
            _detailDroneId = null;
            _detailRefs = null;
            if (empty) empty.style.display = '';
            if (view) view.style.display = 'none';
            return;
        }
        if (empty) empty.style.display = 'none';
        if (view) view.style.display = '';

        // Fingerprint check — skip rebuild when key telemetry unchanged
        var dfp = st.selectedDrone + '|' + (u.trust_score || 0) + '|' +
            (u.latitude != null ? u.latitude.toFixed(5) : '') + '|' +
            (u.longitude != null ? u.longitude.toFixed(5) : '') + '|' +
            (u.altitude_geodetic != null ? Math.round(u.altitude_geodetic) : '') + '|' +
            (u.speed != null ? u.speed.toFixed(1) : '') + '|' +
            (u.ground_track != null ? Math.round(u.ground_track) : '') + '|' +
            (u.rssi || '') + '|' + (u._contactStatus || '') + '|' +
            ((u.spoof_flags || []).length) + '|' + (u.tag || '') + '|' +
            (u.model || '') + '|' + (u.distance_est_m || '') + '|' +
            (u.detection_source || '') + '|' + (u.tap_id || '') + '|' +
            (u.channel || '') + '|' + (u.manufacturer || '') + '|' +
            (u.serial_number || '') + '|' + (u.classification || '');
        if (dfp === _detailFingerprint) return;
        _detailFingerprint = dfp;

        // If same drone, try in-place gauge update first
        if (_detailDroneId === st.selectedDrone && _detailRefs) {
            updateGaugesInPlace(view, u);
            // Still need to update info sections below the gauges — fall through to full rebuild
            // (gauges animate via CSS transitions, info sections are cheap to replace)
        }

        var id = u.identifier || u.mac || '?';
        var name = buildDroneName(u, st.uavs);
        var trust = u.trust_score != null ? u.trust_score : 100;
        var tc = trustCls(trust);
        var tColor = trustColor(trust);
        var flags = u.spoof_flags || [];

        var h = '';

        // Header
        var threat = classifyThreat(u);
        var detTag = u.tag || '';
        h += '<div class="det-hdr"><span class="det-name">' + esc(name) + '</span>';
        if (detTag) {
            var dtColors = { friendly: '#00E676', suspicious: '#FF6D00', hostile: '#FF1744', monitored: '#AA00FF', vip: '#FF4081', ignored: '#78909C' };
            h += '<span class="uc-tag" style="--tc:' + (dtColors[detTag] || '#78909C') + '">' + detTag.toUpperCase() + '</span>';
        }
        h += '<span class="det-class ' + threat.cls + '">' + threat.label + '</span>';
        h += '<button class="det-close" onclick="App.selectDrone(null)">\u2715</button></div>';

        // Gauges row
        h += '<div class="det-gauges">';
        h += '<div class="gauge-wrap">' + svgGauge(trust, 100, tColor, String(trust), 'TRUST') + '<div class="gauge-label">Trust</div></div>';
        h += '<div class="gauge-wrap">' + svgCompass(u.ground_track) + '<div class="gauge-label">Heading</div></div>';
        h += '<div class="gauge-wrap">' + signalMeter(u.rssi) + '<div class="gauge-label">Signal</div></div>';
        h += '</div>';

        // Info sections
        var detDist = (typeof App !== 'undefined') ? App.distanceTo(u.latitude, u.longitude) : null;
        var detTapPos = (typeof NzMap !== 'undefined') ? NzMap.getTapPosition() : null;
        var detBrng = (detTapPos && u.latitude != null && u.longitude != null && typeof App !== 'undefined') ? App.bearingTo(detTapPos.lat, detTapPos.lng, u.latitude, u.longitude) : null;
        h += ds('Position',
            dr('Latitude', nv(u.latitude, '', 6)) +
            dr('Longitude', nv(u.longitude, '', 6)) +
            dr('MGRS', MGRS.forward(u.latitude, u.longitude, 5)) +
            drc('Alt (geo)', nv(u.altitude_geodetic, ' m', 1)) +
            drc('Alt (pres)', nv(u.altitude_pressure, ' m', 1)) +
            drc('Height AGL', nv(u.height_agl, ' m', 1)) +
            drc('Height ref', u.height_reference && u.height_reference !== 'UNKNOWN' ? u.height_reference : null) +
            (detDist != null ? dr('Distance', fmtDist(detDist) + ' from you', 'c') : '') +
            (detBrng != null ? dr('Bearing', fmtBearing(detBrng) + ' from sensor') : ''));

        var movSt = movementStatus(u);
        h += ds('Movement',
            dr('Status', '<span class="uc-mov ' + movSt + '" style="display:inline">' + movSt + '</span>') +
            dr('Speed', u.speed != null ? u.speed.toFixed(1) + ' m/s (' + (u.speed * 3.6).toFixed(0) + ' km/h)' : '--') +
            drc('V/speed', nv(u.vertical_speed, ' m/s', 1)) +
            drc('Track', u.ground_track != null ? u.ground_track.toFixed(0) + '\u00b0 ' + cardinal(u.ground_track) : null) +
            drc('Distance est.', u.distance_est_m != null ? fmtDist(u.distance_est_m) : null));

        // Classification (client-side)
        var classKw = {
            "Matrice":"enterprise","M30":"enterprise","M300":"enterprise","Mavic":"prosumer",
            "Air":"consumer","Mini":"consumer","Phantom":"prosumer","Inspire":"enterprise",
            "FPV":"fpv_racing","Anafi":"prosumer","EVO":"prosumer","Skydio":"enterprise",
            "Wing":"delivery","Agras":"agricultural","Avata":"consumer"
        };
        var uavClass = "unknown";
        if (u.designation && u.designation !== "UNKNOWN") {
            var dUp = u.designation.toUpperCase();
            for (var ck in classKw) { if (dUp.indexOf(ck.toUpperCase()) !== -1) { uavClass = classKw[ck]; break; } }
        }

        // Use server-side classification if available, else fall back to client-side
        var displayClass = u.classification && u.classification !== 'UNKNOWN' && u.classification !== 'unknown'
            ? u.classification.replace(/_/g, ' ')
            : uavClass.replace(/_/g, ' ');

        h += ds('Identity',
            drc('Manufacturer', u.manufacturer && u.manufacturer !== 'UNKNOWN' ? u.manufacturer : null) +
            drc('Model', u.model && u.model !== 'UNKNOWN' && u.model !== '' ? u.model.replace(' (Unknown)', '') : null) +
            drc('Serial', u.serial_number) +
            drc('Type', u.uav_type) +
            dr('Classification', '<span style="text-transform:capitalize">' + displayClass + '</span>') +
            drc('Status', u.operational_status) +
            drc('Registration', u.registration) +
            drc('UTM ID', sanitizeID(u.utm_id) !== '--' ? sanitizeID(u.utm_id) : null) +
            drc('Session', sanitizeID(u.session_id) !== '--' ? sanitizeID(u.session_id) : null) +
            drc('EU Category', u.category_eu) +
            drc('EU Class', u.class_eu));

        // Operator section
        var hasOpPos = u.operator_latitude != null && u.operator_latitude !== 0 &&
                       u.operator_longitude != null && u.operator_longitude !== 0;
        var opAltValid = u.operator_altitude != null && u.operator_altitude > -500 && u.operator_altitude < 1000;
        if (hasOpPos) {
            h += ds('Operator',
                dr('Latitude', nv(u.operator_latitude, '', 6)) +
                dr('Longitude', nv(u.operator_longitude, '', 6)) +
                dr('MGRS', MGRS.forward(u.operator_latitude, u.operator_longitude, 5)) +
                (opAltValid ? dr('Altitude', nv(u.operator_altitude, ' m', 1)) : '') +
                drc('ID', u.operator_id) +
                drc('Location type', u.operator_location_type && u.operator_location_type !== 'OP_LOC_UNKNOWN'
                    ? u.operator_location_type.replace('OP_LOC_', '') : null));
        }

        // Security & Fingerprint
        h += '<div class="det-sec"><div class="det-sec-t">Security</div><div class="det-grid">';
        h += dr('Trust', trust, tc === 'hi' ? 'g' : tc === 'md' ? 'a' : 'r');
        h += '</div>';
        h += '<div class="det-tbar"><div class="det-tbar-f" style="width:' + trust + '%;background:' + tColor + '"></div></div>';
        h += '<div class="det-grid">';
        h += drc('Auth type', u.auth_type);
        h += drc('Auth data', u.auth_data);
        h += drc('Self-ID type', u.self_id_type != null ? String(u.self_id_type) : null);
        h += drc('Self-ID desc', u.self_id_description);
        h += '</div>';
        if (flags.length) {
            h += '<div class="det-grid">';
            h += dr('Spoof Flags', flags.length, 'r');
            flags.forEach(function (f) { h += dr('', f.replace(/_/g, ' '), 'r'); });
            h += '</div>';
        }
        // Fingerprint confidence
        var detFpConf = u.fingerprint_confidence != null ? u.fingerprint_confidence : 0;
        var detFpReasons = u.fingerprint_reasons || [];
        if (detFpConf > 0 || detFpReasons.length > 0) {
            var fpColor = detFpConf >= 70 ? '#00E676' : detFpConf >= 40 ? '#FFB300' : '#FF6D00';
            h += '<div class="det-grid">';
            h += dr('Fingerprint', detFpConf + '%');
            h += '</div>';
            h += '<div class="det-tbar"><div class="det-tbar-f" style="width:' + detFpConf + '%;background:' + fpColor + '"></div></div>';
            if (detFpReasons.length) {
                h += '<div class="det-grid">';
                detFpReasons.forEach(function (r) { h += dr('', r); });
                h += '</div>';
            }
        }
        h += '</div>';

        // Signal section
        var sensorId = u.tap_id || u.tap_uuid || '';
        var sourceStr = u.detection_source && u.detection_source !== '' && u.detection_source !== 'SOURCE_UNKNOWN'
            ? u.detection_source.replace('SOURCE_', '') : 'WiFi RemoteID';
        var tapsCount = u._taps_seeing != null ? u._taps_seeing :
            (u.range_rings && u.range_rings.length ? u.range_rings.length : (sensorId ? 1 : 0));

        h += ds('Signal',
            dr('RSSI', nv(u.rssi, ' dBm')) +
            drc('Channel', u.channel != null ? 'Ch ' + u.channel + (u.frequency_mhz ? ' (' + u.frequency_mhz + ' MHz)' : '') : null) +
            drc('SSID', u.ssid) +
            drc('BSSID', u.mac) +
            dr('Source', sourceStr) +
            drc('Sensor', sensorId) +
            drc('Taps', tapsCount > 0 ? String(tapsCount) : null));

        // Details
        var detailRows = '';
        detailRows += drc('H. accuracy', nv(u.accuracy_horizontal, ' m', 1));
        detailRows += drc('V. accuracy', nv(u.accuracy_vertical, ' m', 1));
        detailRows += drc('Baro. accuracy', nv(u.accuracy_barometer, ' m', 1));
        detailRows += drc('Spd. accuracy', nv(u.accuracy_speed, ' m/s', 1));
        if (u.area_count != null || u.area_radius != null) {
            detailRows += drc('Op. area radius', nv(u.area_radius, ' m', 0));
            detailRows += drc('Op. area ceiling', nv(u.area_ceiling, ' m', 1));
            detailRows += drc('Op. area floor', nv(u.area_floor, ' m', 1));
        }
        detailRows += dr('Identifier', u.identifier || '--');
        detailRows += drc('Designation', u.designation);
        detailRows += dr('Seen', ago(u.timestamp));
        if (u.first_seen) detailRows += dr('First seen', ago(u.first_seen));
        if (u.detection_count > 0) detailRows += dr('Detections', String(u.detection_count));
        detailRows += dr('Sightings', '<a style="color:var(--accent);cursor:pointer;text-decoration:none" onclick="event.stopPropagation();NzDialogs.showSightings(\'' + escJs(id) + '\')">View history &#9654;</a>');
        if (detailRows) {
            h += ds('Details', detailRows);
        }

        // Protocol (ASTM dots)
        h += '<div class="det-sec"><div class="det-sec-t">Protocol</div>' + astmDots(u.message_types_seen) + '</div>';

        // Action buttons
        h += '<div class="det-acts">';
        h += '<button class="det-btn" onclick="App.locateDrone(\'' + escJs(id) + '\')">CENTER MAP</button>';
        h += '<button class="det-btn" onclick="App.showHistory(\'' + escJs(id) + '\')">HISTORY</button>';
        h += '<button class="det-btn" onclick="App.copyDroneInfo(\'' + escJs(id) + '\')">COPY</button>';
        h += '<button class="det-btn" onclick="App.sendTelegram(\'' + escJs(id) + '\')">TELEGRAM</button>';
        h += '<button class="det-btn" onclick="App.showTagPopup(event,\'' + escJs(id) + '\')">TAG</button>';
        h += '<button class="det-btn det-btn-del" onclick="App.deleteDrone(\'' + escJs(id) + '\')">DELETE</button>';
        h += '</div>';

        view.innerHTML = h;
        _detailDroneId = st.selectedDrone;
        cacheDetailRefs(view);
    }

    /* ══════════════════════════════════════
       SENSORS
       ══════════════════════════════════════ */
    function renderSensors(taps) {
        var el = document.getElementById('uav-list');
        if (!el) return;
        taps = (taps || []).filter(function (t) { return tapAge(t.timestamp) < 120; });
        if (!taps.length) { el.innerHTML = '<div class="empty">No sensors connected</div>'; return; }
        taps.sort(function (a, b) { return tapAge(a.timestamp) - tapAge(b.timestamp); });

        var h = '';
        taps.forEach(function (t) {
            var secs = tapAge(t.timestamp), live = secs < 60;
            var cpuPct = t.cpu_percent != null ? t.cpu_percent : 0;
            var cpuTxt = t.cpu_percent != null ? t.cpu_percent.toFixed(0) + '%' : (t.cpu_load != null ? t.cpu_load.toFixed(1) : '--');
            var memPct = t.memory_percent || 0;
            var memTxt = t.memory_percent != null ? t.memory_percent.toFixed(0) + '%' : '--';
            var tmp = t.temperature, tmpPct = tmp != null ? Math.min(tmp / 85 * 100, 100) : 0;
            var tmpTxt = tmp != null ? tmp.toFixed(1) + '\u00b0C' : '--';
            var chans = t.channels ? t.channels.join(',') : (t.channel || '?');
            function gc(p) { return p > 80 ? '#FF1744' : p > 60 ? '#FFB300' : '#00E676'; }

            h += '<div class="sc ' + (live ? 'live' : 'down') + '">';
            h += '<div class="sc-hdr"><span class="sc-name">' + esc(t.tap_name || 'unknown') + '</span>';
            h += '<span class="sc-st ' + (live ? 'on' : 'off') + '">' + (live ? 'LIVE' : 'STALE') + '</span></div>';
            h += gauge('CPU', cpuPct, cpuTxt, gc(cpuPct));
            h += gauge('Mem', memPct, memTxt, gc(memPct));
            h += gauge('Temp', tmpPct, tmpTxt, gc(tmpPct));
            h += '<div class="sc-grid">';
            h += scr('Channel', 'ch' + (t.channel || '?'), 'c');
            h += scr('Channels', '[' + chans + ']');
            h += scr('Frames', fmt(t.frames_total));
            h += scr('Parsed', fmt(t.frames_parsed));
            h += scr('Capture', t.capture_running ? 'RUN' : 'DOWN', t.capture_running ? 'g' : 'r');
            h += scr('Disk', t.disk_free != null ? fmtB(t.disk_free) : '--');
            h += scr('Uptime', fmtUp(t.tap_uptime));
            h += scr('Seen', ago(t.timestamp));
            h += scr('Lat', t.latitude != null ? t.latitude.toFixed(4) : '--');
            h += scr('Lon', t.longitude != null ? t.longitude.toFixed(4) : '--');
            h += scr('MGRS', MGRS.format(t.latitude, t.longitude, 3, true));
            h += scr('Errors', String(t.capture_errors || 0), (t.capture_errors || 0) > 0 ? 'r' : '');
            h += '</div>';
            h += '<div class="sc-meta">' + (t.tap_uuid || '').substring(0, 8) + '... | v' + (t.version || '?') + ' | ' + (t.interface || '--') + '</div>';
            if (live && t.capture_running && (t.frames_total || 0) === 0) h += '<div class="sc-warn a">Capture running, 0 frames</div>';
            if (live && !t.capture_running) h += '<div class="sc-warn r">Capture DOWN</div>';
            h += '</div>';
        });
        el.innerHTML = h;
    }

    /* ══════════════════════════════════════
       SYSTEM INFO (in left panel)
       ══════════════════════════════════════ */
    function renderSystem(st) {
        var el = document.getElementById('uav-list');
        if (!el) return;
        var s = st.stats || {};
        var m = s.merger || {}, e = s.enrichment || {}, a = s.alerts || {};
        var r = s.rssi || {}, f = s.flight_logger || {}, w = s.writer || {};
        var fr = s.frame_router || {}, cor = s.correlator || {}, sp = s.spoof_detector || {};
        var h = '';

        // Frame Router
        h += '<div class="det-sec"><div class="det-sec-t">Frame Router</div><div class="det-grid">';
        h += scr('Frames in', fmt(fr.frames_received)) + scr('Decoded', fmt(fr.frames_decoded));
        h += scr('Errors', String(fr.decode_errors || 0), (fr.decode_errors || 0) > 0 ? 'r' : '');
        var frByType = fr.by_type || {};
        Object.keys(frByType).forEach(function (k) {
            h += scr(k.replace(/_/g, ' '), String(frByType[k]));
        });
        h += '</div></div>';

        // Correlator
        h += '<div class="det-sec"><div class="det-sec-t">Correlator</div><div class="det-grid">';
        h += scr('Reports out', fmt(cor.reports_emitted)) + scr('Flushed', fmt(cor.reports_flushed));
        h += scr('Active MACs', String(cor.active_macs || 0)) + scr('Rate limited', fmt(cor.rate_limited));
        h += '</div></div>';

        // Spoof Detector
        h += '<div class="det-sec"><div class="det-sec-t">Spoof Detector</div><div class="det-grid">';
        h += scr('Checked', fmt(sp.reports_checked)) + scr('Flags', fmt(sp.flags_raised), (sp.flags_raised || 0) > 0 ? 'r' : '');
        h += scr('Serial map', String(sp.serial_map_size || 0));
        var spFlags = sp.by_flag || {};
        Object.keys(spFlags).forEach(function (k) {
            h += scr(k.replace(/_/g, ' '), String(spFlags[k]), 'r');
        });
        h += '</div></div>';

        // Pipeline
        h += '<div class="det-sec"><div class="det-sec-t">Pipeline</div><div class="det-grid">';
        h += scr('Merger in', fmt(m.reports_in)) + scr('Merged', fmt(m.reports_merged));
        h += scr('Emitted', fmt(m.reports_emitted)) + scr('Multi-tap', fmt(m.multi_tap_drones));
        h += scr('Enrich serial', fmt(e.enriched_by_serial)) + scr('Enrich OUI', fmt(e.enriched_by_oui));
        h += scr('Enrich SSID', fmt(e.enriched_by_ssid)) + scr('Enrich FP', fmt(e.enriched_by_fingerprint));
        h += scr('Unknown', fmt(e.unknown)) + scr('Cache hits', fmt(e.cache_hits));
        h += scr('Alerts gen', fmt(a.alerts_generated)) + scr('Deduped', fmt(a.alerts_deduped));
        h += scr('RSSI samples', fmt(r.rssi_samples)) + scr('Approaches', String(r.approach_events || 0));
        h += scr('Flight rows', fmt(f.rows_written)) + scr('Active logs', String(f.active_files || 0));
        h += '</div></div>';

        h += '<div class="det-sec"><div class="det-sec-t">Node</div><div class="det-grid">';
        h += scr('Messages', fmt(s.messages_received)) + scr('Rate', (st.msgRate || '0') + '/s', 'c');
        h += scr('WiFi frames', fmt(s.wifi_frames)) + scr('Decoded', fmt(s.wifi_frames_decoded));
        h += scr('UAV reports', fmt(s.uav_reports)) + scr('Heartbeats', fmt(s.heartbeats));
        h += scr('Queue', String(s.queue_depth || 0)) + scr('Drops', fmt(s.queue_drops), (s.queue_drops || 0) > 0 ? 'a' : '');
        h += scr('Errors', String(s.errors || 0), (s.errors || 0) > 0 ? 'r' : '');
        h += '</div></div>';

        if (w && w.rows_written != null) {
            h += '<div class="det-sec"><div class="det-sec-t">Database</div><div class="det-grid">';
            h += scr('Written', fmt(w.rows_written)) + scr('Connected', w.connected ? 'yes' : 'no', w.connected ? 'g' : 'r');
            h += '</div></div>';
        }

        // Events
        h += '<div class="det-sec"><div class="det-sec-t">Events (' + (st.events || []).length + ')</div>';
        if (st.events && st.events.length) {
            st.events.slice(0, 30).forEach(function (ev) {
                h += '<div style="display:flex;gap:6px;padding:2px 0;font-size:0.62em;border-bottom:1px solid var(--border)">';
                h += '<span style="color:var(--t3);font-family:var(--mono);white-space:nowrap">' + ev.t + '</span>';
                h += '<span style="color:var(--t1)">' + ev.msg + '</span></div>';
            });
        } else { h += '<div class="empty" style="padding:6px">No events</div>'; }
        h += '</div>';

        // Activity chart
        if (typeof App !== 'undefined' && App._activityLog && App._activityLog.length > 1) {
            h += '<div class="det-sec"><div class="det-sec-t">Activity (' + App._activityLog.length + ' samples)</div>';
            h += '<div class="act-chart">';
            var log = App._activityLog;
            var maxN = 1;
            log.forEach(function (e) { if (e.n > maxN) maxN = e.n; });
            log.forEach(function (e) {
                var pct = maxN > 0 ? Math.round((e.n / maxN) * 100) : 0;
                h += '<div class="act-bar" style="height:' + Math.max(pct, 2) + '%"></div>';
            });
            h += '</div></div>';
        }

        el.innerHTML = h;
    }

    /* ══════════════════════════════════════
       ALERTS
       ══════════════════════════════════════ */
    function renderAlerts(st) {
        var el = document.getElementById('alert-list');
        if (!el) return;
        var allAlerts = st.alerts || [];
        var filter = st.alertFilter || 'all';

        // Count unacknowledged alerts per priority
        var counts = { critical: 0, high: 0, medium: 0, low: 0, info: 0 };
        allAlerts.forEach(function (a) {
            if (a.acknowledged) return;
            var p = (a.priority || 'INFO').toLowerCase();
            if (counts[p] != null) counts[p]++;
            else counts.info++;
        });
        ['critical', 'high', 'medium', 'low'].forEach(function (p) {
            var tab = document.querySelector('.aptab[data-pri="' + p + '"]');
            if (tab) tab.textContent = p.toUpperCase() + (counts[p] ? ' ' + counts[p] : '');
        });

        var alerts = allAlerts.slice();
        alerts.sort(function (a, b) {
            var ta = a.timestamp ? new Date(a.timestamp).getTime() : 0;
            var tb = b.timestamp ? new Date(b.timestamp).getTime() : 0;
            return tb - ta;
        });
        if (filter !== 'all') {
            alerts = alerts.filter(function (a) { return (a.priority || 'INFO').toLowerCase() === filter; });
        }

        if (!alerts.length) { el.innerHTML = '<div class="empty" style="padding:6px">No alerts</div>'; return; }

        var h = '';
        alerts.slice(0, 100).forEach(function (a) {
            var pri = a.priority || 'INFO';
            if (a.acknowledged) return;
            var itemCls = pri === 'CRITICAL' ? ' crit' : pri === 'HIGH' ? ' high-pri' : '';
            h += '<div class="al-item' + itemCls + '">';
            h += '<span class="al-ts">' + ago(a.timestamp) + '</span>';
            h += '<span class="al-pri ' + pri + '">' + pri + '</span>';
            h += '<span class="al-msg">' + esc(a.message || '--') + '</span>';
            if (a.id) {
                h += '<button class="al-ack" onclick="App.ackAlert(' + a.id + ')">\u2713</button>';
            }
            h += '</div>';
        });
        if (!h) h = '<div class="empty" style="padding:6px">All acknowledged</div>';
        el.innerHTML = h;
    }

    /* ══════════════════════════════════════
       STATUS BAR
       ══════════════════════════════════════ */
    function updateStatusBar(st) {
        var s = st.stats || {};
        var taps = (st.taps || []).filter(function (t) { return tapAge(t.timestamp) < 60; });
        var el;
        el = document.getElementById('sb-uavs'); if (el) el.textContent = (st.uavs || []).length;
        el = document.getElementById('sb-msgs'); if (el) el.textContent = fmt(s.messages_received);
        el = document.getElementById('sb-hb'); if (el) el.textContent = fmt(s.wifi_frames || 0);
        el = document.getElementById('sb-sensors'); if (el) el.textContent = taps.length + '/' + (st.taps || []).length;
        el = document.getElementById('sb-rate'); if (el) el.textContent = st.msgRate || '0';
        el = document.getElementById('sb-queue'); if (el) el.textContent = s.queue_depth || '0';
        el = document.getElementById('sb-alerts'); if (el) el.textContent = (st.alerts || []).length;
        el = document.getElementById('sb-node'); if (el) el.innerHTML = 'NODE: <b>' + (st.connected ? 'ACTIVE' : 'OFFLINE') + '</b>';
        el = document.getElementById('sb-uptime'); if (el) el.textContent = st.uptime || '--';
        el = document.getElementById('sb-updated'); if (el) el.textContent = new Date().toLocaleTimeString('en-US', { hour12: false });
    }

    /* ══════════════════════════════════════
       MAP HUD
       ══════════════════════════════════════ */
    function updateHUD() {
        var el = document.getElementById('map-hud');
        if (el) el.textContent = NzMap.getInfo();
    }

    /* ══════════════════════════════════════
       CONNECTION ERROR OVERLAY
       ══════════════════════════════════════ */
    var _errorOverlay = null;
    var _errorRetryCount = 0;

    function showConnectionError(message, retryIn) {
        if (!_errorOverlay) {
            _errorOverlay = document.createElement('div');
            _errorOverlay.className = 'conn-error-overlay';
            _errorOverlay.innerHTML = '<div class="error-icon">!</div>' +
                '<span class="error-msg"></span>' +
                '<span class="error-retry"></span>';
            document.body.appendChild(_errorOverlay);
        }
        _errorRetryCount++;
        var msgEl = _errorOverlay.querySelector('.error-msg');
        var retryEl = _errorOverlay.querySelector('.error-retry');
        if (msgEl) msgEl.textContent = message || 'Connection lost';
        if (retryEl) retryEl.textContent = retryIn ? 'Retrying in ' + Math.round(retryIn/1000) + 's...' : 'Reconnecting...';
        _errorOverlay.classList.remove('hidden');
    }

    function hideConnectionError() {
        if (_errorOverlay) {
            _errorOverlay.classList.add('hidden');
            _errorRetryCount = 0;
        }
    }

    /* ══════════════════════════════════════
       LOADING SKELETONS
       ══════════════════════════════════════ */
    function renderLoadingSkeleton(container, count) {
        if (!container) return;
        var h = '';
        for (var i = 0; i < (count || 3); i++) {
            h += '<div class="skeleton skeleton-card"></div>';
        }
        container.innerHTML = h;
    }

    function renderLoadingSpinner(container, message) {
        if (!container) return;
        container.innerHTML = '<div class="loading-container">' +
            '<div class="loading-spinner"></div>' +
            '<span>' + (message || 'Loading...') + '</span>' +
            '</div>';
    }

    function renderEmptyState(container, icon, title, description) {
        if (!container) return;
        container.innerHTML = '<div class="empty-state">' +
            '<div class="empty-state-icon">' + (icon || '📡') + '</div>' +
            '<div class="empty-state-title">' + (title || 'No data') + '</div>' +
            '<div class="empty-state-desc">' + (description || '') + '</div>' +
            '</div>';
    }

    return {
        updateThreatBar: updateThreatBar,
        renderUAVList: renderUAVList,
        renderDetail: renderDetail,
        renderSensors: renderSensors,
        renderSystem: renderSystem,
        renderAlerts: renderAlerts,
        updateStatusBar: updateStatusBar,
        updateHUD: updateHUD,
        // Connection status
        showConnectionError: showConnectionError,
        hideConnectionError: hideConnectionError,
        // Loading states
        renderLoadingSkeleton: renderLoadingSkeleton,
        renderLoadingSpinner: renderLoadingSpinner,
        renderEmptyState: renderEmptyState,
        // Expose for external use
        ago: ago, trustColor: trustColor, classifyThreat: classifyThreat, movementStatus: movementStatus
    };
})();
