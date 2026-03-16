/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS PIPELINE LATENCY MONITOR v1.0
   Waterfall showing tap -> NATS -> node -> dashboard latency
   ═══════════════════════════════════════════════════════════════════════════ */

var PipelineLatency = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        stages: [
            { id: 'tap', name: 'TAP', color: '#4CAF50', icon: '&#128225;' },
            { id: 'nats', name: 'NATS', color: '#00B0FF', icon: '&#9889;' },
            { id: 'node', name: 'NODE', color: '#AA00FF', icon: '&#9881;' },
            { id: 'api', name: 'API', color: '#FF6D00', icon: '&#128280;' },
            { id: 'dashboard', name: 'UI', color: '#FF4081', icon: '&#9638;' }
        ],
        thresholds: {
            good: 50,      // ms
            warning: 150,  // ms
            critical: 500  // ms
        },
        historyLength: 60,  // samples
        updateInterval: 1000
    };

    // ─── STATE ───
    var state = {
        container: null,
        mounted: false,
        latencies: {},  // stage -> current latency
        history: {},    // stage -> [latency history]
        lastUpdate: null,
        totalLatency: 0
    };

    // ─── INITIALIZE HISTORY ───
    CONFIG.stages.forEach(function(s) {
        state.history[s.id] = [];
        state.latencies[s.id] = 0;
    });

    // ─── HTML ESCAPE ───
    var _escDiv = document.createElement('div');
    function esc(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    // ─── GET STATUS COLOR ───
    function getStatusColor(latency) {
        if (latency <= CONFIG.thresholds.good) return '#00E676';
        if (latency <= CONFIG.thresholds.warning) return '#FFB300';
        return '#FF1744';
    }

    // ─── GET STATUS LABEL ───
    function getStatusLabel(latency) {
        if (latency <= CONFIG.thresholds.good) return 'OPTIMAL';
        if (latency <= CONFIG.thresholds.warning) return 'NORMAL';
        if (latency <= CONFIG.thresholds.critical) return 'SLOW';
        return 'CRITICAL';
    }

    // ─── CREATE SPARKLINE ───
    function createSparkline(history, color) {
        if (!history || history.length < 2) {
            return '<div class="pl-sparkline-empty">--</div>';
        }

        var width = 80;
        var height = 20;
        var max = Math.max.apply(null, history) || 1;
        var min = 0;
        var range = max - min || 1;

        var points = history.map(function(val, i) {
            var x = (i / (history.length - 1)) * width;
            var y = height - ((val - min) / range) * (height - 2) - 1;
            return x + ',' + y;
        }).join(' ');

        return '<svg viewBox="0 0 ' + width + ' ' + height + '" class="pl-sparkline">' +
            '<polyline points="' + points + '" fill="none" stroke="' + color + '" stroke-width="1.5" opacity="0.7"/>' +
        '</svg>';
    }

    // ─── RENDER STAGE ───
    function renderStage(stage, index) {
        var latency = state.latencies[stage.id];
        var history = state.history[stage.id] || [];
        var color = stage.color;
        var isNull = latency == null;
        var displayLatency = isNull ? 0 : latency;
        var statusColor = isNull ? 'rgba(255,255,255,0.3)' : getStatusColor(displayLatency);

        var barWidth = isNull ? 0 : Math.min((displayLatency / CONFIG.thresholds.critical) * 100, 100);

        return '<div class="pl-stage" style="--stage-color:' + color + '">' +
            '<div class="pl-stage-header">' +
                '<span class="pl-stage-icon">' + stage.icon + '</span>' +
                '<span class="pl-stage-name">' + stage.name + '</span>' +
            '</div>' +
            '<div class="pl-stage-bar">' +
                '<div class="pl-stage-fill" style="width:' + barWidth + '%;background:' + statusColor + '"></div>' +
            '</div>' +
            '<div class="pl-stage-footer">' +
                '<span class="pl-stage-latency" style="color:' + statusColor + '">' +
                    (isNull ? '--' : Math.round(displayLatency) + 'ms') +
                '</span>' +
                '<div class="pl-stage-spark">' + createSparkline(history, color) + '</div>' +
            '</div>' +
            (index < CONFIG.stages.length - 1 ?
                '<div class="pl-connector"><svg viewBox="0 0 20 10"><path d="M0 5 L15 5 L12 2 M15 5 L12 8" fill="none" stroke="' + color + '" stroke-width="1.5" opacity="0.5"/></svg></div>'
            : '') +
        '</div>';
    }

    // ─── RENDER TOTAL SUMMARY ───
    function renderSummary() {
        var isNull = state.totalLatency == null;
        var total = isNull ? 0 : state.totalLatency;
        var statusColor = isNull ? 'rgba(255,255,255,0.3)' : getStatusColor(total);
        var statusLabel = isNull ? 'NO DATA' : getStatusLabel(total);

        return '<div class="pl-summary">' +
            '<div class="pl-summary-gauge">' +
                '<svg viewBox="0 0 100 60" class="pl-gauge-svg">' +
                    '<path d="M10 55 A 45 45 0 0 1 90 55" fill="none" stroke="rgba(255,255,255,0.1)" stroke-width="8" stroke-linecap="round"/>' +
                    '<path d="M10 55 A 45 45 0 0 1 90 55" fill="none" stroke="' + statusColor + '" stroke-width="8" stroke-linecap="round" ' +
                        'stroke-dasharray="' + (isNull ? 0 : Math.min(total / CONFIG.thresholds.critical, 1) * 141) + ' 141" class="pl-gauge-arc"/>' +
                '</svg>' +
                '<div class="pl-summary-value">' +
                    '<span class="pl-total-latency" style="color:' + statusColor + '">' + (isNull ? '--' : Math.round(total)) + '</span>' +
                    '<span class="pl-total-unit">' + (isNull ? '' : 'ms') + '</span>' +
                '</div>' +
            '</div>' +
            '<div class="pl-summary-info">' +
                '<span class="pl-summary-label">END-TO-END LATENCY</span>' +
                '<span class="pl-summary-status" style="color:' + statusColor + '">' + statusLabel + '</span>' +
            '</div>' +
        '</div>';
    }

    // ─── RENDER FULL COMPONENT ───
    function render() {
        if (!state.container || !state.mounted) return;

        var stagesHtml = '<div class="pl-stages">';
        CONFIG.stages.forEach(function(stage, i) {
            stagesHtml += renderStage(stage, i);
        });
        stagesHtml += '</div>';

        var html = '<div class="pipeline-latency">' +
            '<div class="pl-header">' +
                '<span class="pl-title">PIPELINE LATENCY</span>' +
                '<span class="pl-updated">' + (state.lastUpdate ? new Date(state.lastUpdate).toLocaleTimeString() : '--') + '</span>' +
            '</div>' +
            stagesHtml +
            renderSummary() +
        '</div>';

        state.container.innerHTML = html;
    }

    // ─── UPDATE DATA ───
    function update(latencyData) {
        // latencyData format: { tap: 10, nats: 5, node: 20, api: 15, dashboard: 10 }
        if (!latencyData) {
            // No data available — set all to null (renders as "--")
            CONFIG.stages.forEach(function(stage) {
                state.latencies[stage.id] = null;
            });
            state.totalLatency = null;
            state.lastUpdate = Date.now();
            if (state.mounted) render();
            return;
        }

        var total = 0;
        CONFIG.stages.forEach(function(stage) {
            var lat = latencyData[stage.id] || 0;
            state.latencies[stage.id] = lat;
            total += lat;

            // Update history
            state.history[stage.id].push(lat);
            if (state.history[stage.id].length > CONFIG.historyLength) {
                state.history[stage.id].shift();
            }
        });

        state.totalLatency = total;
        state.lastUpdate = Date.now();

        if (state.mounted) {
            render();
        }
    }

    // ─── MOUNT ───
    function mount(containerId) {
        var container = document.getElementById(containerId);
        if (!container) {
            console.error('PipelineLatency: Container not found:', containerId);
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

    // ─── GET CURRENT STATE ───
    function getState() {
        return {
            latencies: state.latencies,
            totalLatency: state.totalLatency,
            lastUpdate: state.lastUpdate
        };
    }

    // ─── PUBLIC API ───
    return {
        mount: mount,
        unmount: unmount,
        update: update,
        getState: getState,
        CONFIG: CONFIG
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = PipelineLatency;
}
