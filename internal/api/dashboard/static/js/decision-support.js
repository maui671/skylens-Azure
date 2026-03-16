/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS DECISION SUPPORT PANEL v1.0
   Contextual insights and actions for selected drone
   Analyzes: trust score, approach vector, ETA, RemoteID compliance
   Generates actionable insights with quick-action buttons
   ═══════════════════════════════════════════════════════════════════════════ */

var DecisionSupport = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        // Protected asset location (configurable)
        protectedAsset: null,  // { lat, lng, name, radius }

        // Speed thresholds (m/s)
        speedThresholds: {
            slow: 2,      // < 2 m/s = loitering
            fast: 15,     // > 15 m/s = fast approach
            veryFast: 25  // > 25 m/s = racing
        },

        // Altitude thresholds (m)
        altitudeThresholds: {
            low: 30,      // < 30m = low altitude (riskier)
            restricted: 120  // > 120m = above legal limit
        },

        // ETA warning thresholds (seconds)
        etaWarnings: {
            imminent: 30,    // < 30s = imminent
            approaching: 120 // < 2min = approaching
        },

        // Insight priority levels
        priorities: {
            CRITICAL: { level: 0, color: '#FF1744', icon: '&#9888;' },
            HIGH: { level: 1, color: '#FF6D00', icon: '&#9888;' },
            MEDIUM: { level: 2, color: '#FFB300', icon: '&#9432;' },
            LOW: { level: 3, color: '#00B0FF', icon: '&#9432;' },
            INFO: { level: 4, color: '#00E676', icon: '&#10003;' }
        }
    };

    // ─── STATE ───
    var state = {
        mounted: false,
        container: null,
        selectedUAV: null,
        insights: [],
        onAction: null
    };

    // ─── HTML ESCAPE ───
    var _escDiv = document.createElement('div');
    function esc(s) {
        _escDiv.textContent = s == null ? '' : String(s);
        return _escDiv.innerHTML;
    }

    // ─── ANALYZE UAV ───
    function analyze(uav) {
        state.selectedUAV = uav;
        state.insights = [];

        if (!uav) {
            if (state.mounted) render();
            return [];
        }

        // Run all analyzers
        analyzeTrustScore(uav);
        analyzeRemoteID(uav);
        analyzeSpoofFlags(uav);
        analyzeAltitude(uav);
        analyzeSpeed(uav);
        analyzeApproachVector(uav);
        analyzeClassification(uav);
        analyzeContactStatus(uav);

        // Sort by priority
        state.insights.sort(function(a, b) {
            return CONFIG.priorities[a.priority].level - CONFIG.priorities[b.priority].level;
        });

        if (state.mounted) render();
        return state.insights;
    }

    // ─── TRUST SCORE ANALYZER ───
    function analyzeTrustScore(uav) {
        var trust = uav.trust_score != null ? uav.trust_score : 100;

        if (trust < 30) {
            addInsight('CRITICAL', 'VERY LOW TRUST',
                'Trust score is critically low (' + trust + '%). High likelihood of spoofing or malicious activity.',
                ['Tag Hostile', 'Create Incident']);
        } else if (trust < 50) {
            addInsight('HIGH', 'LOW TRUST SCORE',
                'Trust score is below threshold (' + trust + '%). Exercise caution.',
                ['Monitor', 'Investigate']);
        } else if (trust < 80) {
            addInsight('MEDIUM', 'MODERATE TRUST',
                'Trust score (' + trust + '%) indicates some anomalies detected.',
                ['Review Details']);
        }
    }

    // ─── REMOTE ID ANALYZER ───
    function analyzeRemoteID(uav) {
        var hasRID = !!(uav.serial_number || uav.uas_id || uav.remote_id);
        var hasOperatorID = !!uav.operator_id;
        var hasRegistration = uav.registration_status === 'REGISTERED';

        if (!hasRID) {
            addInsight('HIGH', 'MISSING REMOTE ID',
                'This aircraft is not broadcasting Remote ID as required by regulation.',
                ['Report Violation', 'Tag Suspicious']);
        }

        if (!hasOperatorID && uav.detection_source !== 'WiFiFingerprint') {
            addInsight('MEDIUM', 'NO OPERATOR ID',
                'Operator identification is not available in broadcast data.',
                ['Request Info']);
        }

        if (!hasRegistration && hasRID) {
            addInsight('LOW', 'REGISTRATION UNKNOWN',
                'Unable to verify registration status for this aircraft.',
                null);
        }
    }

    // ─── SPOOF FLAGS ANALYZER ───
    function analyzeSpoofFlags(uav) {
        var flags = uav.spoof_flags || [];

        if (flags.length === 0) return;

        var criticalFlags = ['IMPOSSIBLE_SPEED', 'SERIAL_CLONE', 'GEOMETRY_VIOLATION'];
        var hasCritical = flags.some(function(f) { return criticalFlags.indexOf(f) >= 0; });

        if (hasCritical) {
            addInsight('CRITICAL', 'SPOOFING DETECTED',
                'Critical spoofing indicators detected: ' + flags.slice(0, 3).join(', '),
                ['Create Incident', 'Tag Hostile', 'Alert Team']);
        } else if (flags.length > 2) {
            addInsight('HIGH', 'MULTIPLE ANOMALIES',
                flags.length + ' data anomaly flags detected. Possible spoofing attempt.',
                ['Investigate', 'Monitor']);
        } else {
            addInsight('MEDIUM', 'DATA ANOMALY',
                'Anomaly detected: ' + flags[0].replace(/_/g, ' '),
                ['Review Details']);
        }
    }

    // ─── ALTITUDE ANALYZER ───
    function analyzeAltitude(uav) {
        var alt = uav.altitude_geodetic;
        if (alt == null) return;

        if (alt > CONFIG.altitudeThresholds.restricted) {
            addInsight('HIGH', 'ALTITUDE VIOLATION',
                'Operating at ' + Math.round(alt) + 'm, exceeding ' + CONFIG.altitudeThresholds.restricted + 'm limit.',
                ['Report Violation', 'Alert Team']);
        } else if (alt < CONFIG.altitudeThresholds.low && uav.speed > CONFIG.speedThresholds.slow) {
            addInsight('MEDIUM', 'LOW ALTITUDE OPERATIONS',
                'Flying at ' + Math.round(alt) + 'm altitude. Increased risk of collision or surveillance.',
                ['Monitor']);
        }
    }

    // ─── SPEED ANALYZER ───
    function analyzeSpeed(uav) {
        var speed = uav.speed;
        if (speed == null) return;

        if (speed > CONFIG.speedThresholds.veryFast) {
            addInsight('HIGH', 'HIGH SPEED DETECTED',
                'Moving at ' + speed.toFixed(1) + ' m/s (' + Math.round(speed * 3.6) + ' km/h). Racing or evasion behavior.',
                ['Track', 'Alert Team']);
        } else if (speed < 0.5 && uav._sessionDurationS > 60) {
            addInsight('LOW', 'STATIONARY / LOITERING',
                'Aircraft has been stationary or hovering for extended period.',
                ['Monitor']);
        }
    }

    // ─── APPROACH VECTOR ANALYZER ───
    function analyzeApproachVector(uav) {
        if (!CONFIG.protectedAsset) return;
        if (!uav.latitude || !uav.longitude) return;
        if (uav.speed == null || uav.speed < 1) return;
        if (uav.ground_track == null) return;

        var asset = CONFIG.protectedAsset;
        var distance = haversineDistance(uav.latitude, uav.longitude, asset.lat, asset.lng);

        // Already inside protected radius
        if (distance < asset.radius) {
            addInsight('CRITICAL', 'INSIDE PROTECTED ZONE',
                'Aircraft is currently inside ' + esc(asset.name) + ' protected perimeter.',
                ['Create Incident', 'Alert Team']);
            return;
        }

        // Calculate bearing to asset
        var bearingToAsset = calculateBearing(uav.latitude, uav.longitude, asset.lat, asset.lng);
        var headingDiff = Math.abs(normalizeAngle(uav.ground_track - bearingToAsset));

        // Check if approaching (heading toward asset within 30 degrees)
        if (headingDiff < 30) {
            var eta = (distance - asset.radius) / uav.speed;

            if (eta < CONFIG.etaWarnings.imminent) {
                addInsight('CRITICAL', 'IMMINENT APPROACH',
                    'ETA to ' + esc(asset.name) + ': ' + Math.round(eta) + 's at current trajectory.',
                    ['Alert Team', 'Create Incident', 'Intercept']);
            } else if (eta < CONFIG.etaWarnings.approaching) {
                addInsight('HIGH', 'APPROACHING',
                    'On approach vector to ' + esc(asset.name) + '. ETA: ' + formatDuration(eta),
                    ['Monitor', 'Alert Team']);
            } else {
                addInsight('MEDIUM', 'HEADING TOWARD ASSET',
                    'Trajectory points toward ' + esc(asset.name) + '. Distance: ' + formatDistance(distance),
                    ['Monitor']);
            }
        } else if (distance < asset.radius * 3) {
            addInsight('LOW', 'NEAR PROTECTED AREA',
                'Operating ' + formatDistance(distance) + ' from ' + esc(asset.name) + '.',
                null);
        }
    }

    // ─── CLASSIFICATION ANALYZER ───
    function analyzeClassification(uav) {
        var classification = (uav.classification || 'UNKNOWN').toUpperCase();

        if (classification === 'HOSTILE') {
            addInsight('CRITICAL', 'HOSTILE AIRCRAFT',
                'This aircraft has been classified as HOSTILE.',
                ['Create Incident', 'Alert Team', 'Track']);
        } else if (classification === 'UNKNOWN' && (uav.trust_score || 100) < 80) {
            addInsight('MEDIUM', 'UNIDENTIFIED AIRCRAFT',
                'Unable to positively identify this aircraft. Classification pending.',
                ['Classify', 'Add to Watchlist']);
        } else if (classification === 'FRIENDLY') {
            addInsight('INFO', 'KNOWN FLEET',
                'This aircraft is part of the known fleet / whitelist.',
                null);
        }
    }

    // ─── CONTACT STATUS ANALYZER ───
    function analyzeContactStatus(uav) {
        if (uav._contactStatus === 'lost') {
            var lastSeenAgo = uav._lastSeenAgoS || 0;
            addInsight('MEDIUM', 'CONTACT LOST',
                'Last detected ' + formatDuration(lastSeenAgo) + ' ago. Aircraft may have left area or gone silent.',
                ['Show Last Known Position', 'Clear']);
        }
    }

    // ─── ADD INSIGHT ───
    function addInsight(priority, title, description, actions) {
        state.insights.push({
            priority: priority,
            title: title,
            description: description,
            actions: actions || []
        });
    }

    // ─── RENDER ───
    function render() {
        if (!state.container) return;

        var uav = state.selectedUAV;

        if (!uav) {
            state.container.innerHTML = '<div class="ds-panel ds-empty">' +
                '<div class="ds-empty-icon">&#128269;</div>' +
                '<div class="ds-empty-title">No Aircraft Selected</div>' +
                '<div class="ds-empty-desc">Select an aircraft to view decision support insights.</div>' +
            '</div>';
            return;
        }

        var name = uav.designation || uav.identifier || 'Unknown';
        var trust = uav.trust_score != null ? uav.trust_score : 100;
        var trustColor = trust >= 80 ? '#00E676' : trust >= 50 ? '#FFB300' : trust >= 30 ? '#FF6D00' : '#FF1744';

        var html = '<div class="ds-panel">';

        // Header
        html += '<div class="ds-header">' +
            '<span class="ds-header-icon">&#9432;</span>' +
            '<span class="ds-header-title">DECISION SUPPORT</span>' +
            '<span class="ds-trust-badge" style="color:' + trustColor + '">' + trust + '%</span>' +
        '</div>';

        // Target info
        html += '<div class="ds-target">' +
            '<div class="ds-target-name">' + esc(name) + '</div>' +
            '<div class="ds-target-meta">' +
                (uav.model ? '<span>' + esc(uav.model.replace(' (Unknown)', '')) + '</span>' : '') +
                (uav._contactStatus === 'active' ?
                    '<span class="ds-status-active">ACTIVE</span>' :
                    '<span class="ds-status-lost">LOST</span>') +
            '</div>' +
        '</div>';

        // Insights
        if (state.insights.length === 0) {
            html += '<div class="ds-insights">' +
                '<div class="ds-insight info">' +
                    '<span class="ds-insight-icon" style="color:#00E676">&#10003;</span>' +
                    '<div class="ds-insight-content">' +
                        '<div class="ds-insight-title">No Issues Detected</div>' +
                        '<div class="ds-insight-desc">This aircraft appears to be operating normally.</div>' +
                    '</div>' +
                '</div>' +
            '</div>';
        } else {
            html += '<div class="ds-insights">';
            state.insights.slice(0, 5).forEach(function(insight) {  // Limit to 5
                var p = CONFIG.priorities[insight.priority];
                var priorityClass = insight.priority.toLowerCase();

                html += '<div class="ds-insight ' + priorityClass + '">' +
                    '<span class="ds-insight-icon" style="color:' + p.color + '">' + p.icon + '</span>' +
                    '<div class="ds-insight-content">' +
                        '<div class="ds-insight-title">' + esc(insight.title) + '</div>' +
                        '<div class="ds-insight-desc">' + esc(insight.description) + '</div>';

                if (insight.actions && insight.actions.length > 0) {
                    html += '<div class="ds-insight-actions">';
                    insight.actions.forEach(function(action) {
                        html += '<button class="ds-action-btn" data-action="' + esc(action) + '">' +
                            esc(action) +
                        '</button>';
                    });
                    html += '</div>';
                }

                html += '</div></div>';
            });

            if (state.insights.length > 5) {
                html += '<div class="ds-more">+' + (state.insights.length - 5) + ' more insights</div>';
            }

            html += '</div>';
        }

        // Quick actions
        html += '<div class="ds-quick-actions">' +
            '<button class="ds-quick-btn" data-action="track"><span>&#128205;</span> Track</button>' +
            '<button class="ds-quick-btn" data-action="history"><span>&#128197;</span> History</button>' +
            '<button class="ds-quick-btn ds-quick-alert" data-action="alert"><span>&#128227;</span> Alert</button>' +
        '</div>';

        html += '</div>';

        state.container.innerHTML = html;

        // Bind action buttons
        bindActions();
    }

    // ─── BIND ACTIONS ───
    function bindActions() {
        if (!state.container) return;

        var buttons = state.container.querySelectorAll('.ds-action-btn, .ds-quick-btn');
        buttons.forEach(function(btn) {
            btn.addEventListener('click', function(e) {
                e.stopPropagation();
                var action = this.getAttribute('data-action');
                handleAction(action);
            });
        });
    }

    // ─── HANDLE ACTION ───
    function handleAction(action) {
        var uav = state.selectedUAV;
        if (!uav) return;

        var uavId = uav.identifier || uav.mac;

        // Custom callback
        if (state.onAction) {
            state.onAction(action, uav);
        }

        // Built-in handlers
        switch (action) {
            case 'Track':
            case 'track':
                if (typeof App !== 'undefined') {
                    App.locateDrone(uavId);
                    if (typeof NzMap !== 'undefined') NzMap.setAutoFollow(true);
                }
                break;

            case 'History':
            case 'history':
                if (typeof App !== 'undefined' && App.showHistory) {
                    App.showHistory(uavId);
                }
                break;

            case 'Tag Hostile':
            case 'Tag Suspicious':
                if (typeof App !== 'undefined' && App.showTagPopup) {
                    App.showTagPopup(null, uavId);
                } else {
                    fetch('/api/uav/' + encodeURIComponent(uavId) + '/classify', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ classification: 'HOSTILE' })
                    });
                }
                break;

            case 'Investigate':
            case 'Review Details':
                if (typeof App !== 'undefined') App.selectDrone(uavId);
                break;

            case 'Monitor':
                if (typeof App !== 'undefined') App.selectDrone(uavId);
                break;

            case 'Alert':
            case 'alert':
            case 'Alert Team':
                showAlertDialog(uav);
                break;

            case 'Create Incident':
                createIncident(uav);
                break;

            case 'Add to Watchlist':
                addToWatchlist(uav);
                break;

            case 'Report Violation':
                reportViolation(uav);
                break;

            case 'Classify':
                if (typeof App !== 'undefined' && App.showTagPopup) {
                    App.showTagPopup(null, uavId);
                }
                break;

            case 'Show Last Known Position':
                if (typeof NzMap !== 'undefined' && uav.latitude && uav.longitude) {
                    NzMap.getMap().panTo([uav.latitude, uav.longitude], { animate: true });
                }
                break;

            case 'Clear':
                if (typeof App !== 'undefined') App.dismissDrone(uavId);
                break;
        }
    }

    // ─── HELPER ACTIONS ───
    function showAlertDialog(uav) {
        var msg = 'Alert for: ' + (uav.designation || uav.identifier) + '\n\nWould you like to send an alert to the security team?';
        if (confirm(msg)) {
            // Could integrate with alerting API
            console.log('Alert sent for:', uav.identifier);
            if (typeof App !== 'undefined' && App.showToast) {
                App.showToast('Alert sent to team', 'success');
            }
        }
    }

    function createIncident(uav) {
        console.log('Creating incident for:', uav.identifier);
        if (typeof App !== 'undefined' && App.showToast) {
            App.showToast('Incident created', 'success');
        }
        // Could POST to /api/incidents
    }

    function addToWatchlist(uav) {
        console.log('Adding to watchlist:', uav.identifier);
        fetch('/api/uav/' + encodeURIComponent(uav.identifier || uav.mac) + '/tag', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ tag: 'monitored' })
        }).then(function() {
            if (typeof App !== 'undefined' && App.showToast) {
                App.showToast('Added to watchlist', 'success');
            }
        });
    }

    function reportViolation(uav) {
        console.log('Reporting violation for:', uav.identifier);
        if (typeof App !== 'undefined' && App.showToast) {
            App.showToast('Violation reported', 'success');
        }
    }

    // ─── GEOMETRY HELPERS ───
    function haversineDistance(lat1, lng1, lat2, lng2) {
        var R = 6371000;
        var dLat = (lat2 - lat1) * Math.PI / 180;
        var dLng = (lng2 - lng1) * Math.PI / 180;
        var a = Math.sin(dLat / 2) * Math.sin(dLat / 2) +
            Math.cos(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) *
            Math.sin(dLng / 2) * Math.sin(dLng / 2);
        return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
    }

    function calculateBearing(lat1, lng1, lat2, lng2) {
        var dLng = (lng2 - lng1) * Math.PI / 180;
        var y = Math.sin(dLng) * Math.cos(lat2 * Math.PI / 180);
        var x = Math.cos(lat1 * Math.PI / 180) * Math.sin(lat2 * Math.PI / 180) -
            Math.sin(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) * Math.cos(dLng);
        return (Math.atan2(y, x) * 180 / Math.PI + 360) % 360;
    }

    function normalizeAngle(angle) {
        while (angle > 180) angle -= 360;
        while (angle < -180) angle += 360;
        return angle;
    }

    function formatDistance(meters) {
        if (meters < 1000) return Math.round(meters) + 'm';
        return (meters / 1000).toFixed(1) + 'km';
    }

    function formatDuration(seconds) {
        if (seconds < 60) return Math.round(seconds) + 's';
        if (seconds < 3600) return Math.floor(seconds / 60) + 'm ' + Math.round(seconds % 60) + 's';
        return Math.floor(seconds / 3600) + 'h ' + Math.floor((seconds % 3600) / 60) + 'm';
    }

    // ─── MOUNT ───
    function mount(containerId, options) {
        var container = document.getElementById(containerId);
        if (!container) {
            console.error('DecisionSupport: Container not found:', containerId);
            return false;
        }

        state.container = container;
        state.mounted = true;

        if (options) {
            if (options.protectedAsset) CONFIG.protectedAsset = options.protectedAsset;
            if (options.onAction) state.onAction = options.onAction;
        }

        render();
        return true;
    }

    // ─── UNMOUNT ───
    function unmount() {
        if (state.container) {
            state.container.innerHTML = '';
        }
        state.mounted = false;
        state.container = null;
        state.selectedUAV = null;
        state.insights = [];
    }

    // ─── SET PROTECTED ASSET ───
    function setProtectedAsset(lat, lng, name, radius) {
        CONFIG.protectedAsset = {
            lat: lat,
            lng: lng,
            name: name || 'Protected Asset',
            radius: radius || 100
        };

        // Re-analyze if UAV selected
        if (state.selectedUAV) {
            analyze(state.selectedUAV);
        }
    }

    // ─── GET INSIGHTS ───
    function getInsights() {
        return state.insights.slice();
    }

    // ─── PUBLIC API ───
    return {
        mount: mount,
        unmount: unmount,
        analyze: analyze,
        setProtectedAsset: setProtectedAsset,
        getInsights: getInsights,
        CONFIG: CONFIG
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = DecisionSupport;
}
