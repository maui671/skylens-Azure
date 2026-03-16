/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS RANGE RINGS WITH UNCERTAINTY v1.0
   RSSI-based distance estimation rings per tap/drone pair
   Shows inner/outer bands for uncertainty (d_min, d_max)
   Pulse animation on new detection, fade after timeout
   ═══════════════════════════════════════════════════════════════════════════ */

var RangeRings = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        // RSSI to distance model (log-distance path loss)
        // Distance = 10 ^ ((RSSI_0 - RSSI) / (10 * n))
        // Calibrated from 40 ground-truth GPS RemoteID points (Feb 2026)
        pathLossExponent: 2.6,
        txPower: 18.4,  // BaseRSSI0 calibrated from Skylens TAPs (RTL8814AU/RTL8812AU/MT7921U)

        // Uncertainty band (percentage)
        uncertaintyLow: 0.7,   // d_min = d * 0.7
        uncertaintyHigh: 1.4,  // d_max = d * 1.4

        // Ring fade timeout (ms)
        fadeTimeout: 30000,

        // Color palette for drones (cycled)
        colorPalette: [
            '#00E676', '#00B0FF', '#AA00FF', '#FF4081',
            '#FFD740', '#64FFDA', '#FF6E40', '#7C4DFF',
            '#69F0AE', '#40C4FF', '#E040FB', '#FF80AB'
        ],

        // Animation
        pulseDuration: 600,

        // Opacity
        ringOpacity: 0.6,
        bandOpacity: 0.15,
        fadedOpacity: 0.2
    };

    // ─── STATE ───
    var state = {
        map: null,
        rings: {},       // droneId -> { center, inner, outer, label, lastUpdate, color }
        colorMap: {},    // droneId -> color
        colorIndex: 0,
        fadeTimers: {},  // droneId -> timer
        pane: null
    };

    // ─── INITIALIZE ───
    function init(map) {
        if (!map) {
            console.error('RangeRings: Map instance required');
            return false;
        }

        state.map = map;

        // Create custom pane
        if (!map.getPane('rangeRingsPane')) {
            state.pane = map.createPane('rangeRingsPane');
            state.pane.style.zIndex = 340;
        }

        return true;
    }

    // ─── RSSI TO DISTANCE CALCULATION ───
    function rssiToDistance(rssi) {
        // Log-distance path loss model
        // RSSI = TxPower - 10 * n * log10(d)
        // d = 10 ^ ((TxPower - RSSI) / (10 * n))
        var d = Math.pow(10, (CONFIG.txPower - rssi) / (10 * CONFIG.pathLossExponent));
        return Math.max(10, Math.min(d, 50000));  // Clamp 10m - 50km
    }

    // ─── GET COLOR FOR DRONE ───
    function getDroneColor(droneId) {
        if (!state.colorMap[droneId]) {
            state.colorMap[droneId] = CONFIG.colorPalette[state.colorIndex % CONFIG.colorPalette.length];
            state.colorIndex++;
        }
        return state.colorMap[droneId];
    }

    // ─── UPDATE DETECTION ───
    function updateDetection(detection) {
        if (!state.map) return;

        var droneId = detection.identifier || detection.droneId;
        var tapId = detection.tap_uuid || detection.tapId;

        if (!droneId || !tapId) return;

        // Get tap position
        var tapPos = detection.tapPosition || getTapPosition(tapId);
        if (!tapPos) return;

        // Calculate distance from RSSI
        var rssi = detection.rssi;
        if (rssi == null) return;

        var distance = rssiToDistance(rssi);
        var dMin = distance * CONFIG.uncertaintyLow;
        var dMax = distance * CONFIG.uncertaintyHigh;

        var color = getDroneColor(droneId);
        var key = droneId + ':' + tapId;

        // Check if ring exists
        var existing = state.rings[key];

        if (existing) {
            // Update existing ring
            updateRing(key, tapPos, distance, dMin, dMax, detection);
        } else {
            // Create new ring
            createRing(key, droneId, tapPos, distance, dMin, dMax, color, detection);
        }

        // Reset fade timer
        resetFadeTimer(key);
    }

    // ─── CREATE RING ───
    function createRing(key, droneId, tapPos, distance, dMin, dMax, color, detection) {
        var center = [tapPos.lat, tapPos.lng];

        // Outer uncertainty band (filled)
        var outer = L.circle(center, {
            radius: dMax,
            color: color,
            weight: 0,
            fill: true,
            fillColor: color,
            fillOpacity: CONFIG.bandOpacity,
            interactive: false,
            pane: 'rangeRingsPane',
            className: 'rr-band rr-outer'
        }).addTo(state.map);

        // Inner uncertainty band (creates donut when combined with outer)
        var inner = L.circle(center, {
            radius: dMin,
            color: color,
            weight: 0,
            fill: true,
            fillColor: '#0A0E0D',  // Match background
            fillOpacity: 0.8,
            interactive: false,
            pane: 'rangeRingsPane',
            className: 'rr-band rr-inner'
        }).addTo(state.map);

        // Center ring (estimated distance)
        var centerRing = L.circle(center, {
            radius: distance,
            color: color,
            weight: 2,
            fill: false,
            opacity: CONFIG.ringOpacity,
            dashArray: '8 4',
            interactive: false,
            pane: 'rangeRingsPane',
            className: 'rr-center rr-pulse'
        }).addTo(state.map);

        // Label at north edge
        var labelLat = tapPos.lat + (distance / 111320);
        var label = L.marker([labelLat, tapPos.lng], {
            icon: L.divIcon({
                className: 'rr-label',
                html: buildLabelHtml(droneId, distance, detection.rssi, color),
                iconSize: [100, 36],
                iconAnchor: [50, 18]
            }),
            interactive: false,
            pane: 'rangeRingsPane'
        }).addTo(state.map);

        state.rings[key] = {
            outer: outer,
            inner: inner,
            center: centerRing,
            label: label,
            color: color,
            droneId: droneId,
            lastUpdate: Date.now(),
            distance: distance,
            rssi: detection.rssi
        };

        // Trigger pulse animation
        triggerPulse(key);
    }

    // ─── UPDATE RING ───
    function updateRing(key, tapPos, distance, dMin, dMax, detection) {
        var ring = state.rings[key];
        if (!ring) return;

        var center = [tapPos.lat, tapPos.lng];

        // Only update if RSSI changed significantly (> 2 dBm)
        var rssiDelta = Math.abs((ring.rssi || -999) - detection.rssi);
        if (rssiDelta > 2 || Math.abs(ring.distance - distance) > distance * 0.1) {
            ring.outer.setLatLng(center).setRadius(dMax);
            ring.inner.setLatLng(center).setRadius(dMin);
            ring.center.setLatLng(center).setRadius(distance);

            // Update label position and content
            var labelLat = tapPos.lat + (distance / 111320);
            ring.label.setLatLng([labelLat, tapPos.lng]);
            ring.label.setIcon(L.divIcon({
                className: 'rr-label',
                html: buildLabelHtml(ring.droneId, distance, detection.rssi, ring.color),
                iconSize: [100, 36],
                iconAnchor: [50, 18]
            }));

            ring.distance = distance;
            ring.rssi = detection.rssi;

            // Trigger pulse animation
            triggerPulse(key);
        }

        ring.lastUpdate = Date.now();

        // Restore full opacity if faded
        ring.outer.setStyle({ fillOpacity: CONFIG.bandOpacity });
        ring.center.setStyle({ opacity: CONFIG.ringOpacity });
    }

    // ─── BUILD LABEL HTML ───
    function buildLabelHtml(droneId, distance, rssi, color) {
        var name = droneId.length > 12 ? droneId.substring(0, 10) + '..' : droneId;
        var distStr = distance < 1000 ? Math.round(distance) + 'm' : (distance / 1000).toFixed(1) + 'km';

        return '<div class="rr-label-inner" style="border-left-color:' + color + '">' +
            '<div class="rr-label-name" style="color:' + color + '">' + escapeHtml(name) + '</div>' +
            '<div class="rr-label-dist">' + distStr + ' <span class="rr-label-rssi">' + rssi + 'dBm</span></div>' +
        '</div>';
    }

    // ─── TRIGGER PULSE ANIMATION ───
    function triggerPulse(key) {
        var ring = state.rings[key];
        if (!ring || !ring.center._path) return;

        var el = ring.center._path;
        el.classList.remove('rr-pulse-active');
        void el.offsetWidth;  // Force reflow
        el.classList.add('rr-pulse-active');

        setTimeout(function() {
            if (el) el.classList.remove('rr-pulse-active');
        }, CONFIG.pulseDuration);
    }

    // ─── FADE TIMER ───
    function resetFadeTimer(key) {
        if (state.fadeTimers[key]) {
            clearTimeout(state.fadeTimers[key]);
        }

        state.fadeTimers[key] = setTimeout(function() {
            fadeRing(key);
        }, CONFIG.fadeTimeout);
    }

    function fadeRing(key) {
        var ring = state.rings[key];
        if (!ring) return;

        ring.outer.setStyle({ fillOpacity: CONFIG.fadedOpacity * 0.5 });
        ring.center.setStyle({ opacity: CONFIG.fadedOpacity });
    }

    // ─── REMOVE RING ───
    function removeRing(droneId, tapId) {
        var key = tapId ? droneId + ':' + tapId : null;

        if (key && state.rings[key]) {
            var ring = state.rings[key];
            state.map.removeLayer(ring.outer);
            state.map.removeLayer(ring.inner);
            state.map.removeLayer(ring.center);
            state.map.removeLayer(ring.label);
            delete state.rings[key];

            if (state.fadeTimers[key]) {
                clearTimeout(state.fadeTimers[key]);
                delete state.fadeTimers[key];
            }
        } else if (droneId) {
            // Remove all rings for this drone
            Object.keys(state.rings).forEach(function(k) {
                if (k.startsWith(droneId + ':')) {
                    removeRing(null, null);  // Recursive call handled above
                    var ring = state.rings[k];
                    if (ring) {
                        state.map.removeLayer(ring.outer);
                        state.map.removeLayer(ring.inner);
                        state.map.removeLayer(ring.center);
                        state.map.removeLayer(ring.label);
                        delete state.rings[k];
                    }
                }
            });
        }
    }

    // ─── CLEAR ALL RINGS ───
    function clearAll() {
        Object.keys(state.rings).forEach(function(key) {
            var ring = state.rings[key];
            state.map.removeLayer(ring.outer);
            state.map.removeLayer(ring.inner);
            state.map.removeLayer(ring.center);
            state.map.removeLayer(ring.label);
        });

        Object.keys(state.fadeTimers).forEach(function(key) {
            clearTimeout(state.fadeTimers[key]);
        });

        state.rings = {};
        state.fadeTimers = {};
    }

    // ─── BATCH UPDATE ───
    function updateFromUAVs(uavs, taps) {
        if (!state.map) return;

        // Build tap position lookup
        var tapPositions = {};
        (taps || []).forEach(function(t) {
            if (t.tap_uuid && t.latitude && t.longitude) {
                tapPositions[t.tap_uuid] = { lat: t.latitude, lng: t.longitude };
            }
        });

        // Track seen rings
        var seen = {};

        (uavs || []).forEach(function(u) {
            if (!u.rssi || u.rssi === 0) return;
            if (!u.tap_uuid) return;

            var tapPos = tapPositions[u.tap_uuid];
            if (!tapPos) return;

            var key = (u.identifier || u.mac) + ':' + u.tap_uuid;
            seen[key] = true;

            updateDetection({
                identifier: u.identifier || u.mac,
                tap_uuid: u.tap_uuid,
                tapPosition: tapPos,
                rssi: u.rssi
            });
        });

        // Remove stale rings
        Object.keys(state.rings).forEach(function(key) {
            if (!seen[key]) {
                var ring = state.rings[key];
                var age = Date.now() - ring.lastUpdate;
                if (age > CONFIG.fadeTimeout * 2) {
                    removeRing(key.split(':')[0], key.split(':')[1]);
                }
            }
        });
    }

    // ─── HELPER: GET TAP POSITION ───
    function getTapPosition(tapId) {
        // Try to get from NzMap if available
        if (typeof NzMap !== 'undefined') {
            // First check if getTapPosition exists
            if (NzMap.getTapPosition) {
                return NzMap.getTapPosition();
            }
        }

        // Try App.state
        if (typeof App !== 'undefined' && App.state && App.state.taps) {
            var tap = App.state.taps.find(function(t) {
                return t.tap_uuid === tapId;
            });
            if (tap && tap.latitude && tap.longitude) {
                return { lat: tap.latitude, lng: tap.longitude };
            }
        }

        return null;
    }

    // ─── HELPER: ESCAPE HTML ───
    var _escDiv = document.createElement('div');
    function escapeHtml(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    // ─── CONFIGURE ───
    function configure(options) {
        if (options.pathLossExponent) CONFIG.pathLossExponent = options.pathLossExponent;
        if (options.txPower) CONFIG.txPower = options.txPower;
        if (options.uncertaintyLow) CONFIG.uncertaintyLow = options.uncertaintyLow;
        if (options.uncertaintyHigh) CONFIG.uncertaintyHigh = options.uncertaintyHigh;
        if (options.fadeTimeout) CONFIG.fadeTimeout = options.fadeTimeout;
        if (options.colorPalette) CONFIG.colorPalette = options.colorPalette;
    }

    // ─── GET STATE ───
    function getRingCount() {
        return Object.keys(state.rings).length;
    }

    function getRings() {
        return Object.keys(state.rings).map(function(key) {
            var ring = state.rings[key];
            return {
                key: key,
                droneId: ring.droneId,
                distance: ring.distance,
                rssi: ring.rssi,
                color: ring.color,
                lastUpdate: ring.lastUpdate
            };
        });
    }

    // ─── PUBLIC API ───
    return {
        init: init,
        updateDetection: updateDetection,
        updateFromUAVs: updateFromUAVs,
        removeRing: removeRing,
        clearAll: clearAll,
        configure: configure,
        getRingCount: getRingCount,
        getRings: getRings,
        rssiToDistance: rssiToDistance,
        CONFIG: CONFIG
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = RangeRings;
}
