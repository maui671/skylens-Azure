/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS TRUST GAUGE + SPOOF ALERT PANEL v1.0
   Radial gauge with threat ring, spoof evidence breakdown with action buttons
   ═══════════════════════════════════════════════════════════════════════════ */

var TrustGauge = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        gaugeRadius: 60,
        gaugeStroke: 8,
        animationDuration: 500,
        colors: {
            safe: '#00E676',
            caution: '#FFB300',
            warning: '#FF6D00',
            danger: '#FF1744',
            background: 'rgba(255,255,255,0.1)',
            spoofIndicator: '#FF1744'
        },
        thresholds: {
            safe: 80,
            caution: 50,
            warning: 30
        },
        spoofFlags: {
            'IMPOSSIBLE_SPEED': { severity: 'critical', description: 'Teleportation detected - impossible speed change' },
            'INCONSISTENT_ALTITUDE': { severity: 'high', description: 'Altitude jumps without matching vertical speed' },
            'SERIAL_CLONE': { severity: 'critical', description: 'Same serial seen from multiple locations' },
            'TIMESTAMP_DRIFT': { severity: 'medium', description: 'Message timestamps inconsistent with real time' },
            'MAC_MISMATCH': { severity: 'high', description: 'MAC address inconsistent with claimed identity' },
            'GEOMETRY_VIOLATION': { severity: 'high', description: 'Impossible flight geometry detected' },
            'NO_RID': { severity: 'medium', description: 'No Remote ID broadcast present' },
            'REGISTRATION_MISSING': { severity: 'low', description: 'Registration info not provided' }
        }
    };

    // ─── STATE ───
    var instances = {};

    // ─── HTML ESCAPE ───
    var _escDiv = document.createElement('div');
    function esc(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    // ─── GET COLOR FOR TRUST VALUE ───
    function getTrustColor(trust) {
        if (trust >= CONFIG.thresholds.safe) return CONFIG.colors.safe;
        if (trust >= CONFIG.thresholds.caution) return CONFIG.colors.caution;
        if (trust >= CONFIG.thresholds.warning) return CONFIG.colors.warning;
        return CONFIG.colors.danger;
    }

    // ─── GET TRUST LABEL ───
    function getTrustLabel(trust) {
        if (trust >= CONFIG.thresholds.safe) return 'COMPLIANT';
        if (trust >= CONFIG.thresholds.caution) return 'CAUTION';
        if (trust >= CONFIG.thresholds.warning) return 'SUSPECT';
        return 'FLAGGED';
    }

    // ─── CREATE SVG GAUGE ───
    function createGaugeSVG(trust, spoofFlags) {
        var r = CONFIG.gaugeRadius;
        var stroke = CONFIG.gaugeStroke;
        var size = (r + stroke) * 2 + 10;
        var cx = size / 2;
        var cy = size / 2;
        var circumference = 2 * Math.PI * r;
        var progress = Math.max(0, Math.min(100, trust)) / 100;
        var offset = circumference * (1 - progress);
        var color = getTrustColor(trust);

        // Determine if we should show spoof ring
        var hasSpoofFlags = spoofFlags && spoofFlags.length > 0;
        var spoofRingRadius = r + stroke + 4;

        var svg = '<svg viewBox="0 0 ' + size + ' ' + size + '" class="tg-svg">';

        // Outer spoof indicator ring (if flags present)
        if (hasSpoofFlags) {
            svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + spoofRingRadius + '" ' +
                'fill="none" stroke="' + CONFIG.colors.spoofIndicator + '" stroke-width="3" ' +
                'stroke-dasharray="8 4" class="tg-spoof-ring"/>';
        }

        // Background ring
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" ' +
            'fill="none" stroke="' + CONFIG.colors.background + '" stroke-width="' + stroke + '"/>';

        // Progress ring
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" ' +
            'fill="none" stroke="' + color + '" stroke-width="' + stroke + '" ' +
            'stroke-dasharray="' + circumference + '" stroke-dashoffset="' + offset + '" ' +
            'stroke-linecap="round" transform="rotate(-90 ' + cx + ' ' + cy + ')" ' +
            'class="tg-progress"/>';

        // Inner gradient fill
        svg += '<defs>' +
            '<radialGradient id="tg-inner-grad" cx="50%" cy="50%" r="50%">' +
                '<stop offset="0%" stop-color="' + color + '" stop-opacity="0.2"/>' +
                '<stop offset="100%" stop-color="' + color + '" stop-opacity="0.05"/>' +
            '</radialGradient>' +
        '</defs>';
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="' + (r - stroke/2 - 5) + '" ' +
            'fill="url(#tg-inner-grad)"/>';

        // Center text
        svg += '<text x="' + cx + '" y="' + (cy - 5) + '" class="tg-value" fill="' + color + '">' + Math.round(trust) + '</text>';
        svg += '<text x="' + cx + '" y="' + (cy + 14) + '" class="tg-label">' + getTrustLabel(trust) + '</text>';

        svg += '</svg>';
        return svg;
    }

    // ─── RENDER SPOOF EVIDENCE PANEL ───
    function renderSpoofPanel(flags, uavId) {
        if (!flags || flags.length === 0) {
            return '<div class="tg-spoof-panel tg-spoof-clear">' +
                '<div class="tg-spoof-header">' +
                    '<span class="tg-spoof-icon safe">&#10003;</span>' +
                    '<span class="tg-spoof-title">No Anomalies Detected</span>' +
                '</div>' +
                '<div class="tg-spoof-body">' +
                    '<p class="tg-spoof-msg">All broadcast data appears consistent and valid.</p>' +
                '</div>' +
            '</div>';
        }

        var evidenceHtml = '';
        flags.forEach(function(flag) {
            var flagInfo = CONFIG.spoofFlags[flag] || { severity: 'medium', description: flag };
            var severityClass = flagInfo.severity;
            var severityLabel = flagInfo.severity.toUpperCase();

            evidenceHtml += '<div class="tg-evidence-item ' + severityClass + '">' +
                '<div class="tg-evidence-header">' +
                    '<span class="tg-evidence-severity ' + severityClass + '">' + severityLabel + '</span>' +
                    '<span class="tg-evidence-flag">' + esc(flag.replace(/_/g, ' ')) + '</span>' +
                '</div>' +
                '<p class="tg-evidence-desc">' + esc(flagInfo.description) + '</p>' +
            '</div>';
        });

        return '<div class="tg-spoof-panel tg-spoof-alert">' +
            '<div class="tg-spoof-header">' +
                '<span class="tg-spoof-icon danger">&#9888;</span>' +
                '<span class="tg-spoof-title">Spoof Evidence Detected</span>' +
                '<span class="tg-spoof-count">' + flags.length + ' flag' + (flags.length > 1 ? 's' : '') + '</span>' +
            '</div>' +
            '<div class="tg-spoof-body">' + evidenceHtml + '</div>' +
            '<div class="tg-spoof-actions">' +
                '<button class="tg-action-btn tg-action-investigate" data-action="investigate" data-uav="' + esc(uavId) + '">' +
                    '<span class="tg-action-icon">&#128269;</span> Investigate' +
                '</button>' +
                '<button class="tg-action-btn tg-action-tag" data-action="tag" data-uav="' + esc(uavId) + '">' +
                    '<span class="tg-action-icon">&#127991;</span> Tag Hostile' +
                '</button>' +
                '<button class="tg-action-btn tg-action-track" data-action="track" data-uav="' + esc(uavId) + '">' +
                    '<span class="tg-action-icon">&#128205;</span> Track' +
                '</button>' +
            '</div>' +
        '</div>';
    }

    // ─── RENDER MINI GAUGES (for list views) ───
    function renderMiniGauge(trust) {
        var color = getTrustColor(trust);
        var circumference = 2 * Math.PI * 12;
        var offset = circumference * (1 - trust / 100);

        return '<svg viewBox="0 0 30 30" class="tg-mini">' +
            '<circle cx="15" cy="15" r="12" fill="none" stroke="rgba(255,255,255,0.1)" stroke-width="2"/>' +
            '<circle cx="15" cy="15" r="12" fill="none" stroke="' + color + '" stroke-width="2" ' +
                'stroke-dasharray="' + circumference + '" stroke-dashoffset="' + offset + '" ' +
                'stroke-linecap="round" transform="rotate(-90 15 15)"/>' +
            '<text x="15" y="18" class="tg-mini-text">' + Math.round(trust) + '</text>' +
        '</svg>';
    }

    // ─── RENDER FULL COMPONENT ───
    function render(containerId, uav) {
        var container = document.getElementById(containerId);
        if (!container) return;

        var trust = uav.trust_score != null ? uav.trust_score : 100;
        var flags = uav.spoof_flags || [];
        var uavId = uav.identifier || uav.mac || 'unknown';

        var html = '<div class="trust-gauge-component">' +
            '<div class="tg-gauge-container">' +
                createGaugeSVG(trust, flags) +
            '</div>' +
            renderSpoofPanel(flags, uavId) +
        '</div>';

        container.innerHTML = html;

        // Bind action button events
        container.querySelectorAll('.tg-action-btn').forEach(function(btn) {
            btn.addEventListener('click', function(e) {
                e.stopPropagation();
                var action = this.getAttribute('data-action');
                var targetUav = this.getAttribute('data-uav');
                handleAction(action, targetUav);
            });
        });

        instances[containerId] = { uav: uav, container: container };
    }

    // ─── HANDLE ACTIONS ───
    function handleAction(action, uavId) {
        switch(action) {
            case 'investigate':
                // Open history/detail view
                if (typeof App !== 'undefined' && App.showHistory) {
                    App.showHistory(uavId);
                }
                break;
            case 'tag':
                // Tag as hostile
                if (typeof App !== 'undefined' && App.showTagPopup) {
                    App.showTagPopup(null, uavId);
                } else {
                    // Direct API call
                    fetch('/api/uav/' + encodeURIComponent(uavId) + '/classify', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ classification: 'HOSTILE' })
                    }).then(function() {
                        if (typeof App !== 'undefined' && App.poll) App.poll();
                    });
                }
                break;
            case 'track':
                // Center map and enable tracking
                if (typeof App !== 'undefined' && App.locateDrone) {
                    App.locateDrone(uavId);
                }
                if (typeof NzMap !== 'undefined' && NzMap.setAutoFollow) {
                    NzMap.setAutoFollow(true);
                }
                break;
        }
    }

    // ─── UPDATE EXISTING INSTANCE ───
    function update(containerId, uav) {
        render(containerId, uav);
    }

    // ─── DESTROY INSTANCE ───
    function destroy(containerId) {
        if (instances[containerId]) {
            instances[containerId].container.innerHTML = '';
            delete instances[containerId];
        }
    }

    // ─── STATIC METHODS FOR EXTERNAL USE ───
    function getTrustColorStatic(trust) {
        return getTrustColor(trust);
    }

    function createMiniGaugeHTML(trust) {
        return renderMiniGauge(trust);
    }

    // ─── PUBLIC API ───
    return {
        render: render,
        update: update,
        destroy: destroy,
        getMiniGauge: createMiniGaugeHTML,
        getTrustColor: getTrustColorStatic,
        getTrustLabel: getTrustLabel,
        CONFIG: CONFIG
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = TrustGauge;
}
