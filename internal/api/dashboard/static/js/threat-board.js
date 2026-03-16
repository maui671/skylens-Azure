/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS THREAT BOARD v1.0
   Split panel: Known Fleet (left, green) vs Unknown/Hostile (right, red)
   Central threat level indicator with real-time updates
   ═══════════════════════════════════════════════════════════════════════════ */

var ThreatBoard = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        animationDuration: 300,
        pulseInterval: 2000,
        maxItemsPerPanel: 20,
        threatColors: {
            CRITICAL: '#FF1744',
            HIGH: '#FF6D00',
            MODERATE: '#FFB300',
            LOW: '#00E676'
        },
        classificationColors: {
            FRIENDLY: '#4CAF50',
            NEUTRAL: '#2196F3',
            SUSPECT: '#FF9800',
            UNKNOWN: '#9E9E9E',
            HOSTILE: '#FF1744'
        }
    };

    // ─── STATE ───
    var state = {
        mounted: false,
        container: null,
        threatLevel: 'LOW',
        knownFleet: [],
        unknownHostile: [],
        lastUpdate: null,
        animationFrame: null
    };

    // ─── THREAT LEVEL CALCULATION ───
    function calculateThreatLevel(uavs) {
        if (!uavs || uavs.length === 0) return 'LOW';

        var hostile = 0, unknown = 0, spoofing = 0, lowTrust = 0;

        uavs.forEach(function(u) {
            var classification = (u.classification || 'UNKNOWN').toUpperCase();
            var trust = u.trust_score != null ? u.trust_score : 100;
            var flags = u.spoof_flags || [];

            if (classification === 'HOSTILE') hostile++;
            if (classification === 'UNKNOWN') unknown++;
            if (flags.length > 0) spoofing++;
            if (trust < 50) lowTrust++;
        });

        if (hostile > 0 || spoofing > 2 || lowTrust > 3) return 'CRITICAL';
        if (spoofing > 0 || lowTrust > 1) return 'HIGH';
        if (unknown > 2 || lowTrust > 0) return 'MODERATE';
        return 'LOW';
    }

    // ─── CLASSIFY UAVs INTO PANELS ───
    function classifyUAVs(uavs) {
        var known = [], unknownHostile = [];

        (uavs || []).forEach(function(u) {
            var classification = (u.classification || 'UNKNOWN').toUpperCase();
            var trust = u.trust_score != null ? u.trust_score : 100;
            var flags = u.spoof_flags || [];
            var isActive = u._contactStatus !== 'lost';

            var item = {
                id: u.identifier || u.mac,
                name: u.designation || u.identifier || 'Unknown',
                classification: classification,
                trust: trust,
                flags: flags,
                altitude: u.altitude_geodetic,
                speed: u.speed,
                rssi: u.rssi,
                isActive: isActive,
                timestamp: u.timestamp || u.last_seen,
                raw: u
            };

            // Known Fleet: FRIENDLY or high-trust NEUTRAL
            if (classification === 'FRIENDLY' || (classification === 'NEUTRAL' && trust >= 80)) {
                known.push(item);
            } else {
                unknownHostile.push(item);
            }
        });

        // Sort by threat priority (worst first)
        unknownHostile.sort(function(a, b) {
            // Hostile > Unknown > NEUTRAL with low trust
            var order = { HOSTILE: 0, UNKNOWN: 1, NEUTRAL: 2 };
            var aOrder = order[a.classification] !== undefined ? order[a.classification] : 3;
            var bOrder = order[b.classification] !== undefined ? order[b.classification] : 3;
            if (aOrder !== bOrder) return aOrder - bOrder;
            return a.trust - b.trust; // Lower trust first
        });

        // Sort known by name
        known.sort(function(a, b) {
            return (a.name || '').localeCompare(b.name || '');
        });

        return { known: known, unknownHostile: unknownHostile };
    }

    // ─── HTML ESCAPE ───
    var _escDiv = document.createElement('div');
    function esc(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    // ─── TIME AGO ───
    function timeAgo(ts) {
        if (!ts) return '--';
        var diff = (Date.now() - new Date(ts).getTime()) / 1000;
        if (diff < 0) diff = 0;
        if (diff < 60) return Math.floor(diff) + 's';
        if (diff < 3600) return Math.floor(diff / 60) + 'm';
        if (diff < 86400) return Math.floor(diff / 3600) + 'h' + Math.floor((diff % 3600) / 60) + 'm';
        var days = Math.floor(diff / 86400);
        var hours = Math.floor((diff % 86400) / 3600);
        return days + 'd' + hours + 'h';
    }

    // ─── RENDER THREAT INDICATOR ───
    function renderThreatIndicator(level) {
        var color = CONFIG.threatColors[level] || CONFIG.threatColors.LOW;
        var pulseClass = level === 'CRITICAL' ? 'tb-pulse-critical' :
                         level === 'HIGH' ? 'tb-pulse-high' : '';

        return '<div class="tb-center">' +
            '<div class="tb-threat-indicator ' + pulseClass + '">' +
                '<svg viewBox="0 0 100 100" class="tb-threat-svg">' +
                    // Background ring
                    '<circle cx="50" cy="50" r="45" fill="none" stroke="rgba(255,255,255,0.1)" stroke-width="6"/>' +
                    // Colored arc based on threat level
                    '<circle cx="50" cy="50" r="45" fill="none" stroke="' + color + '" stroke-width="6" ' +
                        'stroke-dasharray="' + getThreatArc(level) + ' 283" stroke-linecap="round" ' +
                        'transform="rotate(-90 50 50)" class="tb-threat-arc"/>' +
                    // Inner glow
                    '<circle cx="50" cy="50" r="35" fill="rgba(0,0,0,0.3)"/>' +
                    '<circle cx="50" cy="50" r="35" fill="none" stroke="' + color + '" stroke-width="2" opacity="0.5"/>' +
                '</svg>' +
                '<div class="tb-threat-text">' +
                    '<div class="tb-threat-level" style="color:' + color + '">' + level + '</div>' +
                    '<div class="tb-threat-label">THREAT</div>' +
                '</div>' +
            '</div>' +
        '</div>';
    }

    function getThreatArc(level) {
        switch(level) {
            case 'CRITICAL': return 283;  // Full
            case 'HIGH': return 212;      // 75%
            case 'MODERATE': return 141;  // 50%
            default: return 71;           // 25%
        }
    }

    // ─── RENDER UAV ITEM ───
    function renderUAVItem(item, isHostile) {
        var trustColor = item.trust >= 80 ? '#00E676' : item.trust >= 50 ? '#FFB300' : '#FF1744';
        var classColor = CONFIG.classificationColors[item.classification] || '#9E9E9E';
        var statusClass = item.isActive ? 'active' : 'lost';

        var flagsHtml = '';
        if (item.flags && item.flags.length > 0) {
            flagsHtml = '<div class="tb-item-flags">';
            item.flags.slice(0, 3).forEach(function(f) {
                flagsHtml += '<span class="tb-flag">' + esc(f) + '</span>';
            });
            if (item.flags.length > 3) {
                flagsHtml += '<span class="tb-flag">+' + (item.flags.length - 3) + '</span>';
            }
            flagsHtml += '</div>';
        }

        return '<div class="tb-item ' + (isHostile ? 'hostile' : 'known') + ' ' + statusClass + '" data-id="' + esc(item.id) + '">' +
            '<div class="tb-item-header">' +
                '<span class="tb-item-status ' + statusClass + '"></span>' +
                '<span class="tb-item-name">' + esc(item.name) + '</span>' +
                '<span class="tb-item-class" style="color:' + classColor + '">' + item.classification + '</span>' +
            '</div>' +
            '<div class="tb-item-stats">' +
                '<span class="tb-stat"><span class="tb-stat-label">TRUST</span><span class="tb-stat-val" style="color:' + trustColor + '">' + item.trust + '%</span></span>' +
                '<span class="tb-stat"><span class="tb-stat-label">ALT</span><span class="tb-stat-val">' + (item.altitude != null ? Math.round(item.altitude) + 'm' : '--') + '</span></span>' +
                '<span class="tb-stat"><span class="tb-stat-label">SPD</span><span class="tb-stat-val">' + (item.speed != null ? item.speed.toFixed(1) : '--') + '</span></span>' +
                '<span class="tb-stat tb-stat-time">' + timeAgo(item.timestamp) + '</span>' +
            '</div>' +
            flagsHtml +
        '</div>';
    }

    // ─── RENDER PANEL ───
    function renderPanel(items, isHostile, title, icon) {
        var color = isHostile ? '#FF1744' : '#4CAF50';
        var bgColor = isHostile ? 'rgba(255,23,68,0.05)' : 'rgba(76,175,80,0.05)';
        var borderColor = isHostile ? 'rgba(255,23,68,0.3)' : 'rgba(76,175,80,0.3)';

        var itemsHtml = '';
        if (items.length === 0) {
            itemsHtml = '<div class="tb-empty">' +
                (isHostile ? 'No threats detected' : 'No known fleet') +
            '</div>';
        } else {
            items.slice(0, CONFIG.maxItemsPerPanel).forEach(function(item) {
                itemsHtml += renderUAVItem(item, isHostile);
            });
            if (items.length > CONFIG.maxItemsPerPanel) {
                itemsHtml += '<div class="tb-more">+' + (items.length - CONFIG.maxItemsPerPanel) + ' more</div>';
            }
        }

        return '<div class="tb-panel ' + (isHostile ? 'hostile' : 'known') + '" style="background:' + bgColor + ';border-color:' + borderColor + '">' +
            '<div class="tb-panel-header" style="border-color:' + borderColor + '">' +
                '<span class="tb-panel-icon" style="color:' + color + '">' + icon + '</span>' +
                '<span class="tb-panel-title">' + title + '</span>' +
                '<span class="tb-panel-count" style="background:' + color + '">' + items.length + '</span>' +
            '</div>' +
            '<div class="tb-panel-body">' + itemsHtml + '</div>' +
        '</div>';
    }

    // ─── RENDER FULL BOARD ───
    function render() {
        if (!state.container) return;

        var html = '<div class="threat-board">' +
            renderPanel(state.knownFleet, false, 'KNOWN FLEET', '&#9632;') +
            renderThreatIndicator(state.threatLevel) +
            renderPanel(state.unknownHostile, true, 'UNKNOWN / HOSTILE', '&#9888;') +
        '</div>';

        state.container.innerHTML = html;

        // Add click handlers
        var items = state.container.querySelectorAll('.tb-item');
        items.forEach(function(item) {
            item.addEventListener('click', function() {
                var id = this.getAttribute('data-id');
                if (id && typeof App !== 'undefined' && App.selectDrone) {
                    App.selectDrone(id);
                }
            });
        });
    }

    // ─── UPDATE DATA ───
    function update(uavs) {
        var classified = classifyUAVs(uavs);
        state.knownFleet = classified.known;
        state.unknownHostile = classified.unknownHostile;
        state.threatLevel = calculateThreatLevel(uavs);
        state.lastUpdate = Date.now();

        if (state.mounted) {
            render();
        }
    }

    // ─── MOUNT ───
    function mount(containerId) {
        var container = document.getElementById(containerId);
        if (!container) {
            console.error('ThreatBoard: Container not found:', containerId);
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

    // ─── GET STATE ───
    function getState() {
        return {
            threatLevel: state.threatLevel,
            knownCount: state.knownFleet.length,
            unknownHostileCount: state.unknownHostile.length,
            lastUpdate: state.lastUpdate
        };
    }

    // ─── PUBLIC API ───
    return {
        mount: mount,
        unmount: unmount,
        update: update,
        getState: getState,
        calculateThreatLevel: calculateThreatLevel
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = ThreatBoard;
}
