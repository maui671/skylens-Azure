/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS TAP HEALTH MATRIX v1.0
   Card grid with CPU/MEM/TEMP gauges per tap, latency monitoring
   ═══════════════════════════════════════════════════════════════════════════ */

var TapHealthMatrix = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        refreshInterval: 1000,
        thresholds: {
            cpu: { warning: 70, critical: 85 },
            memory: { warning: 70, critical: 85 },
            temp: { warning: 70, critical: 80 },
            stale: { warning: 30, critical: 60 }  // seconds
        },
        gaugeSize: 50,
        maxLatencyHistory: 60  // samples for sparkline
    };

    // ─── STATE ───
    var state = {
        container: null,
        taps: [],
        latencyHistory: {},  // tap_uuid -> [latency values]
        lastUpdate: null,
        mounted: false
    };

    // ─── HTML ESCAPE ───
    var _escDiv = document.createElement('div');
    function esc(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    // ─── TAP AGE CALCULATION ───
    function tapAge(ts) {
        if (!ts) return 9999;
        try {
            return Math.floor((Date.now() - new Date(ts).getTime()) / 1000);
        } catch(e) { return 9999; }
    }

    // ─── GET STATUS CLASS ───
    function getStatusClass(age) {
        if (age < CONFIG.thresholds.stale.warning) return 'online';
        if (age < CONFIG.thresholds.stale.critical) return 'stale';
        return 'offline';
    }

    // ─── GET GAUGE COLOR ───
    function getGaugeColor(value, type) {
        var thresholds = CONFIG.thresholds[type];
        if (!thresholds) return '#00E676';
        if (value >= thresholds.critical) return '#FF1744';
        if (value >= thresholds.warning) return '#FFB300';
        return '#00E676';
    }

    // ─── CREATE CIRCULAR GAUGE ───
    function createCircularGauge(value, max, type, label, unit) {
        var size = CONFIG.gaugeSize;
        var r = (size - 8) / 2;
        var cx = size / 2;
        var cy = size / 2;
        var circumference = 2 * Math.PI * r;
        var normalizedValue = Math.max(0, Math.min(value, max));
        var progress = normalizedValue / max;
        var offset = circumference * (1 - progress);
        var color = getGaugeColor(normalizedValue, type);

        return '<div class="thm-gauge">' +
            '<svg viewBox="0 0 ' + size + ' ' + size + '">' +
                '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" ' +
                    'fill="none" stroke="rgba(255,255,255,0.08)" stroke-width="4"/>' +
                '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" ' +
                    'fill="none" stroke="' + color + '" stroke-width="4" ' +
                    'stroke-dasharray="' + circumference + '" stroke-dashoffset="' + offset + '" ' +
                    'stroke-linecap="round" transform="rotate(-90 ' + cx + ' ' + cy + ')"/>' +
                '<text x="' + cx + '" y="' + (cy + 3) + '" class="thm-gauge-value" fill="' + color + '">' +
                    (value != null ? Math.round(value) : '--') +
                '</text>' +
            '</svg>' +
            '<div class="thm-gauge-label">' + label + '</div>' +
            '<div class="thm-gauge-unit">' + (value != null ? unit : '') + '</div>' +
        '</div>';
    }

    // ─── CREATE TEMPERATURE ICON ───
    function createTempIcon(temp) {
        if (temp == null) return '';
        var color = getGaugeColor(temp, 'temp');
        var level = Math.min(Math.max((temp / 100) * 100, 10), 90);

        return '<svg viewBox="0 0 24 40" class="thm-temp-icon" style="color:' + color + '">' +
            '<rect x="8" y="4" width="8" height="24" rx="4" fill="none" stroke="currentColor" stroke-width="2"/>' +
            '<circle cx="12" cy="32" r="6" fill="none" stroke="currentColor" stroke-width="2"/>' +
            '<rect x="10" y="' + (28 - level * 0.24) + '" width="4" height="' + (level * 0.24) + '" fill="currentColor"/>' +
            '<circle cx="12" cy="32" r="4" fill="currentColor"/>' +
        '</svg>';
    }

    // ─── CREATE LATENCY SPARKLINE ───
    function createSparkline(history) {
        if (!history || history.length < 2) {
            return '<div class="thm-sparkline-empty">No data</div>';
        }

        var width = 100;
        var height = 24;
        var max = Math.max.apply(null, history) || 1;
        var min = Math.min.apply(null, history) || 0;
        var range = max - min || 1;

        var points = history.map(function(val, i) {
            var x = (i / (history.length - 1)) * width;
            var y = height - ((val - min) / range) * (height - 4) - 2;
            return x + ',' + y;
        }).join(' ');

        var latestVal = history[history.length - 1];
        var color = latestVal > 500 ? '#FF1744' : latestVal > 200 ? '#FFB300' : '#00E676';

        return '<svg viewBox="0 0 ' + width + ' ' + height + '" class="thm-sparkline">' +
            '<polyline points="' + points + '" fill="none" stroke="' + color + '" stroke-width="1.5"/>' +
            '<circle cx="' + (width - 0) + '" cy="' + (height - ((latestVal - min) / range) * (height - 4) - 2) + '" ' +
                'r="2" fill="' + color + '"/>' +
        '</svg>' +
        '<span class="thm-latency-val" style="color:' + color + '">' + Math.round(latestVal) + 'ms</span>';
    }

    // ─── FORMAT BYTES ───
    function formatBytes(bytes) {
        if (bytes == null) return '--';
        if (bytes < 1024) return bytes + ' B';
        if (bytes < 1048576) return (bytes / 1024).toFixed(0) + ' KB';
        if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + ' MB';
        return (bytes / 1073741824).toFixed(1) + ' GB';
    }

    // ─── FORMAT UPTIME ───
    function formatUptime(seconds) {
        if (seconds == null) return '--';
        if (seconds < 60) return Math.floor(seconds) + 's';
        if (seconds < 3600) return Math.floor(seconds / 60) + 'm ' + Math.floor(seconds % 60) + 's';
        var hours = Math.floor(seconds / 3600);
        var mins = Math.floor((seconds % 3600) / 60);
        if (hours >= 24) {
            var days = Math.floor(hours / 24);
            hours = hours % 24;
            return days + 'd ' + hours + 'h';
        }
        return hours + 'h ' + mins + 'm';
    }

    // ─── RENDER TAP CARD ───
    function renderTapCard(tap) {
        var age = tapAge(tap.timestamp);
        var statusClass = getStatusClass(age);
        var statusLabel = statusClass === 'online' ? 'ONLINE' :
                          statusClass === 'stale' ? 'STALE' : 'OFFLINE';

        var cpu = tap.cpu_percent;
        var mem = tap.memory_percent;
        var temp = tap.temperature;
        var name = tap.tap_name || tap.tap_uuid || 'Unknown';
        var uuid = tap.tap_uuid || '';
        var framesTotal = tap.frames_total || tap.packets_captured || 0;
        var pps = tap.packets_per_second || 0;
        var capturing = tap.capture_running && statusClass !== 'offline';
        var diskFree = tap.disk_free;
        var uptime = tap.tap_uptime;
        var channels = tap.channels ? tap.channels.join(', ') : (tap.current_channel || tap.channel || '--');

        // Get latency history
        var history = state.latencyHistory[uuid] || [];

        var html = '<div class="thm-card ' + statusClass + '" data-tap-id="' + esc(uuid) + '">' +
            // Header
            '<div class="thm-card-header">' +
                '<div class="thm-status-dot ' + statusClass + '"></div>' +
                '<div class="thm-card-title">' +
                    '<span class="thm-card-name">' + esc(name) + '</span>' +
                    '<span class="thm-card-uuid">' + esc(uuid.substring(0, 8)) + '...</span>' +
                '</div>' +
                '<div class="thm-status-badge ' + statusClass + '">' + statusLabel + '</div>' +
            '</div>' +

            // Gauges Row
            '<div class="thm-gauges-row">' +
                createCircularGauge(cpu, 100, 'cpu', 'CPU', '%') +
                createCircularGauge(mem, 100, 'memory', 'MEM', '%') +
                '<div class="thm-temp-display">' +
                    createTempIcon(temp) +
                    '<div class="thm-temp-value" style="color:' + getGaugeColor(temp || 0, 'temp') + '">' +
                        (temp != null ? temp.toFixed(1) + '\u00b0C' : '--') +
                    '</div>' +
                '</div>' +
            '</div>' +

            // Latency Sparkline
            '<div class="thm-latency-row">' +
                '<span class="thm-latency-label">LATENCY</span>' +
                '<div class="thm-sparkline-container">' + createSparkline(history) + '</div>' +
            '</div>' +

            // Stats Grid
            '<div class="thm-stats-grid">' +
                '<div class="thm-stat"><span class="thm-stat-label">Frames</span><span class="thm-stat-value">' + formatNumber(framesTotal) + '</span></div>' +
                '<div class="thm-stat"><span class="thm-stat-label">Rate</span><span class="thm-stat-value">' + pps.toFixed(1) + '/s</span></div>' +
                '<div class="thm-stat"><span class="thm-stat-label">Channels</span><span class="thm-stat-value">' + channels + '</span></div>' +
                '<div class="thm-stat"><span class="thm-stat-label">Disk Free</span><span class="thm-stat-value">' + formatBytes(diskFree) + '</span></div>' +
                '<div class="thm-stat"><span class="thm-stat-label">Uptime</span><span class="thm-stat-value">' + formatUptime(uptime) + '</span></div>' +
                '<div class="thm-stat">' +
                    '<span class="thm-stat-label">Capture</span>' +
                    '<span class="thm-stat-value ' + (capturing ? 'good' : 'bad') + '">' +
                        (statusClass === 'offline' ? 'OFFLINE' : (capturing ? 'ACTIVE' : 'STOPPED')) +
                    '</span>' +
                '</div>' +
            '</div>' +

            // Footer with location
            (tap.latitude != null && tap.longitude != null ?
                '<div class="thm-card-footer">' +
                    '<span class="thm-location">' +
                        '&#128205; ' + tap.latitude.toFixed(4) + ', ' + tap.longitude.toFixed(4) + ' | ' + MGRS.format(tap.latitude, tap.longitude, 3, true) +
                    '</span>' +
                    '<span class="thm-age">' + age + 's ago</span>' +
                '</div>' : '') +

            // Warnings
            (statusClass !== 'offline' && !capturing ?
                '<div class="thm-warning">&#9888; Capture not running</div>' : '') +
            (statusClass === 'online' && framesTotal === 0 ?
                '<div class="thm-warning">&#9888; No frames captured</div>' : '') +

        '</div>';

        return html;
    }

    // ─── FORMAT NUMBER ───
    function formatNumber(n) {
        if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
        if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
        return String(n);
    }

    // ─── RENDER OVERVIEW STRIP ───
    function renderOverviewStrip() {
        var online = 0, stale = 0, offline = 0;
        var totalFrames = 0, totalPPS = 0;
        var maxCPU = 0, maxMem = 0, maxTemp = 0;

        state.taps.forEach(function(tap) {
            var age = tapAge(tap.timestamp);
            if (age < CONFIG.thresholds.stale.warning) online++;
            else if (age < CONFIG.thresholds.stale.critical) stale++;
            else offline++;

            totalFrames += tap.frames_total || tap.packets_captured || 0;
            totalPPS += tap.packets_per_second || 0;
            if (tap.cpu_percent > maxCPU) maxCPU = tap.cpu_percent;
            if (tap.memory_percent > maxMem) maxMem = tap.memory_percent;
            if (tap.temperature > maxTemp) maxTemp = tap.temperature;
        });

        var totalTaps = state.taps.length;
        var healthScore = totalTaps > 0 ? Math.round((online / totalTaps) * 100) : 0;
        var healthColor = healthScore >= 80 ? '#00E676' : healthScore >= 50 ? '#FFB300' : '#FF1744';

        return '<div class="thm-overview">' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value" style="color:' + healthColor + '">' + healthScore + '%</span>' +
                '<span class="thm-overview-label">HEALTH</span>' +
            '</div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value online">' + online + '</span>' +
                '<span class="thm-overview-label">ONLINE</span>' +
            '</div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value stale">' + stale + '</span>' +
                '<span class="thm-overview-label">STALE</span>' +
            '</div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value offline">' + offline + '</span>' +
                '<span class="thm-overview-label">OFFLINE</span>' +
            '</div>' +
            '<div class="thm-overview-divider"></div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value">' + formatNumber(totalFrames) + '</span>' +
                '<span class="thm-overview-label">FRAMES</span>' +
            '</div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value">' + totalPPS.toFixed(0) + '/s</span>' +
                '<span class="thm-overview-label">RATE</span>' +
            '</div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value" style="color:' + getGaugeColor(maxCPU, 'cpu') + '">' + Math.round(maxCPU) + '%</span>' +
                '<span class="thm-overview-label">MAX CPU</span>' +
            '</div>' +
            '<div class="thm-overview-stat">' +
                '<span class="thm-overview-value" style="color:' + getGaugeColor(maxTemp, 'temp') + '">' + Math.round(maxTemp) + '\u00b0</span>' +
                '<span class="thm-overview-label">MAX TEMP</span>' +
            '</div>' +
        '</div>';
    }

    // ─── RENDER FULL MATRIX ───
    function render() {
        if (!state.container || !state.mounted) return;

        // Sort taps: online first, then by name
        var sortedTaps = state.taps.slice().sort(function(a, b) {
            var ageA = tapAge(a.timestamp);
            var ageB = tapAge(b.timestamp);
            var statusA = getStatusClass(ageA);
            var statusB = getStatusClass(ageB);
            var order = { online: 0, stale: 1, offline: 2 };
            if (order[statusA] !== order[statusB]) {
                return order[statusA] - order[statusB];
            }
            return (a.tap_name || '').localeCompare(b.tap_name || '');
        });

        var cardsHtml = '';
        if (sortedTaps.length === 0) {
            cardsHtml = '<div class="thm-empty">' +
                '<div class="thm-empty-icon">&#128225;</div>' +
                '<div class="thm-empty-title">No Sensors Connected</div>' +
                '<div class="thm-empty-desc">Waiting for TAP heartbeats...</div>' +
            '</div>';
        } else {
            sortedTaps.forEach(function(tap) {
                cardsHtml += renderTapCard(tap);
            });
        }

        var html = '<div class="tap-health-matrix">' +
            renderOverviewStrip() +
            '<div class="thm-grid">' + cardsHtml + '</div>' +
        '</div>';

        state.container.innerHTML = html;
    }

    // ─── UPDATE DATA ───
    function update(taps) {
        state.taps = taps || [];
        state.lastUpdate = Date.now();

        // Update latency history (simulated from heartbeat age for now)
        state.taps.forEach(function(tap) {
            var uuid = tap.tap_uuid;
            if (!uuid) return;

            if (!state.latencyHistory[uuid]) {
                state.latencyHistory[uuid] = [];
            }

            // Use age as latency proxy (or actual latency if available)
            var latency = tap.nats_latency_ms || tapAge(tap.timestamp) * 10;
            state.latencyHistory[uuid].push(latency);

            // Keep only recent history
            if (state.latencyHistory[uuid].length > CONFIG.maxLatencyHistory) {
                state.latencyHistory[uuid].shift();
            }
        });

        if (state.mounted) {
            render();
        }
    }

    // ─── MOUNT ───
    function mount(containerId) {
        var container = document.getElementById(containerId);
        if (!container) {
            console.error('TapHealthMatrix: Container not found:', containerId);
            return false;
        }

        state.container = container;
        state.mounted = true;
        render();
        return true;
    }

    // ─── UNMOUNT ───
    function unmount() {
        state.mounted = false;
        if (state.container) {
            state.container.innerHTML = '';
        }
        state.container = null;
    }

    // ─── PUBLIC API ───
    return {
        mount: mount,
        unmount: unmount,
        update: update,
        getState: function() {
            return {
                taps: state.taps.length,
                lastUpdate: state.lastUpdate
            };
        }
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = TapHealthMatrix;
}
