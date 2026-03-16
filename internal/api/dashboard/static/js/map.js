/* ═══════════════════════════════════════════════════════════════
   SKYLENS AIRSPACE MONITOR – Map Module  (v2 – CircleMarker engine)
   L.circleMarker for all entities: zero zoom-jitter, instant style
   updates, batched requestAnimationFrame smooth-move, showLost mgmt.
   ═══════════════════════════════════════════════════════════════ */

var NzMap = (function () {
    var map = null, onSelect = null;

    /* ── marker & layer collections ──────── */
    var droneMarkers   = {};   // id → L.circleMarker
    var operatorMarkers = {};  // id → L.circleMarker
    var tapMarkers     = {};   // id → L.circleMarker
    var connLines      = {};   // id → L.polyline  (drone↔operator)
    var trailLines     = {};   // id → L.polyline
    var trails         = {};   // id → [[lat,lng], …]
    var threatRings    = {};   // id → L.circle
    var speedVectors   = {};   // id → L.polyline
    var headingLines   = {};   // id → L.polyline  (short heading tick)
    var historyLines   = {};   // id → L.polyline  (historical flight path)
    var historyVisible = {};   // id → boolean
    var distanceRings  = {};   // id → L.circle (RSSI-estimated distance ring)
    var ringLabels     = {};   // id → L.marker (label at north edge of ring)
    var zonePolygons   = {};
    var drawItems = [], measurePoints = [], measureLine = null, measureLabels = [];
    var hasFitted  = false;
    var _drawMode  = null;
    var _drawCancel = null;
    var _userMarker = null;
    var _autoFollow = false;
    var _rangeRings = {};
    var _zoomAnimating = false;
    var _lastFollowPan = 0;         // throttle follow-UAV panTo
    var _bestRingTap = {};          // id → tapID: hysteresis for ring TAP selection

    /* ── showLost state ──────────────────── */
    var _showLost = true;

    /* ── showControllers state ─────────── */
    var _showControllers = false;
    var _controllerIds = {};   // id → true: tracks which drone ids are controllers

    /* ── style-hash cache (skip needless setStyle) ── */
    var _styleCache = {};   // id → hash string

    /* ── smooth-move animation state ─────── */
    var _anims  = {};       // id → { m, lat0, lng0, lat1, lng1, t0, dur }
    var _animId = null;     // rAF id
    var _pendingTimeouts = {};  // id → timeout handles for cleanup

    /* ── popup throttle state ─────────────── */
    var _popupLastUpdate = {};  // id → timestamp (throttle popup rebuilds)
    var _lastDronePos = {};     // id → [lat, lng] (skip geometry if unchanged)

    /* ── layer visibility ────────────────── */
    var layers = { trails: true, zones: true, threats: true, operators: true, vectors: false };

    /* ── settings ────────────────────────── */
    var settings = {
        maxTrail: parseInt(localStorage.getItem("skylens_trail_length")) || 100,
        threatRadius: parseInt(localStorage.getItem("skylens_range_ring_radius")) || 500,
        tileStyle: (function(){ try { return JSON.parse(localStorage.getItem("skylens_map_style")); } catch(e){} return null; })() || 'satellite'
    };

    /* ── tile providers ──────────────────── */
    var TILES = {
        satellite: {
            url: 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}',
            attr: 'Esri', maxZoom: 19
        },
        terrain: {
            url: 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png',
            attr: 'OSM', maxZoom: 19
        },
        dark: {
            url: 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
            attr: 'CARTO', maxZoom: 19
        }
    };
    var baseTile = null, labelTile = null;

    /* ══════════════════════════════════════
       STYLE CONSTANTS & COLORS
       ══════════════════════════════════════ */
    /* Simplified color scheme: All drones RED, operators YELLOW, taps GREEN */
    var COLORS = {
        drone:    '#FF1744',  // Red - all UAVs
        operator: '#FFEB3B',  // Yellow - operator location
        tapLive:  '#00E676',  // Green - active sensor
        tapDead:  '#FF1744',  // Red - offline sensor
        selected: '#00FFFF',  // Cyan - selected highlight
        lost:     '#78909C',  // Gray - lost contact
        threat:   '#FF5722'   // Orange-red - active threat (approaching/fast)
    };

    /* Operator marker style - bright yellow, highlighted when selected */
    function operatorStyle(selected) {
        if (selected) {
            return { radius: 12, color: '#FFFFFF', fillColor: '#FFC107', fillOpacity: 1, weight: 4 };
        }
        return { radius: 8, color: '#FFC107', fillColor: COLORS.operator, fillOpacity: 0.85, weight: 2 };
    }
    var OP = operatorStyle(false); // Default style
    var TAP_LIVE = { radius: 5, color: COLORS.tapLive, fillColor: COLORS.tapLive, fillOpacity: 0.70, weight: 1.5 };
    var TAP_DEAD = { radius: 4, color: COLORS.tapDead, fillColor: COLORS.tapDead, fillOpacity: 0.60, weight: 1.5 };

    /* ── helpers ─────────────────────────── */
    function droneColor(d) {
        // All drones are RED by default
        // Only show THREAT color if: moving fast (>15m/s) or in zone or approaching zone
        if (d.speed > 15) return COLORS.threat;
        return COLORS.drone;
    }
    function esc(s) { if (s == null) return ''; return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
    function pr(l, v) { return '<div class="pop-r"><span class="pop-l">' + l + '</span><span class="pop-v">' + v + '</span></div>'; }
    function tapAge(ts) { if (!ts) return 9999; try { return Math.floor((Date.now() - new Date(ts).getTime()) / 1000); } catch (e) { return 9999; } }

    /* ── style selection (ALL drones RED, reduced opacity if lost) ─────────────────── */
    function droneStyle(d, selected) {
        var color = droneColor(d);  // Always RED (or THREAT orange if fast)
        var lost = d._contactStatus === 'lost';

        if (lost) {
            // Lost drones: still RED but with reduced opacity and dashed border
            return {
                radius: selected ? 10 : 7,
                color: color,              // RED not gray
                fillColor: color,          // RED not gray
                fillOpacity: selected ? 0.50 : 0.35,
                weight: selected ? 3 : 2,
                opacity: selected ? 0.70 : 0.55,
                dashArray: '4 3'
            };
        }

        return {
            radius: selected ? 12 : 9,
            color: selected ? COLORS.selected : color,
            fillColor: color,
            fillOpacity: selected ? 0.95 : 0.85,
            weight: selected ? 3 : 2,
            opacity: 1,
            dashArray: null
        };
    }
    function styleHash(d, selected) {
        var speedBand = d.speed > 15 ? 'F' : 'N';  // Fast or Normal
        return (d._contactStatus === 'lost' ? 'X' : speedBand) + (selected ? '1' : '0');
    }

    /* ══════════════════════════════════════
       SMOOTH-MOVE ANIMATION  (batched rAF with easing)
       ══════════════════════════════════════ */
    /* Smooth easing function (ease-out cubic) */
    function _easeOutCubic(t) {
        return 1 - Math.pow(1 - t, 3);
    }

    function _animLoop(now) {
        var keys = Object.keys(_anims);
        if (!keys.length) { _animId = null; return; }
        for (var i = 0; i < keys.length; i++) {
            var id = keys[i], a = _anims[id];
            var linear = Math.min((now - a.t0) / a.dur, 1);
            var t = _easeOutCubic(linear);  /* Apply easing */
            var lat = a.lat0 + (a.lat1 - a.lat0) * t;
            var lng = a.lng0 + (a.lng1 - a.lng0) * t;
            a.m.setLatLng([lat, lng]);
            /* also slide associated geometry that must track the drone */
            if (connLines[id]) {
                var cl = connLines[id].getLatLngs();
                connLines[id].setLatLngs([[lat, lng], cl[1]]);
            }
            if (threatRings[id]) threatRings[id].setLatLng([lat, lng]);
            if (linear >= 1) delete _anims[id];
        }
        _animId = requestAnimationFrame(_animLoop);
    }

    function smoothMove(marker, id, pos, durSec) {
        /* Cancel any in-flight animation for this id */
        if (_anims[id]) delete _anims[id];

        if (_zoomAnimating || (map && map._animatingZoom)) {
            marker.setLatLng(pos);
            return;
        }
        var cur = marker.getLatLng();
        /* Skip if negligible delta */
        if (Math.abs(cur.lat - pos[0]) < 1e-7 && Math.abs(cur.lng - pos[1]) < 1e-7) return;

        _anims[id] = {
            m: marker,
            lat0: cur.lat, lng0: cur.lng,
            lat1: pos[0],  lng1: pos[1],
            t0: performance.now(),
            dur: (durSec || 1) * 1000
        };
        if (!_animId) _animId = requestAnimationFrame(_animLoop);
    }

    /* ── popup builder ───────────────────── */
    function dronePopup(d) {
        var name = d.designation && d.designation !== 'UNKNOWN' ? d.designation : (d.identifier || '?');
        var t = d.trust_score != null ? d.trust_score : 100;
        var flags = d.spoof_flags || [];
        var fpConf = d.fingerprint_confidence || 0;
        var src = (d.detection_source || '').replace('WiFi', 'Wi-Fi');
        var popTag = d.tag || '';
        var id = d.identifier || '';
        var isActive = d._contactStatus === 'active';
        var model = (d.model || '').replace(' (Unknown)', '') || null;
        var serial = d.serial_number || null;

        /* Status indicator */
        var statusColor = isActive ? '#00E676' : '#78909C';
        var statusText = isActive ? 'ACTIVE' : 'LAST SEEN';
        var statusBg = isActive ? 'rgba(0,230,118,0.15)' : 'rgba(120,144,156,0.15)';

        var h = '<div class="pop-t">' + esc(name) + '</div>';

        /* Status badge */
        h += '<div style="display:flex;align-items:center;gap:6px;margin:4px 0 8px 0;padding:4px 8px;background:' + statusBg + ';border-radius:4px;border-left:3px solid ' + statusColor + '">';
        h += '<span style="width:8px;height:8px;border-radius:50%;background:' + statusColor + ';' + (isActive ? 'animation:pulse 1.5s infinite' : '') + '"></span>';
        h += '<span style="color:' + statusColor + ';font-weight:700;font-size:11px">' + statusText + '</span>';
        if (!isActive && d._lastSeenAgoS != null) {
            h += '<span style="color:#78909C;font-size:10px;margin-left:auto">' + formatDurationShort(d._lastSeenAgoS) + ' ago</span>';
        }
        h += '</div>';

        /* Model & Serial (if available) */
        if (model) {
            h += pr('Model', '<span style="color:#2196F3;font-weight:600">' + esc(model) + '</span>');
        }
        if (serial) {
            h += pr('Serial', '<span style="font-family:monospace;font-size:10px;opacity:0.85">' + esc(serial) + '</span>');
        }

        /* Tag */
        if (popTag) {
            var ptColors = { friendly: '#00E676', suspicious: '#FF6D00', hostile: '#FF1744', monitored: '#AA00FF', vip: '#FF4081', ignored: '#78909C' };
            var ptc = ptColors[popTag] || '#78909C';
            h += pr('Tag', '<span style="color:' + ptc + ';font-weight:700">' + esc(popTag.toUpperCase()) + '</span>');
        }

        /* Divider */
        h += '<div style="border-top:1px solid rgba(255,255,255,0.1);margin:6px 0"></div>';

        /* Flight data */
        h += pr('Altitude', d.altitude_geodetic != null ? d.altitude_geodetic.toFixed(0) + ' m' : '--');
        h += pr('Speed', d.speed != null ? d.speed.toFixed(1) + ' m/s <span style="opacity:0.6">(' + (d.speed * 3.6).toFixed(0) + ' km/h)</span>' : '--');
        h += pr('Heading', d.ground_track != null ? d.ground_track.toFixed(0) + '\u00b0' : '--');
        h += pr('MGRS', MGRS.forward(d.latitude, d.longitude, 5));

        /* Divider */
        h += '<div style="border-top:1px solid rgba(255,255,255,0.1);margin:6px 0"></div>';

        /* Signal & Trust */
        h += pr('RSSI', d.rssi != null ? '<span style="font-weight:600">' + d.rssi + ' dBm</span>' : '--');
        h += pr('Trust', '<span style="color:' + COLORS.drone + ';font-weight:600">' + t + '%</span>');
        h += pr('Source', esc(src) || '--');

        if (fpConf > 0) h += pr('Fingerprint', fpConf + '%');
        if (flags.length) h += pr('Flags', '<span style="color:#FF1744">' + esc(flags.join(', ')) + '</span>');

        /* RSSI-estimated distance (for fingerprint drones) */
        if (d.estimated_distance_m != null && d.estimated_distance_m > 0) {
            var distConf = d.distance_confidence != null ? Math.round(d.distance_confidence * 100) : '?';
            var distModel = d.distance_model_used || 'generic';
            var distStr = formatDistance(d.estimated_distance_m);
            h += pr('Est. Distance', '<span style="color:#AA00FF;font-weight:600">~' + distStr + '</span> <span style="opacity:0.6;font-size:10px">(' + distConf + '% conf)</span>');
        }

        /* Operator-to-UAV distance */
        if (d.operator_latitude != null && d.operator_longitude != null &&
            d.latitude != null && d.longitude != null &&
            !(d.operator_latitude === 0 && d.operator_longitude === 0)) {
            var dist = haversineDistance(d.latitude, d.longitude, d.operator_latitude, d.operator_longitude);
            h += pr('Op Distance', '<span style="color:#FFB300;font-weight:600">' + formatDistance(dist) + '</span>');
        }

        /* Session info */
        if (d._sessionDurationS != null && d._sessionDurationS > 0) {
            h += '<div style="border-top:1px solid rgba(255,255,255,0.1);margin:6px 0"></div>';
            h += pr('Session', formatDurationShort(d._sessionDurationS));
        }

        /* Flight path toggle button */
        var btnLabel = historyVisible[id] ? 'Hide Path' : 'Show Path';
        var btnStyle = 'background:' + (historyVisible[id] ? '#AA00FF' : '#37474F') +
            ';color:#fff;border:none;padding:4px 10px;border-radius:3px;cursor:pointer;margin-top:6px;font-size:11px;';
        h += '<div style="text-align:center;margin-top:8px;">' +
            '<button style="' + btnStyle + '" onclick="NzMap.showFlightPath(\'' + esc(id) + '\')">' + btnLabel + '</button>' +
            '</div>';
        return h;
    }

    /* Format duration for display (short form) */
    function formatDurationShort(seconds) {
        if (seconds < 60) return Math.round(seconds) + 's';
        if (seconds < 3600) return Math.floor(seconds / 60) + 'm';
        var h = Math.floor(seconds / 3600);
        var m = Math.floor((seconds % 3600) / 60);
        return h + 'h ' + m + 'm';
    }

    /* ── distance helpers ───────────────── */
    function haversineDistance(lat1, lng1, lat2, lng2) {
        var R = 6371000; // Earth radius in meters
        var dLat = (lat2 - lat1) * Math.PI / 180;
        var dLng = (lng2 - lng1) * Math.PI / 180;
        var a = Math.sin(dLat / 2) * Math.sin(dLat / 2) +
            Math.cos(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) *
            Math.sin(dLng / 2) * Math.sin(dLng / 2);
        return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
    }

    function formatDistance(meters) {
        if (meters < 1000) return meters.toFixed(0) + ' m';
        return (meters / 1000).toFixed(2) + ' km';
    }

    /* ── ring label builder (high contrast) ── */
    function buildRingLabelHtml(name, dist, color) {
        return '<div class="ring-label" style="border-left:4px solid ' + color + '">' +
            '<div style="font-weight:700;font-size:11px;line-height:1.2">' + esc(name) + '</div>' +
            '<div style="font-size:10px;opacity:0.85;margin-top:1px">' + dist + '</div></div>';
    }

    /* ══════════════════════════════════════
       INIT
       ══════════════════════════════════════ */
    function init(selectCb) {
        onSelect = selectCb;
        var defLat = parseFloat(localStorage.getItem("skylens_map_center_lat")) || 18.44;
        var defLng = parseFloat(localStorage.getItem("skylens_map_center_lng")) || -66.02;
        var defZoom = parseInt(localStorage.getItem("skylens_map_zoom")) || 13;
        map = L.map('map', { center: [defLat, defLng], zoom: defZoom, zoomControl: true, attributionControl: true });
        setTileStyle(settings.tileStyle);

        /* Zoom-state tracking (circleMarkers don't jitter but we still
           gate smooth-move and fitBounds to avoid competing setView). */
        map.on('zoomstart', function () { _zoomAnimating = true; });
        map.on('zoomanim',  function () { _zoomAnimating = true; });
        map.on('zoomend',   function () {
            _zoomAnimating = false;
        });

        /* Close popups when clicking on empty map area */
        map.on('click', function () {
            map.closePopup();
        });

        setTimeout(function () {
            if (!_zoomAnimating) map.invalidateSize();
        }, 200);

        loadZones();
        loadCustomRanges();
        return map;
    }

    function setTileStyle(style) {
        var t = TILES[style] || TILES.satellite;
        if (baseTile) map.removeLayer(baseTile);
        if (labelTile) map.removeLayer(labelTile);
        baseTile = L.tileLayer(t.url, { attribution: t.attr, maxZoom: t.maxZoom, subdomains: 'abcd' }).addTo(map);
        if (style === 'satellite') {
            labelTile = L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_only_labels/{z}/{x}/{y}{r}.png', {
                subdomains: 'abcd', maxZoom: 19, pane: 'overlayPane'
            }).addTo(map);
        }
        settings.tileStyle = style;
    }

    /* ══════════════════════════════════════
       UPDATE DRONES  (core tracking loop)
       ══════════════════════════════════════ */
    function updateDrones(drones, selectedId) {
        var zooming = _zoomAnimating || (map && map._animatingZoom);
        var seen = {};
        var hadNewMarker = false;

        drones.forEach(function (d) {
            var id = d.identifier || d.mac;
            if (!id) return;

            /* Determine if this is a WiFi-only detection (no GPS but has tap_id and RSSI) */
            var hasGPS = d.latitude && d.longitude && d.latitude !== 0 && d.longitude !== 0;
            var tapId = d.tap_uuid || d.tap_id;
            var hasRssi = d.rssi && d.rssi < 0;
            var isWifiOnly = !hasGPS && tapId && hasRssi;

            /* Skip drones with no GPS unless they're WiFi-only (which use range rings) */
            if (!hasGPS && !isWifiOnly) return;

            seen[id] = true;

            var pos   = hasGPS ? [d.latitude, d.longitude] : null;
            var sel   = selectedId === id;
            var trust = d.trust_score != null ? d.trust_score : 100;
            var isLost = d._contactStatus === 'lost';
            var isController = d.is_controller === true;
            if (isController) _controllerIds[id] = true;
            else delete _controllerIds[id];
            var visible = (!isLost || _showLost) && (!isController || _showControllers);

            /* ══════════════════════════════════════════════════════════════
               WiFi-only drones: skip marker/trail, just process range rings
               ══════════════════════════════════════════════════════════════ */
            if (isWifiOnly) {
                /* Remove stale GPS marker/trail from before the drone went dark */
                if (droneMarkers[id] && map.hasLayer(droneMarkers[id])) {
                    map.removeLayer(droneMarkers[id]);
                }
                if (trailLines[id]) { map.removeLayer(trailLines[id]); delete trailLines[id]; }
                if (operatorMarkers[id]) { map.removeLayer(operatorMarkers[id]); delete operatorMarkers[id]; }
                if (connLines[id]) { map.removeLayer(connLines[id]); delete connLines[id]; }
                if (threatRings[id]) { map.removeLayer(threatRings[id]); delete threatRings[id]; }
                if (headingLines[id]) { map.removeLayer(headingLines[id]); delete headingLines[id]; }
                if (speedVectors[id]) { map.removeLayer(speedVectors[id]); delete speedVectors[id]; }
                delete trails[id];
            } else {
                /* ── trail data (always maintain even when hidden) ── */
                if (!trails[id]) trails[id] = [];
                var tr = trails[id];
                var last = tr.length > 0 ? tr[tr.length - 1] : null;
                if (!last || last[0] !== pos[0] || last[1] !== pos[1]) {
                    tr.push(pos);
                    if (tr.length > settings.maxTrail) tr.shift();
                }

                /* ── drone circleMarker ─────────────── */
                /* For fingerprint drones with distance rings, hide the marker to reduce clutter */
                var isFingerprint = d.detection_source === 'WiFiFingerprint';
                var hasDistRing = d.estimated_distance_m != null && d.estimated_distance_m > 0;
                var hideMarker = isFingerprint && hasDistRing && !sel;  /* Show marker only when selected */

                var sh = styleHash(d, sel);
                if (droneMarkers[id]) {
                    /* update style only when visual state changed */
                    if (_styleCache[id] !== sh) {
                        droneMarkers[id].setStyle(droneStyle(d, sel));
                        _styleCache[id] = sh;
                    }
                    droneMarkers[id]._isLost = isLost;
                    droneMarkers[id]._isController = isController;
                    droneMarkers[id]._hasDistRing = hasDistRing;
                    if (visible && !hideMarker) {
                        if (!map.hasLayer(droneMarkers[id])) droneMarkers[id].addTo(map);
                        if (!zooming) smoothMove(droneMarkers[id], id, pos, 0.3);
                    } else {
                        if (map.hasLayer(droneMarkers[id])) map.removeLayer(droneMarkers[id]);
                    }
                    /* refresh popup only when open (throttled to 2s) */
                    if (droneMarkers[id].isPopupOpen()) {
                        var now = Date.now();
                        if (!_popupLastUpdate[id] || now - _popupLastUpdate[id] > 2000) {
                            droneMarkers[id].setPopupContent(dronePopup(d));
                            _popupLastUpdate[id] = now;
                        }
                    }
                } else {
                    var m = L.circleMarker(pos, droneStyle(d, sel))
                        .bindPopup(dronePopup(d), { closeButton: true, autoClose: true, closeOnClick: true, offset: [0, -8] })
                        .on('click', (function (did) {
                            return function () { if (onSelect) onSelect(did); };
                        })(id));
                    droneMarkers[id] = m;
                    m._isLost = isLost;
                    m._isController = isController;
                    m._hasDistRing = hasDistRing;
                    _styleCache[id] = sh;
                    if (visible && !hideMarker) m.addTo(map);
                    hadNewMarker = true;
                }
            }

            /* ── skip spatial geometry while zooming or hidden ── */
            if (zooming || !visible) return;

            /* ══════════════════════════════════════════════════════════════
               GPS-based geometry (trails, operators, threats, vectors)
               Skip all of these for WiFi-only drones with no GPS position
               ══════════════════════════════════════════════════════════════ */
            /* Check if position actually changed (skip geometry if same) */
            var posChanged = true;
            if (pos && _lastDronePos[id]) {
                var lastPos = _lastDronePos[id];
                if (Math.abs(lastPos[0] - pos[0]) < 1e-7 && Math.abs(lastPos[1] - pos[1]) < 1e-7) {
                    posChanged = false;
                }
            }
            if (pos) _lastDronePos[id] = [pos[0], pos[1]];

            if (!isWifiOnly && pos) {
                /* ── trail polyline (trust-colored, gradient opacity) ──────────────────── */
                if (layers.trails && trails[id] && trails[id].length > 1) {
                    var tr = trails[id];
                    var trailColor = COLORS.drone + '60';  // 38% opacity for visibility
                    if (trailLines[id]) {
                        /* Incremental update: addLatLng for new point, splice from front if over max */
                        if (posChanged && trailLines[id]._nzLen !== tr.length) {
                            var oldLen = trailLines[id]._nzLen || 0;
                            if (tr.length === oldLen + 1) {
                                // Common case: one new point appended
                                trailLines[id].addLatLng(tr[tr.length - 1]);
                                // Trim from front if over maxTrail
                                if (tr.length > settings.maxTrail) {
                                    var lls = trailLines[id].getLatLngs();
                                    if (lls.length > settings.maxTrail) {
                                        lls.splice(0, lls.length - settings.maxTrail);
                                        trailLines[id].setLatLngs(lls);
                                    }
                                }
                            } else {
                                // Bulk change (e.g. trail reset) — full setLatLngs
                                trailLines[id].setLatLngs(tr);
                            }
                            trailLines[id]._nzLen = tr.length;
                        }
                        // Update color if trust changed
                        if (trailLines[id]._nzTrust !== trust) {
                            trailLines[id].setStyle({ color: trailColor });
                            trailLines[id]._nzTrust = trust;
                        }
                    } else {
                        trailLines[id] = L.polyline(tr, { color: trailColor, weight: 2.5, dashArray: '3 5' }).addTo(map);
                        trailLines[id]._nzLen = tr.length;
                        trailLines[id]._nzTrust = trust;
                    }
                } else if (trailLines[id]) {
                    map.removeLayer(trailLines[id]); delete trailLines[id];
                }

                /* ── operator circleMarker ──────────── */
                if (layers.operators && d.operator_latitude != null && d.operator_longitude != null &&
                    !(d.operator_latitude === 0 && d.operator_longitude === 0)) {
                    var oPos = [d.operator_latitude, d.operator_longitude];
                    if (operatorMarkers[id]) {
                        /* Only update position if drone position changed */
                        if (posChanged) operatorMarkers[id].setLatLng(oPos);
                        operatorMarkers[id].setStyle(operatorStyle(sel));
                    } else {
                        operatorMarkers[id] = L.circleMarker(oPos, operatorStyle(sel))
                            .bindPopup('<div class="pop-t">Operator</div>' +
                                pr('Drone', esc(d.identifier || '?')) + pr('Op ID', esc(d.operator_id || '--')),
                                { closeButton: true, autoClose: true, closeOnClick: true }).addTo(map);
                    }
                    /* connection line (yellow to match operator marker) */
                    if (connLines[id]) {
                        if (posChanged) connLines[id].setLatLngs([pos, oPos]);
                    } else {
                        connLines[id] = L.polyline([pos, oPos], { color: 'rgba(255,193,7,0.45)', weight: 2, dashArray: '6 4' }).addTo(map);
                    }
                } else {
                    if (operatorMarkers[id]) { map.removeLayer(operatorMarkers[id]); delete operatorMarkers[id]; }
                    if (connLines[id]) { map.removeLayer(connLines[id]); delete connLines[id]; }
                }

                /* ── threat rings ────────────────────── */
                if (layers.threats && trust < 60) {
                    if (threatRings[id]) { threatRings[id].setLatLng(pos); }
                    else {
                        threatRings[id] = L.circle(pos, {
                            radius: settings.threatRadius, color: COLORS.drone, weight: 1,
                            fillColor: COLORS.drone, fillOpacity: 0.06, dashArray: '6 4'
                        }).addTo(map);
                    }
                } else if (threatRings[id]) {
                    map.removeLayer(threatRings[id]); delete threatRings[id];
                }

                /* ── heading tick (always visible, short indicator) ── */
                if (d.ground_track != null && d.speed != null && d.speed > 0.3) {
                    /* Only update if position actually changed */
                    if (posChanged || !headingLines[id]) {
                        var hLen = Math.min(Math.max(d.speed * 4, 30), 200);   // 30-200 m
                        var hRad = d.ground_track * Math.PI / 180;
                        var hEnd = [
                            pos[0] + (hLen / 111320) * Math.cos(hRad),
                            pos[1] + (hLen / (111320 * Math.cos(pos[0] * Math.PI / 180))) * Math.sin(hRad)
                        ];
                        if (headingLines[id]) { headingLines[id].setLatLngs([pos, hEnd]); }
                        else { headingLines[id] = L.polyline([pos, hEnd], { color: '#FF174480', weight: 2, interactive: false }).addTo(map); }
                    }
                } else if (headingLines[id]) {
                    map.removeLayer(headingLines[id]); delete headingLines[id];
                }

                /* ── speed vectors (toggled layer) ───── */
                if (layers.vectors && d.speed != null && d.speed > 0.5 && d.ground_track != null) {
                    /* Only update if position actually changed */
                    if (posChanged || !speedVectors[id]) {
                        var len = Math.min(d.speed * 20, 300);
                        var rad = d.ground_track * Math.PI / 180;
                        var endLat = pos[0] + (len / 111320) * Math.cos(rad);
                        var endLng = pos[1] + (len / (111320 * Math.cos(pos[0] * Math.PI / 180))) * Math.sin(rad);
                        if (speedVectors[id]) { speedVectors[id].setLatLngs([pos, [endLat, endLng]]); }
                        else { speedVectors[id] = L.polyline([pos, [endLat, endLng]], { color: '#FFB30080', weight: 2 }).addTo(map); }
                    }
                } else if (speedVectors[id]) {
                    map.removeLayer(speedVectors[id]); delete speedVectors[id];
                }
            }

            /* ══════════════════════════════════════════════════════════════
               RSSI-Based Position Display (dark drones without GPS)
               Clean design: ONE ring + estimated position marker.
               - If trilateration available (2+ TAPs): show est. position + error circle
               - Otherwise: show single best-confidence range ring from closest TAP
               ══════════════════════════════════════════════════════════════ */
            var rssiVal = d.rssi || 0;
            var hasApiRings = d.range_rings && d.range_rings.length > 0;
            var tapIdForRing = d.tap_uuid || d.tap_id;
            var ep = d.estimated_position;
            var hasEstPos = ep && ep.latitude && ep.longitude && ep.taps_used >= 1;

            /* Ring key for this drone */
            var ringKey = id + '::ring';
            var estKey = id + '::est';
            var errKey = id + '::err';
            var lblKey = id + '::lbl';
            var activeRingKeys = {};

            /* Hide range rings for lost drones unless selected */
            var showRings = !isLost || sel;

            if (!hasGPS && showRings && (hasApiRings || (hasRssi && tapIdForRing))) {
                var labelName = d.model || (d.designation && d.designation !== 'UNKNOWN'
                    ? d.designation.split(' ')[0] : id.substring(0, 12));

                /* ── Estimated position from trilateration (primary display) ── */
                if (hasEstPos) {
                    var epPos = [ep.latitude, ep.longitude];
                    var errRadius = Math.max(50, Math.min(ep.error_m || 200, 5000));
                    activeRingKeys[estKey] = true;
                    activeRingKeys[errKey] = true;
                    activeRingKeys[lblKey] = true;

                    /* Color by confidence - dark orange tones (distinct from operator yellow) */
                    var epColor = ep.confidence >= 0.7 ? '#FF1744' :
                                  ep.confidence >= 0.4 ? '#FF6D00' : '#E65100';

                    if (distanceRings[estKey]) {
                        /* Update existing - only if position/size actually changed */
                        var ec = distanceRings[estKey]._nzCache || {};
                        var posChanged = Math.abs((ec.lat||0) - epPos[0]) > 1e-6 || Math.abs((ec.lon||0) - epPos[1]) > 1e-6;
                        if (posChanged) {
                            distanceRings[estKey].setLatLng(epPos);
                            distanceRings[estKey]._nzCache = { lat: epPos[0], lon: epPos[1], color: ec.color };
                        }
                        /* Update color if confidence band changed */
                        if (ec.color !== epColor) {
                            distanceRings[estKey].setStyle({ fillColor: epColor });
                            distanceRings[estKey]._nzCache.color = epColor;
                        }
                        if (distanceRings[errKey] && posChanged) {
                            distanceRings[errKey].setLatLng(epPos);
                            var radChanged = Math.abs((ec.radius||0) - errRadius) > 5;
                            if (radChanged) {
                                distanceRings[errKey].setRadius(errRadius);
                                distanceRings[estKey]._nzCache.radius = errRadius;
                            }
                        }
                    } else {
                        /* Estimated position marker - pulsing dot */
                        var estM = L.circleMarker(epPos, {
                            radius: 8, color: '#FFFFFF', fillColor: epColor,
                            fillOpacity: 0.95, weight: 2.5, className: 'est-pos-marker'
                        });
                        estM.on('click', (function(did) {
                            return function() { if (onSelect) onSelect(did); };
                        })(id));
                        estM.bindPopup(dronePopup(d), { closeButton: true });
                        estM.addTo(map);
                        distanceRings[estKey] = estM;

                        /* Error circle - soft uncertainty area */
                        var errC = L.circle(epPos, {
                            radius: errRadius, color: epColor, weight: 1.5,
                            opacity: 0.6, fill: true, fillColor: epColor,
                            fillOpacity: 0.07, dashArray: '6 4',
                            className: 'rssi-ring'
                        });
                        errC.addTo(map);
                        distanceRings[errKey] = errC;

                        /* Label */
                        var metersPerDegLat = 111320;
                        var lblLat = epPos[0] + (errRadius / metersPerDegLat);
                        var distStr = errRadius < 1000 ? '\u00b1' + Math.round(errRadius) + 'm'
                            : '\u00b1' + (errRadius / 1000).toFixed(1) + 'km';
                        var lbl = L.marker([lblLat, epPos[1]], {
                            icon: L.divIcon({
                                className: 'ring-label-container',
                                html: buildRingLabelHtml(labelName, distStr, epColor),
                                iconSize: [120, 36], iconAnchor: [60, 36]
                            }),
                            interactive: false, zIndexOffset: 1000
                        }).addTo(map);
                        distanceRings[lblKey] = lbl;
                    }

                    /* ── Per-TAP range rings (show detection geometry alongside est. position) ── */
                    if (hasApiRings) {
                        var tapColors = ['#00E676', '#00B0FF', '#AA00FF', '#FF4081', '#FFD740'];
                        for (var ti = 0; ti < d.range_rings.length; ti++) {
                            var tr = d.range_rings[ti];
                            if (!tr.tap_lat || !tr.tap_lon || !tr.distance_m || tr.distance_m <= 0) continue;
                            var tapRingKey = id + '::tr:' + tr.tap_id;
                            var tapLblKey = id + '::tl:' + tr.tap_id;
                            activeRingKeys[tapRingKey] = true;
                            activeRingKeys[tapLblKey] = true;
                            var tColor = tapColors[ti % tapColors.length];
                            var tRadius = Math.max(30, Math.min(tr.distance_m, 20000));
                            var tCenter = [tr.tap_lat, tr.tap_lon];

                            if (distanceRings[tapRingKey]) {
                                var tc = distanceRings[tapRingKey]._nzCache || {};
                                if (Math.abs((tc.radius||0) - tRadius) > 5) {
                                    distanceRings[tapRingKey].setRadius(tRadius);
                                    distanceRings[tapRingKey]._nzCache.radius = tRadius;
                                    /* Update label position and text */
                                    if (distanceRings[tapLblKey]) {
                                        var updDistStr = tRadius < 1000 ? '~' + Math.round(tRadius) + 'm'
                                            : '~' + (tRadius / 1000).toFixed(1) + 'km';
                                        var updLblLat = tCenter[0] + (tRadius / 111320);
                                        distanceRings[tapLblKey].setLatLng([updLblLat, tCenter[1]]);
                                        if (distanceRings[tapLblKey]._icon) {
                                            distanceRings[tapLblKey]._icon.innerHTML =
                                                '<div style="font:bold 10px monospace;color:' + tColor +
                                                ';text-shadow:0 0 3px #000;white-space:nowrap">' +
                                                tr.tap_id + ' ' + updDistStr + '</div>';
                                        }
                                    }
                                }
                            } else {
                                var tapRing = L.circle(tCenter, {
                                    radius: tRadius, color: tColor, weight: 1.5,
                                    opacity: 0.5, fill: false, dashArray: '4 6',
                                    className: 'rssi-ring'
                                });
                                tapRing.addTo(map);
                                tapRing._nzCache = { radius: tRadius };
                                distanceRings[tapRingKey] = tapRing;

                                /* Tap ring label */
                                var tDistStr = tRadius < 1000 ? '~' + Math.round(tRadius) + 'm'
                                    : '~' + (tRadius / 1000).toFixed(1) + 'km';
                                var tLblLat = tCenter[0] + (tRadius / 111320);
                                var tapLbl = L.marker([tLblLat, tCenter[1]], {
                                    icon: L.divIcon({
                                        className: 'ring-label-container',
                                        html: '<div style="font:bold 10px monospace;color:' + tColor +
                                              ';text-shadow:0 0 3px #000;white-space:nowrap">' +
                                              tr.tap_id + ' ' + tDistStr + '</div>',
                                        iconSize: [120, 20], iconAnchor: [60, 10]
                                    }),
                                    interactive: false, zIndexOffset: 900
                                }).addTo(map);
                                distanceRings[tapLblKey] = tapLbl;
                            }
                        }
                    }
                } else {
                    /* ── Single best range ring (no trilateration) ── */
                    /* Hysteresis: prefer the currently-shown TAP unless another is
                       significantly better (confidence >=0.1 higher or 30%+ closer).
                       This prevents spazzy ring-switching from RSSI jitter. */
                    var bestRing = null;
                    var currentTap = _bestRingTap[id] || null;
                    var currentRing = null;
                    if (hasApiRings) {
                        for (var ri = 0; ri < d.range_rings.length; ri++) {
                            var ar = d.range_rings[ri];
                            if (ar.tap_lat && ar.tap_lon && ar.distance_m > 0) {
                                if (currentTap && ar.tap_id === currentTap) currentRing = ar;
                                if (!bestRing || ar.confidence > bestRing.confidence ||
                                    (ar.confidence === bestRing.confidence && ar.distance_m < bestRing.distance_m)) {
                                    bestRing = ar;
                                }
                            }
                        }
                        /* Stick with current TAP unless new best is significantly better */
                        if (currentRing && bestRing && bestRing.tap_id !== currentTap) {
                            var confGain = bestRing.confidence - currentRing.confidence;
                            var distRatio = currentRing.distance_m > 0 ? bestRing.distance_m / currentRing.distance_m : 1;
                            if (confGain < 0.15 && distRatio > 0.6) {
                                bestRing = currentRing;  // Keep current - need significant improvement to switch
                            }
                        }
                        if (bestRing) _bestRingTap[id] = bestRing.tap_id;
                    }

                    var ringCenter, ringRadius, ringRssi;
                    if (bestRing) {
                        ringCenter = [bestRing.tap_lat, bestRing.tap_lon];
                        ringRadius = Math.max(50, Math.min(bestRing.distance_m, 20000));
                        ringRssi = bestRing.rssi || rssiVal;
                    } else if (hasRssi && tapIdForRing) {
                        ringCenter = getTapPositionForUav(tapIdForRing);
                        var txPower = 18.4, pathLossExp = 2.6;
                        ringRadius = Math.pow(10, (txPower - rssiVal) / (10 * pathLossExp));
                        ringRadius = Math.max(50, Math.min(ringRadius, 50000));
                        ringRssi = rssiVal;
                    }

                    if (ringCenter) {
                        activeRingKeys[ringKey] = true;
                        activeRingKeys[lblKey] = true;

                        /* Color by RSSI proximity */
                        var ringColor, ringWeight;
                        if (ringRssi >= -60) {
                            ringColor = '#D50000'; ringWeight = 3;
                        } else if (ringRssi >= -70) {
                            ringColor = '#E65100'; ringWeight = 2.5;
                        } else if (ringRssi >= -80) {
                            ringColor = '#FF6F00'; ringWeight = 2;
                        } else {
                            ringColor = '#00BFA5'; ringWeight = 2;
                        }

                        var distStr = ringRadius < 1000
                            ? '~' + Math.round(ringRadius) + 'm'
                            : '~' + (ringRadius / 1000).toFixed(1) + 'km';
                        var metersPerDegLat = 111320;
                        var lblLat = ringCenter[0] + (ringRadius / metersPerDegLat);

                        if (distanceRings[ringKey]) {
                            var cached = distanceRings[ringKey]._nzCache || {};
                            if (Math.abs((cached.rssi || -999) - ringRssi) > 2 || cached.radius !== ringRadius) {
                                distanceRings[ringKey].setLatLng(ringCenter);
                                distanceRings[ringKey].setRadius(ringRadius);
                                distanceRings[ringKey].setStyle({ color: ringColor, weight: ringWeight, opacity: 0.8 });
                                distanceRings[ringKey]._nzCache = { rssi: ringRssi, radius: ringRadius };
                                if (distanceRings[lblKey]) {
                                    distanceRings[lblKey].setLatLng([lblLat, ringCenter[1]]);
                                    if (distanceRings[lblKey]._icon) {
                                        var newHtml = buildRingLabelHtml(labelName, distStr, ringColor);
                                        if (distanceRings[lblKey]._nzHtml !== newHtml) {
                                            distanceRings[lblKey]._icon.innerHTML = newHtml;
                                            distanceRings[lblKey]._nzHtml = newHtml;
                                        }
                                    }
                                }
                            }
                        } else {
                            var ring = L.circle(ringCenter, {
                                radius: ringRadius, color: ringColor, weight: ringWeight,
                                opacity: 0.8, fill: true, fillColor: ringColor, fillOpacity: 0.05,
                                dashArray: '10 6', className: 'rssi-ring'
                            });
                            ring.on('click', (function(did) {
                                return function() { if (onSelect) onSelect(did); };
                            })(id));
                            ring.bindPopup(dronePopup(d), { closeButton: true });
                            ring.addTo(map);
                            ring._nzCache = { rssi: ringRssi, radius: ringRadius };
                            distanceRings[ringKey] = ring;

                            var lbl = L.marker([lblLat, ringCenter[1]], {
                                icon: L.divIcon({
                                    className: 'ring-label-container',
                                    html: buildRingLabelHtml(labelName, distStr, ringColor),
                                    iconSize: [120, 36], iconAnchor: [60, 36]
                                }),
                                interactive: false, zIndexOffset: 1000
                            }).addTo(map);
                            distanceRings[lblKey] = lbl;
                        }
                    }
                }
            }

            /* Clean up stale ring layers for this drone */
            var prefix = id + '::';
            var rKeys = Object.keys(distanceRings);
            for (var rki = 0; rki < rKeys.length; rki++) {
                var rk = rKeys[rki];
                if (rk.indexOf(prefix) === 0 && !activeRingKeys[rk]) {
                    map.removeLayer(distanceRings[rk]); delete distanceRings[rk];
                    if (ringLabels[rk]) { map.removeLayer(ringLabels[rk]); delete ringLabels[rk]; }
                }
            }
            /* Clean legacy single-key rings */
            if (distanceRings[id]) {
                map.removeLayer(distanceRings[id]); delete distanceRings[id];
                if (ringLabels[id]) { map.removeLayer(ringLabels[id]); delete ringLabels[id]; }
            }
        });

        /* ── remove stale (no longer in data at all) ── */
        var markerIds = Object.keys(droneMarkers);
        for (var mi = 0; mi < markerIds.length; mi++) {
            var mid = markerIds[mi];
            if (!seen[mid]) {
                removeDrone(mid);
            }
        }

        /* ── fit / follow ────────────────────── */
        if (!zooming) {
            if (hadNewMarker || (!hasFitted && Object.keys(droneMarkers).length > 0)) {
                fitAll();
                hasFitted = true;
            }
            if (_autoFollow && selectedId && droneMarkers[selectedId]) {
                var now = Date.now();
                if (now - _lastFollowPan > 400) {  // Max 2.5 pans/sec
                    var targetLL = droneMarkers[selectedId].getLatLng();
                    var center = map.getCenter();
                    /* Only pan if drone moved >15% of visible area to prevent micro-pans */
                    var bounds = map.getBounds();
                    var viewH = Math.abs(bounds.getNorth() - bounds.getSouth());
                    var viewW = Math.abs(bounds.getEast() - bounds.getWest());
                    var dLat = Math.abs(targetLL.lat - center.lat);
                    var dLng = Math.abs(targetLL.lng - center.lng);
                    if (dLat > viewH * 0.15 || dLng > viewW * 0.15) {
                        map.panTo(targetLL, { animate: true, duration: 0.6, easeLinearity: 0.5 });
                        _lastFollowPan = now;
                    }
                }
            }
        }
    }

    /* ══════════════════════════════════════
       UPDATE TAPS
       ══════════════════════════════════════ */
    function updateTaps(taps) {
        var zooming = _zoomAnimating || (map && map._animatingZoom);
        var seen = {};

        taps.forEach(function (t) {
            var id = t.tap_uuid;
            if (!id || t.latitude == null || t.longitude == null) return;
            if (t.latitude === 0 && t.longitude === 0) return;
            seen[id] = true;
            var pos  = [t.latitude, t.longitude];
            var live = tapAge(t.timestamp) < 60;
            var sty  = live ? TAP_LIVE : TAP_DEAD;

            if (tapMarkers[id]) {
                if (!zooming) tapMarkers[id].setLatLng(pos);
                tapMarkers[id].setStyle(sty);
            } else {
                var chans = t.channels ? t.channels.join(',') : (t.channel || '?');
                tapMarkers[id] = L.circleMarker(pos, sty)
                    .bindPopup('<div class="pop-t">' + esc(t.tap_name || 'Tap') + '</div>' +
                        pr('MGRS', MGRS.forward(t.latitude, t.longitude, 4)) +
                        pr('Channels', '[' + esc(chans) + ']') +
                        pr('Frames', (t.frames_total || 0).toLocaleString()) +
                        pr('Parsed', (t.frames_parsed || 0).toLocaleString()) +
                        pr('Interface', esc(t.interface || '--')) +
                        pr('Capture', t.capture_running ? 'Running' : 'Down') +
                        pr('Version', 'v' + esc(t.version || '?')),
                        { closeButton: false }).addTo(map);
            }
        });

        Object.keys(tapMarkers).forEach(function (id) {
            if (!seen[id]) { map.removeLayer(tapMarkers[id]); delete tapMarkers[id]; }
        });

        /* range rings */
        if (!zooming) {
            taps.forEach(function (t) {
                var id = t.tap_uuid;
                if (!id || !seen[id]) return;
                if (layers.zones && t.latitude && t.longitude) {
                    var pos2 = [t.latitude, t.longitude];
                    if (_rangeRings[id]) {
                        _rangeRings[id].forEach(function (r) { r.setLatLng(pos2); });
                    } else {
                        _rangeRings[id] = [
                            L.circle(pos2, { radius: 500,  color: '#4CAF5030', weight: 1, fillOpacity: 0, dashArray: '4 6', interactive: false }).addTo(map),
                            L.circle(pos2, { radius: 1500, color: '#4CAF5020', weight: 1, fillOpacity: 0, dashArray: '4 6', interactive: false }).addTo(map),
                            L.circle(pos2, { radius: 3000, color: '#4CAF5015', weight: 1, fillOpacity: 0, dashArray: '4 6', interactive: false }).addTo(map)
                        ];
                    }
                }
            });
        }
        Object.keys(_rangeRings).forEach(function (id) {
            if (!seen[id]) {
                _rangeRings[id].forEach(function (r) { map.removeLayer(r); });
                delete _rangeRings[id];
            }
        });

        if (!zooming && !hasFitted && Object.keys(tapMarkers).length > 0) { fitAll(); hasFitted = true; }
    }

    /* ══════════════════════════════════════
       SHOW / HIDE LOST
       ══════════════════════════════════════ */
    function setShowLost(on) {
        _showLost = on;
        Object.keys(droneMarkers).forEach(function (id) {
            var m = droneMarkers[id];
            if (!m._isLost) return;                  // only touch lost markers
            if (on) {
                if (!map.hasLayer(m)) m.addTo(map);
                /* re-show associated layers respecting their toggles */
                if (operatorMarkers[id] && layers.operators && !map.hasLayer(operatorMarkers[id])) operatorMarkers[id].addTo(map);
                if (connLines[id]       && layers.operators && !map.hasLayer(connLines[id]))       connLines[id].addTo(map);
                if (trailLines[id]      && layers.trails    && !map.hasLayer(trailLines[id]))      trailLines[id].addTo(map);
                if (threatRings[id]     && layers.threats   && !map.hasLayer(threatRings[id]))      threatRings[id].addTo(map);
                if (speedVectors[id]    && layers.vectors   && !map.hasLayer(speedVectors[id]))     speedVectors[id].addTo(map);
                if (headingLines[id]    && !map.hasLayer(headingLines[id]))                         headingLines[id].addTo(map);
            } else {
                if (map.hasLayer(m)) map.removeLayer(m);
                if (operatorMarkers[id] && map.hasLayer(operatorMarkers[id])) map.removeLayer(operatorMarkers[id]);
                if (connLines[id]       && map.hasLayer(connLines[id]))       map.removeLayer(connLines[id]);
                if (trailLines[id]      && map.hasLayer(trailLines[id]))      map.removeLayer(trailLines[id]);
                if (threatRings[id]     && map.hasLayer(threatRings[id]))      map.removeLayer(threatRings[id]);
                if (speedVectors[id]    && map.hasLayer(speedVectors[id]))     map.removeLayer(speedVectors[id]);
                if (headingLines[id]    && map.hasLayer(headingLines[id]))     map.removeLayer(headingLines[id]);
            }
            /* Toggle distance rings (keyed as id or id::suffix) */
            var prefix = id + '::';
            Object.keys(distanceRings).forEach(function (key) {
                if (key === id || key.indexOf(prefix) === 0) {
                    if (on) {
                        if (!map.hasLayer(distanceRings[key])) distanceRings[key].addTo(map);
                    } else {
                        if (map.hasLayer(distanceRings[key])) map.removeLayer(distanceRings[key]);
                    }
                }
            });
        });
    }

    /* ══════════════════════════════════════
       SHOW / HIDE CONTROLLERS
       ══════════════════════════════════════ */
    function setShowControllers(on) {
        _showControllers = on;
        Object.keys(droneMarkers).forEach(function (id) {
            var m = droneMarkers[id];
            if (!m._isController) return;          // only touch controller markers
            if (on) {
                if (!map.hasLayer(m)) m.addTo(map);
                if (operatorMarkers[id] && layers.operators && !map.hasLayer(operatorMarkers[id])) operatorMarkers[id].addTo(map);
                if (connLines[id]       && layers.operators && !map.hasLayer(connLines[id]))       connLines[id].addTo(map);
                if (trailLines[id]      && layers.trails    && !map.hasLayer(trailLines[id]))      trailLines[id].addTo(map);
                if (threatRings[id]     && layers.threats   && !map.hasLayer(threatRings[id]))      threatRings[id].addTo(map);
                if (speedVectors[id]    && layers.vectors   && !map.hasLayer(speedVectors[id]))     speedVectors[id].addTo(map);
                if (headingLines[id]    && !map.hasLayer(headingLines[id]))                         headingLines[id].addTo(map);
            } else {
                if (map.hasLayer(m)) map.removeLayer(m);
                if (operatorMarkers[id] && map.hasLayer(operatorMarkers[id])) map.removeLayer(operatorMarkers[id]);
                if (connLines[id]       && map.hasLayer(connLines[id]))       map.removeLayer(connLines[id]);
                if (trailLines[id]      && map.hasLayer(trailLines[id]))      map.removeLayer(trailLines[id]);
                if (threatRings[id]     && map.hasLayer(threatRings[id]))      map.removeLayer(threatRings[id]);
                if (speedVectors[id]    && map.hasLayer(speedVectors[id]))     map.removeLayer(speedVectors[id]);
                if (headingLines[id]    && map.hasLayer(headingLines[id]))     map.removeLayer(headingLines[id]);
            }
        });
        /* Also toggle distance rings owned by controllers (including WiFi-only with no drone marker) */
        Object.keys(distanceRings).forEach(function (key) {
            var droneId = key.split('::')[0];
            if (!_controllerIds[droneId]) return;
            if (on) {
                if (!map.hasLayer(distanceRings[key])) distanceRings[key].addTo(map);
            } else {
                if (map.hasLayer(distanceRings[key])) map.removeLayer(distanceRings[key]);
            }
        });
    }

    /* ══════════════════════════════════════
       TOGGLE LAYERS
       ══════════════════════════════════════ */
    function toggleLayer(name) {
        layers[name] = !layers[name];
        if (name === 'trails') {
            Object.keys(trailLines).forEach(function (id) {
                if (!layers.trails) { map.removeLayer(trailLines[id]); delete trailLines[id]; }
            });
        }
        if (name === 'operators') {
            Object.keys(operatorMarkers).forEach(function (id) {
                if (!layers.operators) {
                    map.removeLayer(operatorMarkers[id]); delete operatorMarkers[id];
                    if (connLines[id]) { map.removeLayer(connLines[id]); delete connLines[id]; }
                }
            });
        }
        if (name === 'threats') {
            Object.keys(threatRings).forEach(function (id) {
                if (!layers.threats) { map.removeLayer(threatRings[id]); delete threatRings[id]; }
            });
        }
        if (name === 'vectors') {
            Object.keys(speedVectors).forEach(function (id) {
                if (!layers.vectors) { map.removeLayer(speedVectors[id]); delete speedVectors[id]; }
            });
        }
        if (name === 'zones') {
            Object.keys(zonePolygons).forEach(function (id) {
                if (layers.zones) zonePolygons[id].addTo(map);
                else map.removeLayer(zonePolygons[id]);
            });
            Object.keys(_rangeRings).forEach(function (id) {
                _rangeRings[id].forEach(function (r) {
                    if (layers.zones) r.addTo(map);
                    else map.removeLayer(r);
                });
            });
        }
        return layers[name];
    }

    /* ══════════════════════════════════════
       ZONES
       ══════════════════════════════════════ */
    var ZONE_COLORS = { restricted: '#FF1744', alert: '#FF6D00', warning: '#FFB300', safe: '#00E676' };

    function loadZones() {
        try {
            var data = JSON.parse(localStorage.getItem('nz_zones') || '[]');
            data.forEach(function (z) { addZoneToMap(z); });
        } catch (e) { /* ignore */ }
    }

    function addZoneToMap(z) {
        var color = ZONE_COLORS[z.type] || '#FFB300';
        var poly = L.polygon(z.points, {
            color: color, weight: 2, fillColor: color, fillOpacity: 0.08, dashArray: '6 4'
        });
        poly._zoneData = z;
        if (layers.zones) poly.addTo(map);
        zonePolygons[z.id] = poly;
        poly.bindPopup('<div class="pop-t">' + esc(z.name) + '</div>' + pr('Type', esc(z.type)));
    }

    function saveZones() {
        var data = [];
        Object.keys(zonePolygons).forEach(function (id) {
            var z = zonePolygons[id]._zoneData;
            if (z) data.push(z);
        });
        localStorage.setItem('nz_zones', JSON.stringify(data));
        if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferencesNow) SkylensAuth.savePreferencesNow();
    }

    function getZones() {
        var list = [];
        Object.keys(zonePolygons).forEach(function (id) {
            var z = zonePolygons[id]._zoneData;
            if (z) list.push(z);
        });
        return list;
    }

    function removeZone(id) {
        if (zonePolygons[id]) {
            map.removeLayer(zonePolygons[id]);
            delete zonePolygons[id];
            saveZones();
        }
    }

    function clearAllZones() {
        Object.keys(zonePolygons).forEach(function (id) {
            map.removeLayer(zonePolygons[id]);
        });
        zonePolygons = {};
        localStorage.removeItem('nz_zones');
        if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferencesNow) SkylensAuth.savePreferencesNow();
    }

    function startZoneDraw(name, type, onComplete) {
        var points = [];
        var tempLine = null;
        var tempMarkers = [];
        var lastClickTime = 0;

        function onClick(e) {
            var now = Date.now();
            if (now - lastClickTime < 400) return;
            lastClickTime = now;
            points.push([e.latlng.lat, e.latlng.lng]);
            var mk = L.circleMarker(e.latlng, { radius: 4, color: ZONE_COLORS[type] || '#FFB300', fillOpacity: 1 }).addTo(map);
            tempMarkers.push(mk);
            if (points.length > 1) {
                if (tempLine) map.removeLayer(tempLine);
                tempLine = L.polyline(points, { color: ZONE_COLORS[type] || '#FFB300', weight: 2, dashArray: '4 4' }).addTo(map);
            }
        }

        function onDblClick(e) {
            map.off('click', onClick);
            map.off('dblclick', onDblClick);
            tempMarkers.forEach(function (mk) { map.removeLayer(mk); });
            if (tempLine) map.removeLayer(tempLine);
            if (points.length >= 3) {
                var zone = { id: 'zone-' + Date.now(), name: name || 'Zone', type: type || 'warning', points: points };
                addZoneToMap(zone);
                saveZones();
                if (onComplete) onComplete(zone);
            }
        }

        map.on('click', onClick);
        map.on('dblclick', onDblClick);
    }

    /* ══════════════════════════════════════
       CUSTOM RANGE RINGS (user-defined, localStorage-backed)
       ══════════════════════════════════════ */
    var _customRings = {};       // id → L.circle
    var _customRingLabels = {};  // id → L.marker (label)
    var _customRangesVisible = true;
    var CUSTOM_RANGE_KEY = 'skylens_custom_ranges';

    function loadCustomRanges() {
        try {
            var data = JSON.parse(localStorage.getItem(CUSTOM_RANGE_KEY) || '[]');
            data.forEach(function (r) { if (r.enabled !== false) addCustomRange(r, true); });
        } catch (e) { /* ignore */ }
        // Restore visibility state
        try {
            var vis = localStorage.getItem('skylens_range_rings_visible');
            if (vis !== null) {
                _customRangesVisible = JSON.parse(vis);
                setCustomRangesVisible(_customRangesVisible);
            }
        } catch (e) { /* ignore */ }
    }

    function _saveCustomRanges() {
        var list = _getCustomRangeData();
        localStorage.setItem(CUSTOM_RANGE_KEY, JSON.stringify(list));
        if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferencesNow) SkylensAuth.savePreferencesNow();
    }

    function _getCustomRangeData() {
        var list = [];
        Object.keys(_customRings).forEach(function (id) {
            var c = _customRings[id];
            if (c._rangeData) list.push(c._rangeData);
        });
        return list;
    }

    function addCustomRange(rng, skipSave) {
        if (_customRings[rng.id]) removeCustomRange(rng.id, true);

        // Look up TAP position
        var tapPos = null;
        var tm = tapMarkers[rng.tapId];
        if (tm) {
            var ll = tm.getLatLng();
            tapPos = [ll.lat, ll.lng];
        } else {
            // Try App.state.taps fallback
            var taps = (typeof App !== 'undefined' && App.state && App.state.taps) || [];
            for (var i = 0; i < taps.length; i++) {
                if (taps[i].tap_uuid === rng.tapId && taps[i].latitude && taps[i].longitude) {
                    tapPos = [taps[i].latitude, taps[i].longitude];
                    break;
                }
            }
        }
        if (!tapPos) return; // Can't place without tap position

        var circle = L.circle(tapPos, {
            radius: rng.distance,
            color: rng.color || '#00E676',
            weight: 2,
            fill: false,
            dashArray: '8 4',
            opacity: 0.7
        });
        circle._rangeData = rng;

        if (_customRangesVisible) circle.addTo(map);
        _customRings[rng.id] = circle;

        // Label at north edge
        var R = 6371000;
        var dLat = rng.distance / R;
        var labelLat = tapPos[0] + dLat * (180 / Math.PI);
        var distLabel = rng.distance >= 1000 ? (rng.distance / 1000).toFixed(1) + ' km' : rng.distance + ' m';
        var text = rng.name ? rng.name + ' (' + distLabel + ')' : distLabel;

        var label = L.marker([labelLat, tapPos[1]], {
            icon: L.divIcon({
                className: '',
                iconSize: [80, 16],
                iconAnchor: [40, 8],
                html: '<div style="background:rgba(10,14,13,0.85);color:' + (rng.color || '#00E676') + ';font-size:10px;padding:1px 4px;border-radius:3px;text-align:center;white-space:nowrap;font-family:var(--mono)">' + esc(text) + '</div>'
            }),
            interactive: false
        });
        if (_customRangesVisible) label.addTo(map);
        _customRingLabels[rng.id] = label;

        if (!skipSave) _saveCustomRanges();
    }

    function removeCustomRange(id, skipSave) {
        if (_customRings[id]) {
            if (map.hasLayer(_customRings[id])) map.removeLayer(_customRings[id]);
            delete _customRings[id];
        }
        if (_customRingLabels[id]) {
            if (map.hasLayer(_customRingLabels[id])) map.removeLayer(_customRingLabels[id]);
            delete _customRingLabels[id];
        }
        if (!skipSave) _saveCustomRanges();
    }

    function clearCustomRanges() {
        Object.keys(_customRings).forEach(function (id) { removeCustomRange(id, true); });
        _customRings = {};
        _customRingLabels = {};
        localStorage.removeItem(CUSTOM_RANGE_KEY);
        if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferencesNow) SkylensAuth.savePreferencesNow();
    }

    function setCustomRangesVisible(vis) {
        _customRangesVisible = vis;
        Object.keys(_customRings).forEach(function (id) {
            var c = _customRings[id];
            var l = _customRingLabels[id];
            if (vis) {
                if (!map.hasLayer(c)) c.addTo(map);
                if (l && !map.hasLayer(l)) l.addTo(map);
            } else {
                if (map.hasLayer(c)) map.removeLayer(c);
                if (l && map.hasLayer(l)) map.removeLayer(l);
            }
        });
    }

    function toggleCustomRanges() {
        _customRangesVisible = !_customRangesVisible;
        setCustomRangesVisible(_customRangesVisible);
        try { localStorage.setItem('skylens_range_rings_visible', JSON.stringify(_customRangesVisible)); } catch(e) {}
        if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferencesNow) SkylensAuth.savePreferencesNow();
        return _customRangesVisible;
    }

    function getCustomRanges() {
        return _getCustomRangeData();
    }

    /* ══════════════════════════════════════
       DRAWING TOOLS
       ══════════════════════════════════════ */
    function startDraw(type) {
        if (_drawCancel) _drawCancel();
        _drawMode = type;
        var points = [];
        var tempLayer = null;
        var lastClickTime = 0;

        function onClick(e) {
            var now = Date.now();
            if (now - lastClickTime < 400) return;
            lastClickTime = now;
            points.push([e.latlng.lat, e.latlng.lng]);
            if (type === 'marker') {
                var mk = L.marker(e.latlng).addTo(map);
                drawItems.push(mk);
                endDraw();
                return;
            }
            if (type === 'circle' && points.length === 2) {
                var center = L.latLng(points[0]);
                var radius = center.distanceTo(L.latLng(points[1]));
                drawItems.push(L.circle(points[0], { radius: radius, color: '#00B0FF', weight: 2, fillOpacity: 0.05 }).addTo(map));
                endDraw();
                return;
            }
            if (tempLayer) map.removeLayer(tempLayer);
            if (type === 'polyline' || type === 'polygon' || type === 'rectangle') {
                tempLayer = L.polyline(points, { color: '#00B0FF', weight: 2, dashArray: '4 4' }).addTo(map);
            }
        }

        function onDblClick() {
            endDraw();
            if (points.length < 2) return;
            if (type === 'polyline') {
                drawItems.push(L.polyline(points, { color: '#00B0FF', weight: 2 }).addTo(map));
            } else if (type === 'polygon') {
                drawItems.push(L.polygon(points, { color: '#00B0FF', weight: 2, fillOpacity: 0.05 }).addTo(map));
            } else if (type === 'rectangle' && points.length >= 2) {
                drawItems.push(L.rectangle([points[0], points[points.length - 1]], { color: '#00B0FF', weight: 2, fillOpacity: 0.05 }).addTo(map));
            }
        }

        function endDraw() {
            map.off('click', onClick);
            map.off('dblclick', onDblClick);
            if (tempLayer) map.removeLayer(tempLayer);
            _drawMode = null;
            _drawCancel = null;
        }

        _drawCancel = endDraw;
        map.on('click', onClick);
        map.on('dblclick', onDblClick);
    }

    function cancelDraw() {
        if (_drawCancel) { _drawCancel(); return true; }
        return false;
    }

    function clearDrawings() {
        drawItems.forEach(function (l) { map.removeLayer(l); });
        drawItems = [];
    }

    /* ══════════════════════════════════════
       MEASUREMENT
       ══════════════════════════════════════ */
    function startMeasure() {
        measurePoints = [];
        if (measureLine) { map.removeLayer(measureLine); measureLine = null; }
        measureLabels.forEach(function (l) { map.removeLayer(l); });
        measureLabels = [];

        function fmtDist(d) { return d >= 1000 ? (d / 1000).toFixed(2) + ' km' : d.toFixed(0) + ' m'; }

        function onClick(e) {
            measurePoints.push(e.latlng);
            if (measurePoints.length > 1) {
                if (measureLine) map.removeLayer(measureLine);
                measureLine = L.polyline(measurePoints, { color: '#4CAF50', weight: 2, dashArray: '6 4' }).addTo(map);
                var seg = measurePoints[measurePoints.length - 2].distanceTo(measurePoints[measurePoints.length - 1]);
                var total = 0;
                for (var i = 1; i < measurePoints.length; i++) total += measurePoints[i - 1].distanceTo(measurePoints[i]);
                var mid = L.latLng(
                    (measurePoints[measurePoints.length - 2].lat + measurePoints[measurePoints.length - 1].lat) / 2,
                    (measurePoints[measurePoints.length - 2].lng + measurePoints[measurePoints.length - 1].lng) / 2
                );
                var label = L.marker(mid, {
                    icon: L.divIcon({
                        className: '', iconSize: [60, 16], iconAnchor: [30, 8],
                        html: '<div style="background:rgba(10,14,13,0.85);color:#4CAF50;font-size:10px;padding:1px 4px;border-radius:3px;text-align:center;white-space:nowrap;font-family:var(--mono)">' + fmtDist(seg) + '</div>'
                    }),
                    interactive: false
                }).addTo(map);
                measureLabels.push(label);
                var el = document.getElementById('measure-val');
                if (el) el.textContent = fmtDist(total) + ' (' + measurePoints.length + ' pts)';
            }
        }

        map.on('click', onClick);
        map._measureClick = onClick;
    }

    function stopMeasure() {
        if (map._measureClick) { map.off('click', map._measureClick); delete map._measureClick; }
        if (measureLine) { map.removeLayer(measureLine); measureLine = null; }
        measureLabels.forEach(function (l) { map.removeLayer(l); });
        measureLabels = [];
        measurePoints = [];
    }

    /* ══════════════════════════════════════
       UTILITIES
       ══════════════════════════════════════ */
    function fitAll(_retryCount) {
        _retryCount = _retryCount || 0;

        // If map is animating, retry after a short delay (max 5 retries)
        if (_zoomAnimating || (map && map._animatingZoom)) {
            if (_retryCount < 5) {
                setTimeout(function() { fitAll(_retryCount + 1); }, 200);
            }
            return;
        }

        var all = Object.values(droneMarkers).concat(Object.values(tapMarkers)).concat(Object.values(operatorMarkers));
        if (all.length === 0) return;
        map.fitBounds(L.featureGroup(all).getBounds().pad(0.15), { maxZoom: 16 });
    }

    function selectDrone(id, _retryCount) {
        console.log('selectDrone called:', id, 'retry:', _retryCount, 'hasMarker:', !!droneMarkers[id], 'hasRing:', !!distanceRings[id]);
        if (!id) return;
        _retryCount = _retryCount || 0;

        // If map is animating, retry after a short delay (max 5 retries)
        if (_zoomAnimating || (map && map._animatingZoom)) {
            console.log('selectDrone: map animating, retry later');
            if (_retryCount < 5) {
                setTimeout(function() { selectDrone(id, _retryCount + 1); }, 200);
            }
            return;
        }

        // Force map to recalculate size (fixes issue where map doesn't move on first click)
        map.invalidateSize({ pan: false });

        /* Check if this drone has a distance ring (fingerprint drone) */
        if (distanceRings[id]) {
            var ring = distanceRings[id];
            /* Fit map to show the entire ring with smooth animation */
            setTimeout(function() {
                map.fitBounds(ring.getBounds().pad(0.15), {
                    animate: true,
                    duration: 0.5,
                    easeLinearity: 0.25,
                    maxZoom: 15
                });
            }, 10);
            /* Highlight the ring with smooth pulse animation */
            var origWeight = ring.options.weight;
            var origOpacity = ring.options.opacity;
            var origColor = ring.options.color;
            ring.setStyle({ weight: origWeight + 3, opacity: 1, color: '#FFFFFF' });
            setTimeout(function() {
                ring.setStyle({ weight: origWeight + 2, opacity: 0.95, color: origColor });
            }, 150);
            setTimeout(function() {
                ring.setStyle({ weight: origWeight, opacity: origOpacity, color: origColor });
            }, 1500);
            /* Open popup on the ring */
            setTimeout(function() { ring.openPopup(); }, 300);
            return;
        }

        /* Regular drone with marker - fit to show drone + operator */
        if (droneMarkers[id]) {
            var dronePos = droneMarkers[id].getLatLng();

            /* Include operator position if available */
            if (operatorMarkers[id]) {
                var opPos = operatorMarkers[id].getLatLng();
                var bounds = L.latLngBounds([dronePos, opPos]);
                // First do instant move to ensure map responds
                map.fitBounds(bounds.pad(0.3), { animate: false, maxZoom: 17 });
            } else {
                /* Single point - zoom directly to drone */
                // First do instant move to ensure map responds
                map.setView(dronePos, 16, { animate: false });
            }
            /* Don't auto-open popup - user can click if they want details */
        } else {
            console.log('selectDrone: no marker found for', id);
        }
    }

    function removeDrone(id) {
        /* Cancel any pending animations and timeouts */
        if (_anims[id]) delete _anims[id];
        if (_pendingTimeouts[id]) { clearTimeout(_pendingTimeouts[id]); delete _pendingTimeouts[id]; }

        /* Remove all map layers for this drone */
        if (droneMarkers[id])    { if (map.hasLayer(droneMarkers[id]))    map.removeLayer(droneMarkers[id]);    delete droneMarkers[id]; }
        delete _controllerIds[id];
        delete _styleCache[id];
        delete _popupLastUpdate[id];
        delete _lastDronePos[id];
        if (trailLines[id])      { map.removeLayer(trailLines[id]);      delete trailLines[id]; }
        if (operatorMarkers[id]) { map.removeLayer(operatorMarkers[id]); delete operatorMarkers[id]; }
        if (connLines[id])       { map.removeLayer(connLines[id]);       delete connLines[id]; }
        if (threatRings[id])     { map.removeLayer(threatRings[id]);     delete threatRings[id]; }
        if (speedVectors[id])    { map.removeLayer(speedVectors[id]);    delete speedVectors[id]; }
        if (headingLines[id])    { map.removeLayer(headingLines[id]);    delete headingLines[id]; }
        /* Remove all range rings for this drone (multi-TAP keys: id::tapId, id::est, etc.) */
        if (distanceRings[id])   { map.removeLayer(distanceRings[id]);   delete distanceRings[id]; }
        if (ringLabels[id])      { map.removeLayer(ringLabels[id]);      delete ringLabels[id]; }
        var prefix = id + '::';
        var drk = Object.keys(distanceRings);
        for (var dri = 0; dri < drk.length; dri++) {
            if (drk[dri].indexOf(prefix) === 0) {
                map.removeLayer(distanceRings[drk[dri]]); delete distanceRings[drk[dri]];
                if (ringLabels[drk[dri]]) { map.removeLayer(ringLabels[drk[dri]]); delete ringLabels[drk[dri]]; }
            }
        }
        if (historyLines[id])    { hideFlightPath(id); }
        delete trails[id];
        delete _bestRingTap[id];
    }

    function getInfo() {
        var dc = Object.keys(droneMarkers).length;
        var tc = Object.keys(tapMarkers).length;
        var zc = Object.keys(zonePolygons).length;
        return dc + ' drone' + (dc !== 1 ? 's' : '') +
            ' \u00b7 ' + tc + ' sensor' + (tc !== 1 ? 's' : '') +
            ' \u00b7 ' + zc + ' zone' + (zc !== 1 ? 's' : '') +
            ' \u00b7 ' + Object.keys(trailLines).length + ' trail' + (Object.keys(trailLines).length !== 1 ? 's' : '');
    }

    /* ── zone violation check ─────────── */
    function checkZoneViolations(drones) {
        var violations = [];
        var zones = getZones();
        if (!zones.length) return violations;
        drones.forEach(function (d) {
            if (d.latitude == null || d.longitude == null) return;
            if (d._contactStatus === 'lost') return;
            var id = d.identifier || d.mac;
            zones.forEach(function (z) {
                if (z.type !== 'restricted' && z.type !== 'alert') return;
                if (pointInPolygon([d.latitude, d.longitude], z.points)) {
                    violations.push({ droneId: id, zoneName: z.name, zoneType: z.type, designation: d.designation || id });
                    if (zonePolygons[z.id]) {
                        var poly = zonePolygons[z.id];
                        poly.setStyle({ fillOpacity: 0.25 });
                        setTimeout(function () { poly.setStyle({ fillOpacity: 0.08 }); }, 1500);
                    }
                }
            });
        });
        return violations;
    }

    function pointInPolygon(point, polygon) {
        var x = point[0], y = point[1], inside = false;
        for (var i = 0, j = polygon.length - 1; i < polygon.length; j = i++) {
            var xi = polygon[i][0], yi = polygon[i][1];
            var xj = polygon[j][0], yj = polygon[j][1];
            if (((yi > y) !== (yj > y)) && (x < (xj - xi) * (y - yi) / (yj - yi) + xi)) inside = !inside;
        }
        return inside;
    }

    /* ── user location ───────────────────── */
    function setUserLocation(lat, lng) {
        if (lat == null || lng == null) return;
        var pos = [lat, lng];
        if (_userMarker) {
            _userMarker.setLatLng(pos);
        } else {
            _userMarker = L.marker(pos, {
                icon: L.divIcon({
                    className: '',
                    html: '<div style="width:14px;height:14px;border-radius:50%;background:rgba(33,150,243,0.3);border:2px solid #2196F3;box-shadow:0 0 10px rgba(33,150,243,0.5);"></div>' +
                        '<div style="position:absolute;top:3px;left:3px;width:8px;height:8px;border-radius:50%;background:#2196F3;animation:user-pulse 2s ease-in-out infinite;"></div>',
                    iconSize: [14, 14], iconAnchor: [7, 7]
                }),
                interactive: false, zIndexOffset: -100
            }).addTo(map);
        }
    }

    /* ── auto-follow ─────────────────────── */
    function setAutoFollow(on) { _autoFollow = !!on; }
    function isAutoFollow() { return _autoFollow; }

    /* ── tap distance ────────────────────── */
    function getTapPosition() {
        var ids = Object.keys(tapMarkers);
        if (!ids.length) return null;
        var ll = tapMarkers[ids[0]].getLatLng();
        return { lat: ll.lat, lng: ll.lng };
    }

    /* Get tap position by UUID (for ring centering) */
    function getTapPositionForUav(tapUuid) {
        if (!tapUuid) return null;
        var marker = tapMarkers[tapUuid];
        if (marker) {
            var ll = marker.getLatLng();
            return [ll.lat, ll.lng];
        }
        /* Fallback: return first tap position */
        var ids = Object.keys(tapMarkers);
        if (ids.length) {
            var ll = tapMarkers[ids[0]].getLatLng();
            return [ll.lat, ll.lng];
        }
        return null;
    }

    function getMap() { return map; }

    /* ══════════════════════════════════════
       HISTORICAL FLIGHT PATH
       ══════════════════════════════════════ */
    function showFlightPath(identifier, callback) {
        if (historyLines[identifier]) {
            /* Already showing - toggle off */
            hideFlightPath(identifier);
            if (callback) callback(false);
            return;
        }

        fetch('/api/uav/' + encodeURIComponent(identifier) + '/history')
            .then(function (r) { return r.json(); })
            .then(function (data) {
                if (data.error || !data.positions || data.positions.length < 2) {
                    console.warn('No flight history for', identifier);
                    if (callback) callback(false);
                    return;
                }

                var coords = data.positions.map(function (p) {
                    return [p.lat, p.lng];
                });

                /* Create gradient polyline: older points dimmer */
                var line = L.polyline(coords, {
                    color: '#AA00FF',      /* purple - distinct from trail */
                    weight: 3,
                    opacity: 0.8,
                    lineCap: 'round',
                    lineJoin: 'round'
                }).addTo(map);

                /* Add start/end markers */
                if (coords.length > 0) {
                    var startIcon = L.divIcon({
                        className: '',
                        html: '<div style="width:8px;height:8px;border-radius:50%;background:#00E676;border:2px solid #fff;box-shadow:0 0 4px rgba(0,0,0,0.5);"></div>',
                        iconSize: [8, 8], iconAnchor: [4, 4]
                    });
                    var endIcon = L.divIcon({
                        className: '',
                        html: '<div style="width:8px;height:8px;border-radius:50%;background:#FF1744;border:2px solid #fff;box-shadow:0 0 4px rgba(0,0,0,0.5);"></div>',
                        iconSize: [8, 8], iconAnchor: [4, 4]
                    });
                    line._startMarker = L.marker(coords[0], { icon: startIcon, interactive: false }).addTo(map);
                    line._endMarker = L.marker(coords[coords.length - 1], { icon: endIcon, interactive: false }).addTo(map);
                }

                /* Store stats on the line for popup */
                line._historyStats = data.stats;
                line.bindPopup(
                    '<div class="pop-t">Flight History</div>' +
                    pr('Points', data.stats.position_count) +
                    pr('Distance', (data.stats.total_distance_m / 1000).toFixed(2) + ' km') +
                    pr('Max Alt', data.stats.max_altitude_m.toFixed(0) + ' m') +
                    pr('Max Speed', data.stats.max_speed_ms.toFixed(1) + ' m/s') +
                    pr('Duration', formatDuration(data.stats.duration_s)),
                    { closeButton: false }
                );

                historyLines[identifier] = line;
                historyVisible[identifier] = true;

                /* Fit bounds to show full path */
                map.fitBounds(line.getBounds().pad(0.1), { maxZoom: 16 });

                if (callback) callback(true);
            })
            .catch(function (err) {
                console.error('Failed to load flight history:', err);
                if (callback) callback(false);
            });
    }

    function hideFlightPath(identifier) {
        var line = historyLines[identifier];
        if (line) {
            if (line._startMarker) map.removeLayer(line._startMarker);
            if (line._endMarker) map.removeLayer(line._endMarker);
            map.removeLayer(line);
            delete historyLines[identifier];
        }
        historyVisible[identifier] = false;
    }

    function isFlightPathVisible(identifier) {
        return !!historyVisible[identifier];
    }

    function formatDuration(seconds) {
        if (seconds < 60) return seconds.toFixed(0) + 's';
        if (seconds < 3600) return Math.floor(seconds / 60) + 'm ' + Math.floor(seconds % 60) + 's';
        var h = Math.floor(seconds / 3600);
        var m = Math.floor((seconds % 3600) / 60);
        return h + 'h ' + m + 'm';
    }

    function updateSettings(s) {
        if (s.maxTrail != null) settings.maxTrail = s.maxTrail;
        if (s.threatRadius != null) settings.threatRadius = s.threatRadius;
        if (s.tileStyle && s.tileStyle !== settings.tileStyle) setTileStyle(s.tileStyle);
    }

    /* ══════════════════════════════════════
       MEMORY CLEANUP — call periodically
       ══════════════════════════════════════ */
    function cleanup() {
        /* Remove orphaned animation entries */
        var id, ids = Object.keys(_anims);
        for (var i = 0; i < ids.length; i++) {
            id = ids[i];
            if (!droneMarkers[id]) delete _anims[id];
        }
        /* Remove orphaned style cache entries */
        ids = Object.keys(_styleCache);
        for (i = 0; i < ids.length; i++) {
            id = ids[i];
            if (!droneMarkers[id]) delete _styleCache[id];
        }
        /* Remove orphaned pending timeouts */
        ids = Object.keys(_pendingTimeouts);
        for (i = 0; i < ids.length; i++) {
            id = ids[i];
            if (!droneMarkers[id] && !distanceRings[id]) {
                clearTimeout(_pendingTimeouts[id]);
                delete _pendingTimeouts[id];
            }
        }
        /* Remove orphaned trails */
        ids = Object.keys(trails);
        for (i = 0; i < ids.length; i++) {
            id = ids[i];
            if (!droneMarkers[id]) delete trails[id];
        }
        /* Remove orphaned popup/position caches */
        ids = Object.keys(_popupLastUpdate);
        for (i = 0; i < ids.length; i++) {
            id = ids[i];
            if (!droneMarkers[id]) delete _popupLastUpdate[id];
        }
        ids = Object.keys(_lastDronePos);
        for (i = 0; i < ids.length; i++) {
            id = ids[i];
            if (!droneMarkers[id]) delete _lastDronePos[id];
        }
    }

    /* Get memory stats for debugging */
    function getMemoryStats() {
        return {
            droneMarkers: Object.keys(droneMarkers).length,
            operatorMarkers: Object.keys(operatorMarkers).length,
            tapMarkers: Object.keys(tapMarkers).length,
            trails: Object.keys(trails).length,
            trailLines: Object.keys(trailLines).length,
            anims: Object.keys(_anims).length,
            styleCache: Object.keys(_styleCache).length,
            pendingTimeouts: Object.keys(_pendingTimeouts).length,
            distanceRings: Object.keys(distanceRings).length,
            historyLines: Object.keys(historyLines).length,
            popupThrottle: Object.keys(_popupLastUpdate).length,
            posCache: Object.keys(_lastDronePos).length
        };
    }

    /* ══════════════════════════════════════
       PUBLIC API
       ══════════════════════════════════════ */
    return {
        init: init,
        updateDrones: updateDrones,
        updateTaps: updateTaps,
        toggleLayer: toggleLayer,
        layers: layers,
        fitAll: fitAll,
        selectDrone: selectDrone,
        removeDrone: removeDrone,
        getInfo: getInfo,
        getMap: getMap,
        updateSettings: updateSettings,
        setTileStyle: setTileStyle,
        // Show/hide lost
        setShowLost: setShowLost,
        // Show/hide controllers
        setShowControllers: setShowControllers,
        // Drawing
        startDraw: startDraw,
        clearDrawings: clearDrawings,
        cancelDraw: cancelDraw,
        // Measurement
        startMeasure: startMeasure,
        stopMeasure: stopMeasure,
        // User location
        setUserLocation: setUserLocation,
        // Zones
        getZones: getZones,
        removeZone: removeZone,
        clearAllZones: clearAllZones,
        startZoneDraw: startZoneDraw,
        // Custom range rings
        addCustomRange: addCustomRange,
        removeCustomRange: removeCustomRange,
        clearCustomRanges: clearCustomRanges,
        toggleCustomRanges: toggleCustomRanges,
        setCustomRangesVisible: setCustomRangesVisible,
        getCustomRanges: getCustomRanges,
        loadCustomRanges: loadCustomRanges,
        // Zone violations
        checkZoneViolations: checkZoneViolations,
        // Auto-follow
        setAutoFollow: setAutoFollow,
        isAutoFollow: isAutoFollow,
        // Tap position
        getTapPosition: getTapPosition,
        // Flight path history
        showFlightPath: showFlightPath,
        hideFlightPath: hideFlightPath,
        isFlightPathVisible: isFlightPathVisible,
        // Memory management
        cleanup: cleanup,
        getMemoryStats: getMemoryStats
    };
})();
