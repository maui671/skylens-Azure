/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS TACTICAL SITUATION DISPLAY (TSD) v1.0
   Radar-style overlay for Leaflet map with range rings, bearing lines,
   sweep animation, and threat vectors
   ═══════════════════════════════════════════════════════════════════════════ */

var TacticalDisplay = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        // Range rings in meters
        rangeRings: [500, 1000, 2000, 5000],
        // Bearing lines every N degrees
        bearingInterval: 30,
        // Bearing labels
        bearingLabels: {
            0: 'N', 30: 'NNE', 60: 'ENE', 90: 'E',
            120: 'ESE', 150: 'SSE', 180: 'S',
            210: 'SSW', 240: 'WSW', 270: 'W',
            300: 'WNW', 330: 'NNW'
        },
        // Threat vector prediction times in seconds
        vectorTimes: [30, 60],
        // Colors
        colors: {
            ringPrimary: 'rgba(0, 230, 118, 0.6)',
            ringSecondary: 'rgba(0, 230, 118, 0.3)',
            bearingLine: 'rgba(0, 230, 118, 0.25)',
            bearingLabel: '#00E676',
            sweep: 'rgba(0, 230, 118, 0.15)',
            sweepLine: 'rgba(0, 230, 118, 0.8)',
            vector30s: 'rgba(255, 179, 0, 0.7)',
            vector60s: 'rgba(255, 109, 0, 0.5)',
            centerMarker: '#00E676'
        },
        // Animation
        sweepDuration: 4000,  // ms for full rotation
        // Default center (will be set by user or first tap)
        defaultCenter: null
    };

    // ─── STATE ───
    var state = {
        enabled: false,
        sweepEnabled: true,
        map: null,
        center: null,      // { lat, lng }
        layers: {
            rings: [],
            bearings: [],
            sweep: null,
            vectors: [],
            centerMarker: null
        },
        sweepAngle: 0,
        sweepAnimId: null,
        lastSweepTime: 0,
        uavs: [],
        pane: null
    };

    // ─── INITIALIZE ───
    function init(map, center) {
        if (!map) {
            console.error('TacticalDisplay: Map instance required');
            return false;
        }

        state.map = map;
        state.center = center || CONFIG.defaultCenter;

        // Create custom pane for TSD elements (below markers, above tiles)
        if (!map.getPane('tsdPane')) {
            state.pane = map.createPane('tsdPane');
            state.pane.style.zIndex = 350;  // Below markerPane (400)
        }

        return true;
    }

    // ─── ENABLE TSD MODE ───
    function enable(center) {
        if (!state.map) {
            console.error('TacticalDisplay: Not initialized');
            return false;
        }

        if (center) {
            state.center = center;
        } else if (!state.center) {
            // Try to get center from first tap
            if (typeof NzMap !== 'undefined' && NzMap.getTapPosition) {
                var tapPos = NzMap.getTapPosition();
                if (tapPos) {
                    state.center = tapPos;
                }
            }
        }

        if (!state.center) {
            // Use map center as fallback
            var mapCenter = state.map.getCenter();
            state.center = { lat: mapCenter.lat, lng: mapCenter.lng };
        }

        state.enabled = true;

        // Draw all elements
        drawRangeRings();
        drawBearingLines();
        drawCenterMarker();

        if (state.sweepEnabled) {
            startSweep();
        }

        // Add TSD class to map container for styling
        state.map.getContainer().classList.add('tsd-mode');

        return true;
    }

    // ─── DISABLE TSD MODE ───
    function disable() {
        state.enabled = false;
        stopSweep();
        clearAllLayers();

        if (state.map) {
            state.map.getContainer().classList.remove('tsd-mode');
        }
    }

    // ─── TOGGLE TSD MODE ───
    function toggle(center) {
        if (state.enabled) {
            disable();
        } else {
            enable(center);
        }
        return state.enabled;
    }

    // ─── SET CENTER ───
    function setCenter(lat, lng) {
        state.center = { lat: lat, lng: lng };

        if (state.enabled) {
            // Redraw all elements
            clearAllLayers();
            drawRangeRings();
            drawBearingLines();
            drawCenterMarker();
        }
    }

    // ─── DRAW RANGE RINGS ───
    function drawRangeRings() {
        if (!state.center || !state.map) return;

        var centerLatLng = [state.center.lat, state.center.lng];

        CONFIG.rangeRings.forEach(function(radius, idx) {
            var isPrimary = idx === 0 || idx === CONFIG.rangeRings.length - 1;
            var color = isPrimary ? CONFIG.colors.ringPrimary : CONFIG.colors.ringSecondary;
            var weight = isPrimary ? 2 : 1;

            var ring = L.circle(centerLatLng, {
                radius: radius,
                color: color,
                weight: weight,
                fill: false,
                dashArray: isPrimary ? null : '8 6',
                interactive: false,
                pane: 'tsdPane'
            }).addTo(state.map);

            // Add range label at north edge
            var labelLat = state.center.lat + (radius / 111320);
            var labelPos = [labelLat, state.center.lng];
            var label = L.marker(labelPos, {
                icon: L.divIcon({
                    className: 'tsd-range-label',
                    html: '<span>' + formatDistance(radius) + '</span>',
                    iconSize: [60, 16],
                    iconAnchor: [30, 8]
                }),
                interactive: false,
                pane: 'tsdPane'
            }).addTo(state.map);

            state.layers.rings.push(ring);
            state.layers.rings.push(label);
        });
    }

    // ─── DRAW BEARING LINES ───
    function drawBearingLines() {
        if (!state.center || !state.map) return;

        var maxRange = CONFIG.rangeRings[CONFIG.rangeRings.length - 1];
        var centerLatLng = [state.center.lat, state.center.lng];

        for (var bearing = 0; bearing < 360; bearing += CONFIG.bearingInterval) {
            var radians = bearing * Math.PI / 180;

            // Calculate end point
            var endLat = state.center.lat + (maxRange / 111320) * Math.cos(radians);
            var endLng = state.center.lng + (maxRange / (111320 * Math.cos(state.center.lat * Math.PI / 180))) * Math.sin(radians);

            var line = L.polyline([centerLatLng, [endLat, endLng]], {
                color: CONFIG.colors.bearingLine,
                weight: 1,
                dashArray: '4 8',
                interactive: false,
                pane: 'tsdPane'
            }).addTo(state.map);

            state.layers.bearings.push(line);

            // Add bearing label
            var label = CONFIG.bearingLabels[bearing];
            if (label) {
                var labelDist = maxRange * 1.08;
                var labelLat = state.center.lat + (labelDist / 111320) * Math.cos(radians);
                var labelLng = state.center.lng + (labelDist / (111320 * Math.cos(state.center.lat * Math.PI / 180))) * Math.sin(radians);

                var labelMarker = L.marker([labelLat, labelLng], {
                    icon: L.divIcon({
                        className: 'tsd-bearing-label',
                        html: '<span>' + label + '</span>',
                        iconSize: [30, 16],
                        iconAnchor: [15, 8]
                    }),
                    interactive: false,
                    pane: 'tsdPane'
                }).addTo(state.map);

                state.layers.bearings.push(labelMarker);
            }
        }
    }

    // ─── DRAW CENTER MARKER ───
    function drawCenterMarker() {
        if (!state.center || !state.map) return;

        var centerLatLng = [state.center.lat, state.center.lng];

        // Create a crosshair marker
        state.layers.centerMarker = L.marker(centerLatLng, {
            icon: L.divIcon({
                className: 'tsd-center-marker',
                html: '<div class="tsd-crosshair">' +
                    '<div class="tsd-crosshair-h"></div>' +
                    '<div class="tsd-crosshair-v"></div>' +
                    '<div class="tsd-crosshair-center"></div>' +
                '</div>',
                iconSize: [30, 30],
                iconAnchor: [15, 15]
            }),
            interactive: false,
            pane: 'tsdPane'
        }).addTo(state.map);
    }

    // ─── SWEEP ANIMATION ───
    function startSweep() {
        if (!state.center || !state.map) return;
        state.sweepEnabled = true;
        state.lastSweepTime = performance.now();
        animateSweep();
    }

    function stopSweep() {
        state.sweepEnabled = false;
        if (state.sweepAnimId) {
            cancelAnimationFrame(state.sweepAnimId);
            state.sweepAnimId = null;
        }
        if (state.layers.sweep) {
            state.map.removeLayer(state.layers.sweep);
            state.layers.sweep = null;
        }
    }

    function animateSweep() {
        if (!state.sweepEnabled || !state.enabled) return;

        var now = performance.now();
        var delta = now - state.lastSweepTime;
        state.lastSweepTime = now;

        // Update angle
        state.sweepAngle = (state.sweepAngle + (delta / CONFIG.sweepDuration) * 360) % 360;

        // Draw sweep wedge
        drawSweepWedge();

        state.sweepAnimId = requestAnimationFrame(animateSweep);
    }

    function drawSweepWedge() {
        if (!state.center || !state.map) return;

        // Remove old sweep
        if (state.layers.sweep) {
            state.map.removeLayer(state.layers.sweep);
        }

        var maxRange = CONFIG.rangeRings[CONFIG.rangeRings.length - 1];
        var centerLatLng = [state.center.lat, state.center.lng];

        // Create sweep line (single line extending from center)
        var radians = state.sweepAngle * Math.PI / 180;
        var endLat = state.center.lat + (maxRange / 111320) * Math.cos(radians);
        var endLng = state.center.lng + (maxRange / (111320 * Math.cos(state.center.lat * Math.PI / 180))) * Math.sin(radians);

        // Draw the main sweep line
        state.layers.sweep = L.polyline([centerLatLng, [endLat, endLng]], {
            color: CONFIG.colors.sweepLine,
            weight: 2,
            interactive: false,
            pane: 'tsdPane',
            className: 'tsd-sweep-line'
        }).addTo(state.map);

        // Add a trailing gradient effect using multiple lines
        for (var i = 1; i <= 15; i++) {
            var trailAngle = (state.sweepAngle - i * 2) * Math.PI / 180;
            var trailEndLat = state.center.lat + (maxRange / 111320) * Math.cos(trailAngle);
            var trailEndLng = state.center.lng + (maxRange / (111320 * Math.cos(state.center.lat * Math.PI / 180))) * Math.sin(trailAngle);
            var opacity = (15 - i) / 15 * 0.3;

            var trailLine = L.polyline([centerLatLng, [trailEndLat, trailEndLng]], {
                color: CONFIG.colors.sweep,
                weight: 1,
                opacity: opacity,
                interactive: false,
                pane: 'tsdPane'
            }).addTo(state.map);

            // Store in sweep layer to be cleared
            if (!state.layers.sweepTrail) state.layers.sweepTrail = [];
            state.layers.sweepTrail.push(trailLine);
        }

        // Clear old trail lines (keep last 15)
        while (state.layers.sweepTrail && state.layers.sweepTrail.length > 15) {
            var oldLine = state.layers.sweepTrail.shift();
            state.map.removeLayer(oldLine);
        }
    }

    // ─── UPDATE UAV POSITIONS & DRAW VECTORS ───
    function updateUAVs(uavs) {
        state.uavs = uavs || [];

        if (state.enabled) {
            drawThreatVectors();
        }
    }

    function drawThreatVectors() {
        // Clear old vectors
        state.layers.vectors.forEach(function(v) {
            state.map.removeLayer(v);
        });
        state.layers.vectors = [];

        if (!state.center || !state.map) return;

        state.uavs.forEach(function(uav) {
            if (!uav.latitude || !uav.longitude) return;
            if (uav.speed == null || uav.speed < 0.5) return;  // Skip stationary
            if (uav.ground_track == null) return;

            var pos = [uav.latitude, uav.longitude];
            var heading = uav.ground_track;
            var speed = uav.speed;  // m/s
            var trust = uav.trust_score != null ? uav.trust_score : 100;

            // Only show vectors for potential threats
            if (trust >= 80 && (uav.classification || '').toUpperCase() === 'FRIENDLY') return;

            CONFIG.vectorTimes.forEach(function(seconds, idx) {
                var distance = speed * seconds;
                var radians = heading * Math.PI / 180;

                var endLat = uav.latitude + (distance / 111320) * Math.cos(radians);
                var endLng = uav.longitude + (distance / (111320 * Math.cos(uav.latitude * Math.PI / 180))) * Math.sin(radians);

                var color = idx === 0 ? CONFIG.colors.vector30s : CONFIG.colors.vector60s;
                var weight = idx === 0 ? 3 : 2;

                var vector = L.polyline([pos, [endLat, endLng]], {
                    color: color,
                    weight: weight,
                    dashArray: idx === 0 ? '6 4' : '4 6',
                    interactive: false,
                    pane: 'tsdPane'
                }).addTo(state.map);

                // Add predicted position marker
                var markerHtml = '<div class="tsd-predicted-pos" style="border-color:' + color + '">' +
                    '<span class="tsd-predicted-time">' + seconds + 's</span>' +
                '</div>';

                var marker = L.marker([endLat, endLng], {
                    icon: L.divIcon({
                        className: 'tsd-predicted-marker',
                        html: markerHtml,
                        iconSize: [24, 24],
                        iconAnchor: [12, 12]
                    }),
                    interactive: false,
                    pane: 'tsdPane'
                }).addTo(state.map);

                state.layers.vectors.push(vector);
                state.layers.vectors.push(marker);
            });
        });
    }

    // ─── CLEAR ALL LAYERS ───
    function clearAllLayers() {
        if (!state.map) return;

        state.layers.rings.forEach(function(l) { state.map.removeLayer(l); });
        state.layers.bearings.forEach(function(l) { state.map.removeLayer(l); });
        state.layers.vectors.forEach(function(l) { state.map.removeLayer(l); });

        if (state.layers.sweep) {
            state.map.removeLayer(state.layers.sweep);
        }
        if (state.layers.sweepTrail) {
            state.layers.sweepTrail.forEach(function(l) { state.map.removeLayer(l); });
        }
        if (state.layers.centerMarker) {
            state.map.removeLayer(state.layers.centerMarker);
        }

        state.layers = {
            rings: [],
            bearings: [],
            sweep: null,
            sweepTrail: [],
            vectors: [],
            centerMarker: null
        };
    }

    // ─── HELPERS ───
    function formatDistance(meters) {
        if (meters >= 1000) {
            return (meters / 1000).toFixed(1) + 'km';
        }
        return meters + 'm';
    }

    // ─── CONFIGURE ───
    function configure(options) {
        if (options.rangeRings) CONFIG.rangeRings = options.rangeRings;
        if (options.sweepDuration) CONFIG.sweepDuration = options.sweepDuration;
        if (options.vectorTimes) CONFIG.vectorTimes = options.vectorTimes;
        if (options.colors) Object.assign(CONFIG.colors, options.colors);
    }

    // ─── GET STATE ───
    function isEnabled() {
        return state.enabled;
    }

    function getCenter() {
        return state.center;
    }

    // ─── PUBLIC API ───
    return {
        init: init,
        enable: enable,
        disable: disable,
        toggle: toggle,
        setCenter: setCenter,
        updateUAVs: updateUAVs,
        configure: configure,
        isEnabled: isEnabled,
        getCenter: getCenter,
        startSweep: startSweep,
        stopSweep: stopSweep,
        CONFIG: CONFIG
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = TacticalDisplay;
}
