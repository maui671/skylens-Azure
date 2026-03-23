/* ═══════════════════════════════════════════════════════
   SKYLENS COMMAND CENTER — DASHBOARD JS
   Operator-focused: threat, contacts, alerts, sensors
   FIXED: Stable DOM updates, reduced poll rate
   ═══════════════════════════════════════════════════════ */

(function () {
"use strict";

var POLL_MS = 5000;   // Fallback poll rate (SSE provides real-time updates)
var map = null;
var droneMarkers = {};
var droneVectors = {};
var tapMarkers = {};
var lastThreat = null;
var pollFails = 0;

// ─── EVENT TRACKING ───
var knownUavIds = {};       // identifier -> true (persists across polls)
var KNOWN_UAVS_MAX = 500;   // Cap to prevent unbounded growth

// ─── TIMER HANDLES (for cleanup) ───
var _pollTimer = null;
var _tickTimer = null;
var prevPendingAlerts = 0;
var prevThreatLevel = "LOW";
var firstPoll = true;

// ─── CLASSIFICATION COLORS ───
var CLASSIFICATION_COLORS = {
    "FRIENDLY": "#4CAF50",   // Green
    "HOSTILE": "#FF1744",    // Red
    "NEUTRAL": "#2196F3",    // Blue
    "SUSPECT": "#FF9800",    // Orange
    "UNKNOWN": "#9E9E9E"     // Gray
};

function getClassificationColor(classification) {
    return CLASSIFICATION_COLORS[(classification || "UNKNOWN").toUpperCase()] || CLASSIFICATION_COLORS.UNKNOWN;
}

// Build a useful display name from manufacturer + model/serial
function buildDroneName(u) {
    var mfg = u.manufacturer && u.manufacturer !== "UNKNOWN" ? u.manufacturer : null;
    var model = u.model && u.model !== "UNKNOWN" && u.model !== "" ? u.model.replace(" (Unknown)", "") : null;
    if (model && mfg && model === mfg) model = null; // avoid "DJI DJI"
    var serial = u.serial_number || null;
    var ident = u.identifier || "Unknown";
    var trk = u.track_number > 0 ? "TRK-" + String(u.track_number).padStart(3, "0") + " " : "";

    if (mfg && model) return trk + mfg + " " + model;
    if (mfg && serial) {
        var shortSerial = serial.length > 10 ? serial.substring(0, 10) + "…" : serial;
        return trk + mfg + " • " + shortSerial;
    }
    if (mfg) return trk + mfg;
    if (serial) return trk + (serial.length > 14 ? serial.substring(0, 14) + "…" : serial);
    return trk + (ident.length > 14 ? ident.substring(0, 14) + "…" : ident);
}

// ─── OPERATIONAL STATE ───
var lastUavs = [];          // stored for live timer + search + keyboard
var lastAlerts = [];        // stored for quick-ack
var lastPollTime = 0;       // ms timestamp of last successful poll
var selectedIdx = -1;       // keyboard-selected contact index
var contactFilter = "";     // search filter string
var prevRssi = {};          // identifier -> last RSSI for approach/depart
var lastUavsHash = "";      // hash to detect changes
var lastAlertsHash = "";    // hash to detect changes
var lastSensorsHash = "";   // hash to detect changes
var showControllers = false; // read from localStorage

// Stable DOM caching for cards
var contactCards = {};      // identifier -> DOM element
var sensorCards = {};       // tap_uuid -> DOM element

// ─── AUDIO ENGINE (Web Audio API) ───
var Audio = {
    ctx: null,
    enabled: false,
    init: function() {
        this.enabled = localStorage.getItem("skylens_audio") === "true";
        this.updateBtn();
    },
    getCtx: function() {
        if (!this.ctx) {
            this.ctx = new (window.AudioContext || window.webkitAudioContext)();
        }
        if (this.ctx.state === "suspended") this.ctx.resume();
        return this.ctx;
    },
    toggle: function() {
        this.enabled = !this.enabled;
        localStorage.setItem("skylens_audio", String(this.enabled));
        if (this.enabled) this.getCtx();
        this.updateBtn();
    },
    updateBtn: function() {
        var btn = document.getElementById("hdr-audio-btn");
        if (!btn) return;
        btn.textContent = this.enabled ? "AUDIO ON" : "AUDIO OFF";
        btn.className = "hdr-audio-btn" + (this.enabled ? " active" : "");
    },
    // Generate tones procedurally — no external files needed
    play: function(type) {
        if (!this.enabled) return;
        try {
            var ctx = this.getCtx();
            var now = ctx.currentTime;
            var g = ctx.createGain();
            g.connect(ctx.destination);

            if (type === "new_contact") {
                // Ascending two-tone radar contact beep
                var o1 = ctx.createOscillator();
                o1.type = "sine";
                o1.frequency.setValueAtTime(880, now);
                o1.frequency.setValueAtTime(1320, now + 0.08);
                o1.connect(g);
                g.gain.setValueAtTime(0.15, now);
                g.gain.exponentialRampToValueAtTime(0.001, now + 0.25);
                o1.start(now);
                o1.stop(now + 0.25);
            } else if (type === "spoofing") {
                // Urgent triple-pulse warning
                var o2 = ctx.createOscillator();
                o2.type = "square";
                o2.frequency.setValueAtTime(660, now);
                o2.connect(g);
                g.gain.setValueAtTime(0, now);
                for (var i = 0; i < 3; i++) {
                    var t = now + i * 0.15;
                    g.gain.setValueAtTime(0.12, t);
                    g.gain.setValueAtTime(0, t + 0.08);
                }
                o2.start(now);
                o2.stop(now + 0.5);
            } else if (type === "critical") {
                // Rising alarm tone
                var o3 = ctx.createOscillator();
                o3.type = "sawtooth";
                o3.frequency.setValueAtTime(440, now);
                o3.frequency.linearRampToValueAtTime(880, now + 0.4);
                o3.connect(g);
                g.gain.setValueAtTime(0.1, now);
                g.gain.exponentialRampToValueAtTime(0.001, now + 0.5);
                o3.start(now);
                o3.stop(now + 0.5);
            } else if (type === "lost") {
                // Descending tone
                var o4 = ctx.createOscillator();
                o4.type = "sine";
                o4.frequency.setValueAtTime(660, now);
                o4.frequency.linearRampToValueAtTime(220, now + 0.3);
                o4.connect(g);
                g.gain.setValueAtTime(0.1, now);
                g.gain.exponentialRampToValueAtTime(0.001, now + 0.35);
                o4.start(now);
                o4.stop(now + 0.35);
            }
        } catch (e) { /* audio not available */ }
    }
};

// ─── SINGLE UAV MARKER UPDATE (for SSE real-time) ───
function updateDroneMarker(u) {
    if (!map || !u.latitude || !u.longitude) return;
    var id = u.identifier;
    var ll = [u.latitude, u.longitude];
    var trust = u.trust_score != null ? u.trust_score : 100;
    var trustColor = trust >= 80 ? '#00E676' : trust >= 50 ? '#FFB300' : '#FF1744';
    var classColor = '#FF1744';  // ALL DRONES RED
    var classLabel = (u.classification || 'UNKNOWN').toUpperCase();

    var popupHtml = '<b>' + esc(buildDroneName(u)) + '</b>' +
        '<br><span style="color:' + classColor + ';font-weight:bold">' + classLabel + '</span>' +
        (u.detection_source ? '<br>Source: ' + esc(u.detection_source) : '') +
        '<br>Alt: ' + (u.altitude_geodetic != null ? u.altitude_geodetic.toFixed(0) + 'm' : '?') +
        ' / Spd: ' + (u.speed != null ? u.speed.toFixed(1) + ' m/s' : '0') +
        (u.rssi != null ? '<br>RSSI: ' + u.rssi + ' dBm' : '') +
        '<br>Trust: <b style="color:' + trustColor + '">' + trust + '%</b>' +
        (u.spoof_flags && u.spoof_flags.length ? '<br><span style="color:#FF1744">Flags: ' + u.spoof_flags.join(', ') + '</span>' : '') +
        '<br><a href="/airspace" style="color:#4CAF50;font-size:11px">Open in Airspace &#8594;</a>';

    if (droneMarkers[id]) {
        droneMarkers[id].setLatLng(ll);
        droneMarkers[id].setPopupContent(popupHtml);
        droneMarkers[id].setStyle({ color: classColor, fillColor: classColor });
    } else {
        droneMarkers[id] = L.circleMarker(ll, {
            radius: 6,
            color: classColor,
            fillColor: classColor,
            fillOpacity: 0.75,
            weight: 2,
        }).addTo(map).bindPopup(popupHtml);
        // Pan to new drone
        map.panTo(ll);
    }

    // Velocity vector
    if (u.speed != null && u.speed > 0.5 && u.ground_track != null) {
        var len = Math.min(u.speed * 15, 200);
        var rad = u.ground_track * Math.PI / 180;
        var endLat = ll[0] + (len / 111320) * Math.cos(rad);
        var endLng = ll[1] + (len / (111320 * Math.cos(ll[0] * Math.PI / 180))) * Math.sin(rad);
        if (droneVectors[id]) {
            droneVectors[id].setLatLngs([ll, [endLat, endLng]]);
        } else {
            droneVectors[id] = L.polyline([ll, [endLat, endLng]], {
                color: "#FF174480", weight: 2
            }).addTo(map);
        }
    }
}

// ─── SSE (Server-Sent Events) for real-time updates ───
var sseSource = null;
var sseReconnectDelay = 1000;

function initSSE() {
    if (sseSource) {
        sseSource.close();
    }
    sseSource = new EventSource("/api/events");

    sseSource.onopen = function() {
        console.log("SSE connected");
        sseReconnectDelay = 1000; // Reset backoff on success
        lastPollTime = Date.now(); // SSE connected = data is live
    };

    sseSource.addEventListener("uav_new", function(e) {
        lastPollTime = Date.now(); // SSE event = data is fresh
        try {
            var uav = JSON.parse(e.data);
            // Skip controllers when hidden
            if (!showControllers && isController(uav)) return;
            // Check if this is truly new (with cap to prevent unbounded growth)
            if (!knownUavIds[uav.identifier]) {
                // Cap knownUavIds to prevent memory leak
                var knownKeys = Object.keys(knownUavIds);
                if (knownKeys.length >= KNOWN_UAVS_MAX) {
                    // Remove oldest 25%
                    for (var ki = 0; ki < KNOWN_UAVS_MAX / 4; ki++) {
                        delete knownUavIds[knownKeys[ki]];
                    }
                }
                knownUavIds[uav.identifier] = true;
                Audio.play("new_contact");
                // Browser notification
                if ("Notification" in window && Notification.permission === "granted") {
                    var name = buildDroneName(uav);
                    try { new Notification("SKYLENS — New Contact", { body: name + " detected", tag: "nz-" + uav.identifier }); } catch(err) {}
                }
            }
            // Update map marker immediately
            updateDroneMarker(uav);
        } catch (err) { console.error("SSE uav_new parse error:", err); }
    });

    sseSource.addEventListener("uav_update", function(e) {
        lastPollTime = Date.now(); // SSE event = data is fresh
        try {
            var uav = JSON.parse(e.data);
            // Skip controllers when hidden
            if (!showControllers && isController(uav)) return;
            // Update map marker immediately
            updateDroneMarker(uav);
            // Check for spoofing
            if (uav.spoof_flags && uav.spoof_flags.length > 0 && (uav.trust_score || 100) < 50) {
                if (knownUavIds[uav.identifier] !== "spoofed") {
                    knownUavIds[uav.identifier] = "spoofed";
                    Audio.play("spoofing");
                }
            }
        } catch (err) { console.error("SSE uav_update parse error:", err); }
    });

    sseSource.onerror = function() {
        console.log("SSE disconnected, reconnecting in " + sseReconnectDelay + "ms");
        sseSource.close();
        setTimeout(initSSE, sseReconnectDelay);
        sseReconnectDelay = Math.min(sseReconnectDelay * 1.5, 30000);
    };
}

// ─── CLEANUP (on page unload) ───
function cleanup() {
    if (_pollTimer) { clearInterval(_pollTimer); _pollTimer = null; }
    if (_tickTimer) { clearInterval(_tickTimer); _tickTimer = null; }
    if (sseSource) { sseSource.close(); sseSource = null; }
    if (Audio.ctx) { try { Audio.ctx.close(); } catch(e) {} Audio.ctx = null; }
}

// ─── INIT ───
function loadShowControllers() {
    try {
        var v = localStorage.getItem("skylens_show_controllers");
        if (v !== null) return JSON.parse(v) === true;
        // Fallback: read from nz_settings blob (airspace page)
        var blob = JSON.parse(localStorage.getItem("nz_settings") || "{}");
        if (blob.showControllers !== undefined) return !!blob.showControllers;
    } catch (e) {}
    return false;
}

function isController(u) {
    return u.is_controller === true;
}

function init() {
    showControllers = loadShowControllers();
    Audio.init();
    initMap();
    initSSE();  // Start real-time updates
    tick();
    poll();
    _pollTimer = setInterval(poll, POLL_MS);
    _tickTimer = setInterval(tick, 1000);

    // Cleanup on page unload
    window.addEventListener("beforeunload", cleanup);
    window.addEventListener("pagehide", cleanup);

    // Audio toggle button
    var audioBtn = document.getElementById("hdr-audio-btn");
    if (audioBtn) {
        audioBtn.addEventListener("click", function() { Audio.toggle(); });
    }

    // Request notification permission (non-blocking)
    if ("Notification" in window && Notification.permission === "default") {
        Notification.requestPermission();
    }

    // Contact search filter
    var searchEl = document.getElementById("ct-search");
    if (searchEl) {
        searchEl.addEventListener("input", function() {
            contactFilter = this.value.toLowerCase().trim();
            renderContacts(lastUavs);
        });
    }

    // Contact card click -> navigate to airspace with drone selected (event delegation)
    var contactList = document.getElementById("contacts-list");
    if (contactList) {
        contactList.addEventListener("click", function(e) {
            var card = e.target.closest(".contact-card");
            if (!card) return;
            var id = card.getAttribute("data-id");
            if (id) window.location.href = "/airspace?focus=" + encodeURIComponent(id);
        });
    }

    // Alert table quick-ack (event delegation)
    var alertBody = document.getElementById("alert-body");
    if (alertBody) {
        alertBody.addEventListener("click", function(e) {
            var btn = e.target.closest(".alert-ack-btn");
            if (!btn) return;
            var alertId = btn.getAttribute("data-alert-id");
            if (!alertId) return;
            btn.textContent = "...";
            btn.classList.add("acked");
            SkylensAuth.fetch("/api/alert/" + alertId + "/ack", { method: "POST" })
                .then(function() { poll(); })
                .catch(function() { btn.textContent = "ACK"; btn.classList.remove("acked"); });
        });
    }

    // Ack-all button
    var ackAllBtn = document.getElementById("ack-all-btn");
    if (ackAllBtn) {
        ackAllBtn.addEventListener("click", function() {
            ackAllBtn.textContent = "...";
            SkylensAuth.fetch("/api/alerts/ack-all", { method: "POST" })
                .then(function() { ackAllBtn.textContent = "ACK ALL"; poll(); })
                .catch(function() { ackAllBtn.textContent = "ACK ALL"; });
        });
    }

    // Clear alerts button
    var clearAlertsBtn = document.getElementById("clear-alerts-btn");
    if (clearAlertsBtn) {
        clearAlertsBtn.addEventListener("click", function() {
            if (!confirm("Delete all alerts?")) return;
            clearAlertsBtn.textContent = "...";
            SkylensAuth.fetch("/api/alerts/clear", { method: "POST" })
                .then(function() { clearAlertsBtn.textContent = "CLEAR"; poll(); })
                .catch(function() { clearAlertsBtn.textContent = "CLEAR"; });
        });
    }

    // Keyboard shortcuts
    document.addEventListener("keydown", function(e) {
        // Don't intercept when typing in search
        if (e.target.tagName === "INPUT" || e.target.tagName === "TEXTAREA") {
            if (e.key === "Escape") { e.target.blur(); selectedIdx = -1; renderContacts(lastUavs); }
            return;
        }

        if (e.key === "ArrowDown") {
            e.preventDefault();
            var max = getFilteredUavs().length;
            if (max > 0) { selectedIdx = Math.min(selectedIdx + 1, max - 1); renderContacts(lastUavs); }
        } else if (e.key === "ArrowUp") {
            e.preventDefault();
            if (selectedIdx > 0) { selectedIdx--; renderContacts(lastUavs); }
        } else if (e.key === "Enter" && selectedIdx >= 0) {
            window.location.href = "/airspace";
        } else if (e.key === "Escape") {
            selectedIdx = -1;
            renderContacts(lastUavs);
        } else if (e.key === "a" && !e.ctrlKey && !e.metaKey) {
            // Ack all alerts
            if (ackAllBtn) ackAllBtn.click();
        } else if (e.key === "/" || e.key === "f" && !e.ctrlKey) {
            // Focus search
            if (searchEl) { e.preventDefault(); searchEl.focus(); }
        }
    });
}

function tick() {
    var now = new Date();
    var el = document.getElementById("hdr-clock");
    if (el) {
        el.textContent = pad(now.getHours()) + ":" + pad(now.getMinutes()) + ":" + pad(now.getSeconds());
    }

    // Stale data indicator: show when last poll > 12s ago (2+ missed polls)
    var staleEl = document.getElementById("hdr-stale");
    if (staleEl && lastPollTime > 0) {
        var age = (Date.now() - lastPollTime) / 1000;
        if (age > 12) {
            staleEl.textContent = "STALE " + Math.floor(age) + "s";
            staleEl.classList.add("visible");
        } else {
            staleEl.classList.remove("visible");
        }
    }

    // Live contact timer update (update "Xs ago" every second without full re-render)
    var seenEls = document.querySelectorAll("[data-ts]");
    for (var i = 0; i < seenEls.length; i++) {
        seenEls[i].textContent = timeAgo(seenEls[i].getAttribute("data-ts"));
    }
}

function pad(n) { return n < 10 ? "0" + n : String(n); }

// ─── POLL ───
function poll() {
    // Fetch both status and threat in parallel
    var statusOk = false;
    fetch("/api/status")
        .then(function(r) {
            if (r.status === 401 && typeof SkylensAuth !== 'undefined') {
                SkylensAuth.refreshSession();
                throw new Error("401");
            }
            if (!r.ok) throw new Error(r.status);
            return r.json();
        })
        .then(function(data) {
            pollFails = 0;
            statusOk = true;
            lastPollTime = Date.now();
            setConnected(true);
            render(data);
        })
        .catch(function() {
            pollFails++;
            if (pollFails > 2) setConnected(false);
        });

    fetch("/api/threat")
        .then(function(r) {
            if (r.status === 401 && typeof SkylensAuth !== 'undefined') {
                SkylensAuth.refreshSession();
                throw new Error("401");
            }
            if (!r.ok) throw new Error(r.status);
            return r.json();
        })
        .then(function(data) { lastThreat = data; })
        .catch(function() {});
}

function setConnected(ok) {
    var el = document.getElementById("hdr-conn");
    if (el) el.innerHTML = ok
        ? '<span class="hdr-dot green"></span> Connected'
        : '<span class="hdr-dot red"></span> Disconnected';
    var sb = document.getElementById("sb-node-status");
    if (sb) sb.innerHTML = ok
        ? '<span class="sb-dot green"></span><span>Node Online</span>'
        : '<span class="sb-dot red"></span><span>Node Offline</span>';
}

// ─── RENDER ───
function render(d) {
    var s = d.stats || {};
    // Re-check setting each poll (user might change it on settings page)
    showControllers = loadShowControllers();
    var allUavs = d.uavs || [];
    lastUavs = showControllers ? allUavs : allUavs.filter(function(u) { return !isController(u); });
    d.uavs = lastUavs; // propagate filter to renderKPIs/renderThreatBanner/etc
    lastAlerts = (d.alerts_history || []).slice().sort(function(a, b) {
        return (b.timestamp || '').localeCompare(a.timestamp || '');
    });

    // Store data for settings modal
    storeSettingsData(d);

    // Track RSSI for approach/depart
    lastUavs.forEach(function(u) {
        if (u.rssi != null) {
            var prev = prevRssi[u.identifier];
            u._rssiTrend = prev != null ? (u.rssi - prev > 3 ? "approaching" : u.rssi - prev < -3 ? "departing" : "stable") : "stable";
            prevRssi[u.identifier] = u.rssi;
        }
    });

    renderHeader(d);
    renderThreatBanner(s, d);
    renderKPIs(s, d);
    renderContacts(lastUavs);
    renderAlerts(lastAlerts);
    renderMap(lastUavs, d.taps || []);
    renderSensors(d.taps || []);
    checkNewEvents(lastUavs, s);
    updateSidebarBadges(lastUavs, s);
    updatePageTitle(s);
    if (firstPoll && typeof SkylensAuth !== 'undefined') SkylensAuth.revealPage();
    firstPoll = false;
}

// ─── HEADER ───
function renderHeader(d) {
    var node = document.getElementById("hdr-node");
    if (node) {
        var name = d.node_name || "skylens-node";
        node.textContent = name;
    }

    var up = document.getElementById("hdr-uptime");
    if (up && d.uptime) up.textContent = "UP " + d.uptime;

    var sbVer = document.getElementById("sb-version");
    if (sbVer && d.version) sbVer.textContent = "v" + d.version;
}

// ─── THREAT BANNER ───
function renderThreatBanner(s, d) {
    var banner = document.getElementById("threat-banner");
    var levelEl = document.getElementById("threat-level");
    var detailEl = document.getElementById("threat-detail");
    if (!banner) return;

    var allUavs = d.uavs || [];

    // Filter out controllers if setting is off
    if (!showControllers) {
        allUavs = allUavs.filter(function(u) { return !isController(u); });
    }

    // Count active (non-lost) UAVs
    var activeUavs = allUavs.filter(function(u) { return u._contactStatus !== 'lost'; });
    var activeCount = activeUavs.length;

    // Set the big number = active UAV count
    if (levelEl) levelEl.textContent = activeCount;

    // Determine highest-severity classification among active UAVs
    var hasHostile = false;
    var hasLowTrust = false;
    var hasUnknown = false;
    var allFriendlyOrNeutral = activeCount > 0;
    var names = [];

    activeUavs.forEach(function(u) {
        var cls = (u.classification || "UNKNOWN").toUpperCase();
        var trust = u.trust_score != null ? u.trust_score : 100;

        if (cls === "HOSTILE") hasHostile = true;
        if (trust < 30) hasLowTrust = true;
        if (cls === "UNKNOWN" || cls === "") hasUnknown = true;
        if (cls !== "FRIENDLY" && cls !== "NEUTRAL") allFriendlyOrNeutral = false;

        names.push(buildDroneName(u));
    });

    // Determine banner state and detail text
    var bannerClass, detailText;

    if (activeCount === 0) {
        // Green — all clear
        bannerClass = "threat-clear";
        detailText = "All clear \u2014 no active contacts";
    } else if (hasHostile || hasLowTrust) {
        // Red — hostile or low trust
        bannerClass = "threat-alert";
        detailText = names.join(", ");
    } else if (hasUnknown) {
        // Yellow — unknown classification
        bannerClass = "threat-caution";
        detailText = names.join(", ");
    } else {
        // Blue — all friendly/neutral
        bannerClass = "threat-active";
        detailText = names.join(", ");
    }

    if (detailEl) detailEl.textContent = detailText;

    // Update banner class
    banner.className = "threat-banner " + bannerClass;
}

// ─── KPIs ───
function renderKPIs(s, d) {
    var al = s.alerts || {};
    var uavs = d.uavs || [];
    if (!showControllers) {
        uavs = uavs.filter(function(u) { return !isController(u); });
    }
    var taps = d.taps || [];
    var sp = s.spoof_detector || {};

    // UAVs in Airspace (active only, show +N for lost)
    var activeUavCount = 0;
    var lostUavCount = 0;
    for (var u = 0; u < uavs.length; u++) {
        if (uavs[u]._contactStatus === 'lost') lostUavCount++;
        else activeUavCount++;
    }
    var uavLabel = activeUavCount.toString();
    if (lostUavCount > 0) uavLabel += " +" + lostUavCount;
    setKPI("kpi-uavs", uavLabel);
    colorKPI("kpi-uavs", activeUavCount > 0 ? "#00B0FF" : lostUavCount > 0 ? "#607D8B" : "#00E676");

    // Sensors Online: count live taps (heartbeat < 30s old)
    var totalTaps = taps.length;
    var liveTaps = 0;
    for (var i = 0; i < taps.length; i++) {
        if (tapAge(taps[i].timestamp) < 60) liveTaps++;
    }
    setKPI("kpi-sensors", liveTaps + " / " + totalTaps);
    colorKPI("kpi-sensors", liveTaps === totalTaps && totalTaps > 0 ? "#00E676" :
             liveTaps > 0 ? "#FFB300" : "#FF1744");

    // Pending Alerts
    var pending = al.pending_alerts || 0;
    setKPI("kpi-pending", pending);
    colorKPI("kpi-pending", pending > 5 ? "#FF1744" : pending > 0 ? "#FF6D00" : "#00E676");

    // Spoof Suspects
    var spoofCount = 0;
    for (var j = 0; j < uavs.length; j++) {
        if ((uavs[j].trust_score || 100) < 50) spoofCount++;
    }
    setKPI("kpi-spoof", spoofCount);
    colorKPI("kpi-spoof", spoofCount > 0 ? "#FF1744" : "#00E676");

    // System Health — derive from errors, queue, and pipeline state
    var errors = s.errors || 0;
    var qDrops = s.queue_drops || 0;
    var qDepth = s.queue_depth || 0;
    var wrErrors = (s.writer || {}).errors || 0;
    var healthLabel, healthColor;
    if (errors > 10 || wrErrors > 5 || qDepth > 4000) {
        healthLabel = "CRITICAL";
        healthColor = "#FF1744";
    } else if (errors > 0 || wrErrors > 0 || qDrops > 0 || qDepth > 1000) {
        healthLabel = "DEGRADED";
        healthColor = "#FF6D00";
    } else {
        healthLabel = "OK";
        healthColor = "#00E676";
    }
    setKPI("kpi-health", healthLabel);
    colorKPI("kpi-health", healthColor);
}

function setKPI(id, val) {
    var el = document.getElementById(id);
    if (!el) return;
    var v = el.querySelector(".kpi-val");
    if (!v) return;
    var strVal = String(val);
    if (v.textContent !== strVal) {
        v.textContent = strVal;
        // Brief white flash that fades back to the target color
        v.classList.add("changed");
        setTimeout(function() { v.classList.remove("changed"); }, 50);
    }
}

function colorKPI(id, color) {
    var el = document.getElementById(id);
    if (!el) return;
    var v = el.querySelector(".kpi-val");
    if (v) v.style.color = color;
}

function fmtNum(n) {
    if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
    if (n >= 1000) return (n / 1000).toFixed(1) + "K";
    return String(n);
}

// ─── ACTIVE CONTACTS ───
function getFilteredUavs() {
    var sorted = lastUavs.slice().sort(function(a, b) {
        // Sort by most recent (last_seen) first
        var ta = new Date(a.last_seen || a.timestamp || 0).getTime();
        var tb = new Date(b.last_seen || b.timestamp || 0).getTime();
        return tb - ta;
    });
    if (!contactFilter) return sorted;
    return sorted.filter(function(u) {
        var haystack = [
            u.designation || "", u.identifier || "", u.detection_source || "",
            u.uav_type || "", u.mac || "", u.ssid || ""
        ].join(" ").toLowerCase();
        return haystack.indexOf(contactFilter) !== -1;
    });
}

function renderRssiBars(rssi) {
    if (rssi == null) return "";
    // 4 bars: -40=excellent, -55=good, -70=ok, -80=weak
    var level = rssi > -40 ? 4 : rssi > -55 ? 3 : rssi > -70 ? 2 : rssi > -80 ? 1 : 0;
    var cls = level <= 1 ? "very-weak" : level === 2 ? "weak" : "";
    var bars = "";
    var heights = [3, 6, 9, 12];
    for (var i = 0; i < 4; i++) {
        bars += '<span class="ct-rssi-bar' + (i < level ? " on" : "") + '" style="height:' + heights[i] + 'px"></span>';
    }
    return '<span class="ct-rssi ' + cls + '" title="RSSI: ' + rssi + ' dBm">' + bars + '</span>';
}

function renderContacts(uavs, forceRender) {
    var list = document.getElementById("contacts-list");
    var badge = document.getElementById("contact-badge");
    if (!list) return;

    if (badge) badge.textContent = uavs.length;

    var filtered = getFilteredUavs();

    if (!uavs.length) {
        selectedIdx = -1;
        // Clear cached cards
        contactCards = {};
        list.innerHTML = '<div class="empty-radar">' +
            '<div class="empty-radar-rings"><div class="ring"></div><div class="ring"></div><div class="ring"></div></div>' +
            '<div class="empty-radar-text">Airspace Clear</div>' +
            '<div class="empty-radar-sub">Scanning for UAV signals...</div>' +
            '</div>';
        return;
    }

    if (!filtered.length && contactFilter) {
        list.innerHTML = '<div class="empty-state">No matches for "' + esc(contactFilter) + '"</div>';
        return;
    }

    // Remove empty state if present
    var emptyEl = list.querySelector(".empty-radar, .empty-state");
    if (emptyEl) emptyEl.remove();

    // Clamp selected index
    if (selectedIdx >= filtered.length) selectedIdx = filtered.length - 1;

    // Build set of current IDs
    var currentIds = {};
    filtered.forEach(function(u) {
        currentIds[u.identifier] = u;
    });

    // Remove cards for UAVs no longer in filtered list
    Object.keys(contactCards).forEach(function(id) {
        if (!currentIds[id]) {
            if (contactCards[id] && contactCards[id].parentNode) {
                contactCards[id].parentNode.removeChild(contactCards[id]);
            }
            delete contactCards[id];
        }
    });

    // Update or create cards
    filtered.forEach(function(u, idx) {
        var id = u.identifier;
        if (contactCards[id]) {
            // UPDATE existing card in-place
            updateContactCard(contactCards[id], u, idx);
        } else {
            // CREATE new card
            var card = createContactCard(u, idx);
            contactCards[id] = card;
            list.appendChild(card);
        }
    });

    // Reorder cards to match sorted order
    filtered.forEach(function(u) {
        var card = contactCards[u.identifier];
        if (card && card.parentNode === list) {
            list.appendChild(card); // moves to end in sorted order
        }
    });

    // Scroll selected into view
    if (selectedIdx >= 0) {
        var cards = list.querySelectorAll(".contact-card");
        if (cards[selectedIdx]) cards[selectedIdx].scrollIntoView({ block: "nearest" });
    }
}

function createContactCard(u, idx) {
    var card = document.createElement("div");
    card.className = getContactCardClass(u, idx);
    card.setAttribute("data-id", u.identifier || "");
    card.innerHTML = buildContactCardInner(u);
    return card;
}

function getContactCardClass(u, idx) {
    var trust = u.trust_score != null ? u.trust_score : 100;
    var trustClass = trust < 30 ? "crit" : trust < 50 ? "high" : trust < 70 ? "med" : "safe";
    var isSelected = idx === selectedIdx;
    var isActive = u._contactStatus === "active";
    return "contact-card " + trustClass + (isSelected ? " selected" : "") + (isActive ? "" : " lost");
}

function buildContactCardInner(u) {
    var trust = u.trust_score != null ? u.trust_score : 100;
    var name = buildDroneName(u);
    var flags = u.spoof_flags || [];
    var source = u.detection_source || "Unknown";
    var alt = u.altitude_geodetic != null ? Math.round(u.altitude_geodetic) + "m" : "--";
    var spd = u.speed != null ? u.speed.toFixed(1) + " m/s" : "--";
    var ts = u.last_seen || u.timestamp;
    var isActive = u._contactStatus === "active";
    var model = (u.model || '').replace(' (Unknown)', '') || null;

    // RSSI + trend
    var rssiHtml = renderRssiBars(u.rssi);
    var trendIcon = "";
    if (u._rssiTrend === "approaching") trendIcon = '<span class="ct-approach" title="Signal increasing (approaching)">&#9650;</span>';
    else if (u._rssiTrend === "departing") trendIcon = '<span class="ct-depart" title="Signal decreasing (departing)">&#9660;</span>';

    var flagsHtml = "";
    if (flags.length) {
        flagsHtml = '<div class="ct-flags">' + flags.map(function(f) {
            return '<span class="ct-flag">' + esc(f) + '</span>';
        }).join("") + '</div>';
    }

    var statusClass = isActive ? "ct-status-active" : "ct-status-lost";
    var statusDot = '<span class="ct-status-dot ' + statusClass + '"></span>';
    var modelBadge = model ? '<span class="ct-model">' + esc(model) + '</span>' : '';

    return '<div class="ct-header">' +
            statusDot +
            '<div class="ct-name" title="' + esc(u.identifier || "") + '">' + esc(name) + '</div>' +
            rssiHtml + trendIcon +
            '<div class="ct-trust">' + trust + '%</div>' +
        '</div>' +
        (model ? '<div class="ct-model-row">' + modelBadge + '</div>' : '') +
        '<div class="ct-meta">' +
            '<span class="ct-tag">' + esc(source) + '</span>' +
            '<span class="ct-stat ct-alt">ALT ' + alt + '</span>' +
            '<span class="ct-stat ct-spd">SPD ' + spd + '</span>' +
            '<span class="ct-seen ' + (isActive ? "" : "lost") + '" data-ts="' + esc(ts || "") + '">' + (isActive ? timeAgo(ts) : "LOST " + timeAgo(ts)) + '</span>' +
        '</div>' +
        flagsHtml;
}

function updateContactCard(card, u, idx) {
    // Update card class
    card.className = getContactCardClass(u, idx);

    // Update trust percentage
    var trustEl = card.querySelector(".ct-trust");
    if (trustEl) {
        var trust = u.trust_score != null ? u.trust_score : 100;
        trustEl.textContent = trust + "%";
    }

    // Update name
    var nameEl = card.querySelector(".ct-name");
    if (nameEl) nameEl.textContent = buildDroneName(u);

    // Update status dot
    var dotEl = card.querySelector(".ct-status-dot");
    if (dotEl) {
        var isActive = u._contactStatus === "active";
        dotEl.className = "ct-status-dot " + (isActive ? "ct-status-active" : "ct-status-lost");
    }

    // Update altitude
    var altEl = card.querySelector(".ct-alt");
    if (altEl) {
        var alt = u.altitude_geodetic != null ? Math.round(u.altitude_geodetic) + "m" : "--";
        altEl.textContent = "ALT " + alt;
    }

    // Update speed
    var spdEl = card.querySelector(".ct-spd");
    if (spdEl) {
        var spd = u.speed != null ? u.speed.toFixed(1) + " m/s" : "--";
        spdEl.textContent = "SPD " + spd;
    }

    // Update seen time (handled by tick() for live updates via data-ts)
    var seenEl = card.querySelector(".ct-seen");
    if (seenEl) {
        var ts = u.last_seen || u.timestamp;
        var isActive = u._contactStatus === "active";
        seenEl.setAttribute("data-ts", ts || "");
        seenEl.className = "ct-seen " + (isActive ? "" : "lost");
    }
}

// ─── ALERTS TABLE ───
function renderAlerts(alerts, forceRender) {
    var body = document.getElementById("alert-body");
    var empty = document.getElementById("alert-empty");
    var badge = document.getElementById("alert-badge");
    if (!body) return;

    // Count unacknowledged
    var unacked = 0;
    for (var i = 0; i < alerts.length; i++) {
        if (!alerts[i].acknowledged) unacked++;
    }

    if (badge) {
        badge.textContent = unacked;
        badge.className = "badge" + (alerts.some(function(a) { return a.priority === "CRITICAL" && !a.acknowledged; }) ? " crit" :
                                     alerts.some(function(a) { return a.priority === "HIGH" && !a.acknowledged; }) ? " warn" : "");
    }

    if (!alerts.length) {
        if (lastAlertsHash !== "empty") {
            body.innerHTML = "";
            lastAlertsHash = "empty";
        }
        if (empty) empty.style.display = "";
        return;
    }
    if (empty) empty.style.display = "none";

    // Check if data changed (avoid re-render flicker on hover)
    var visibleAlerts = alerts.slice(0, 20);
    var newHash = quickHash(visibleAlerts, ['id', 'priority', 'alert_type', 'acknowledged', 'timestamp']);
    if (!forceRender && newHash === lastAlertsHash && body.children.length > 0) {
        return; // No changes, skip re-render
    }
    lastAlertsHash = newHash;

    body.innerHTML = visibleAlerts.map(function(a) {
        var pri = (a.priority || "INFO").toLowerCase();
        var priClass = pri === "critical" ? "crit" : pri === "high" ? "high" : pri === "medium" ? "med" : "info";
        var type = (a.alert_type || "").replace(/_/g, " ");
        var uavId = a.uav_identifier || "--";
        var acked = a.acknowledged;
        var ts = a.timestamp || "";

        // Quick-ack button replaces static badge
        var ackHtml = acked
            ? '<span class="ack-badge ack-yes">ACK</span>'
            : '<button class="alert-ack-btn" data-alert-id="' + (a.id || "") + '">ACK</button>';

        return '<tr class="' + (acked ? "acked" : "") + '">' +
            '<td><span class="pri-badge ' + priClass + '">' + esc(a.priority || "INFO") + '</span></td>' +
            '<td class="alert-type">' + esc(type) + '</td>' +
            '<td class="alert-uav" title="' + esc(uavId) + '">' + esc(uavId) + '</td>' +
            '<td class="alert-msg" title="' + esc(a.message || "") + '">' + esc(a.message || "") + '</td>' +
            '<td>' + ackHtml + '</td>' +
            '<td class="alert-time" data-ts="' + esc(ts) + '">' + timeAgo(ts) + '</td>' +
        '</tr>';
    }).join("");
}

// ─── MINI MAP ───
function initMap() {
    var el = document.getElementById("minimap");
    if (!el) return;

    map = L.map(el, {
        zoomControl: true,
        attributionControl: false,
        minZoom: 3,
        maxZoom: 18,
    }).setView([
        parseFloat(localStorage.getItem("skylens_map_center_lat")) || 18.44,
        parseFloat(localStorage.getItem("skylens_map_center_lng")) || -66.02
    ], parseInt(localStorage.getItem("skylens_map_zoom")) || 13);

    L.tileLayer("https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png", {
        subdomains: "abcd",
    }).addTo(map);
}

function renderMap(uavs, taps) {
    if (!map) return;

    // Update tap markers
    var tapIds = new Set();
    taps.forEach(function(t) {
        if (!t.latitude || !t.longitude) return;
        var id = t.tap_uuid;
        tapIds.add(id);
        var ll = [t.latitude, t.longitude];

        var popupHtml = '<b>' + esc(t.tap_name || "Sensor") + '</b>' +
            '<br><span style="font-size:10px;color:#999">' + esc(t.tap_uuid || "") + '</span>' +
            '<br>Status: ' + (tapAge(t.timestamp) < 60 ? '<span style="color:#00E676">Online</span>' : '<span style="color:#FF1744">Offline</span>') +
            '<br>Frames: ' + fmtNum(t.frames_total || 0) +
            (t.tap_uptime ? '<br>Uptime: ' + fmtDuration(t.tap_uptime) : '');

        if (tapMarkers[id]) {
            tapMarkers[id].setLatLng(ll);
            tapMarkers[id].setPopupContent(popupHtml);
        } else {
            tapMarkers[id] = L.circleMarker(ll, {
                radius: 7,
                color: "#4CAF50",
                fillColor: "#4CAF50",
                fillOpacity: 0.6,
                weight: 2,
            }).addTo(map).bindPopup(popupHtml);
        }
    });
    Object.keys(tapMarkers).forEach(function(id) {
        if (!tapIds.has(id)) { map.removeLayer(tapMarkers[id]); delete tapMarkers[id]; }
    });

    // Update drone markers — consistent red UAV, yellow operator circles
    var uavIds = new Set();
    var hadNewMarker = false;
    uavs.forEach(function(u) {
        if (!u.latitude || !u.longitude) return;
        var id = u.identifier;
        uavIds.add(id);
        var ll = [u.latitude, u.longitude];
        var trust = u.trust_score != null ? u.trust_score : 100;
        var trustColor = trust >= 80 ? '#00E676' : trust >= 50 ? '#FFB300' : '#FF1744';

        // Marker color - ALL DRONES RED (lost = reduced opacity)
        var isLost = u._contactStatus === 'lost';
        var classColor = '#FF1744';  // Always RED
        var markerOpacity = isLost ? 0.5 : 0.85;
        var classLabel = (u.classification || 'UNKNOWN').toUpperCase();
        var statusLabel = isLost ? '<br><span style="color:#90A4AE;font-weight:bold">LOST CONTACT</span>' : '';

        var popupHtml = '<b>' + esc(buildDroneName(u)) + '</b>' +
            statusLabel +
            '<br><span style="color:' + (isLost ? '#546E7A' : getClassificationColor(u.classification)) + ';font-weight:bold">' + classLabel + '</span>' +
            (u.detection_source ? '<br>Source: ' + esc(u.detection_source) : '') +
            '<br>Alt: ' + (u.altitude_geodetic != null ? u.altitude_geodetic.toFixed(0) + 'm' : '?') +
            ' / Spd: ' + (u.speed != null ? u.speed.toFixed(1) + ' m/s' : '0') +
            (u.rssi != null ? '<br>RSSI: ' + u.rssi + ' dBm' : '') +
            '<br>Trust: <b style="color:' + trustColor + '">' + trust + '%</b>' +
            (u.spoof_flags && u.spoof_flags.length ? '<br><span style="color:#FF1744">Flags: ' + u.spoof_flags.join(', ') + '</span>' : '') +
            (u.operator_latitude && u.operator_longitude ? '<br>Operator: ' + u.operator_latitude.toFixed(4) + ', ' + u.operator_longitude.toFixed(4) + '<br>Op MGRS: ' + MGRS.forward(u.operator_latitude, u.operator_longitude, 4) : '') +
            '<br><a href="/airspace" style="color:#4CAF50;font-size:11px">Open in Airspace &#8594;</a>';

        if (droneMarkers[id]) {
            droneMarkers[id].setLatLng(ll);
            droneMarkers[id].setPopupContent(popupHtml);
            droneMarkers[id].setStyle({ color: classColor, fillColor: classColor, fillOpacity: markerOpacity });
        } else {
            droneMarkers[id] = L.circleMarker(ll, {
                radius: isLost ? 5 : 6,
                color: classColor,
                fillColor: classColor,
                fillOpacity: markerOpacity,
                weight: isLost ? 1 : 2,
            }).addTo(map).bindPopup(popupHtml);
            hadNewMarker = true;
        }

        // Operator marker (yellow circle)
        if (u.operator_latitude && u.operator_longitude &&
            !(u.operator_latitude === 0 && u.operator_longitude === 0)) {
            var opId = id + '_op';
            var oLL = [u.operator_latitude, u.operator_longitude];
            if (!droneMarkers[opId]) {
                droneMarkers[opId] = L.circleMarker(oLL, {
                    radius: 5,
                    color: "#FFB300",
                    fillColor: "#FFB300",
                    fillOpacity: 0.70,
                    weight: 2,
                }).addTo(map).bindPopup('<b>Operator</b><br>' + esc(buildDroneName(u)));
                uavIds.add(opId);
            } else {
                droneMarkers[opId].setLatLng(oLL);
                uavIds.add(opId);
            }
        }

        // Velocity vector
        if (u.speed != null && u.speed > 0.5 && u.ground_track != null) {
            var len = Math.min(u.speed * 15, 200);
            var rad = u.ground_track * Math.PI / 180;
            var endLat = ll[0] + (len / 111320) * Math.cos(rad);
            var endLng = ll[1] + (len / (111320 * Math.cos(ll[0] * Math.PI / 180))) * Math.sin(rad);
            if (droneVectors[id]) {
                droneVectors[id].setLatLngs([ll, [endLat, endLng]]);
            } else {
                droneVectors[id] = L.polyline([ll, [endLat, endLng]], {
                    color: "#FF174480", weight: 2
                }).addTo(map);
            }
        } else if (droneVectors[id]) {
            map.removeLayer(droneVectors[id]); delete droneVectors[id];
        }
    });
    Object.keys(droneMarkers).forEach(function(id) {
        if (!uavIds.has(id)) {
            map.removeLayer(droneMarkers[id]); delete droneMarkers[id];
            if (droneVectors[id]) { map.removeLayer(droneVectors[id]); delete droneVectors[id]; }
        }
    });

    // Re-fit bounds when new markers appear so new drones are always visible
    var allMarkers = [].concat(Object.values(tapMarkers), Object.values(droneMarkers));
    if (allMarkers.length && (hadNewMarker || !map._hasData)) {
        var group = L.featureGroup(allMarkers);
        map.fitBounds(group.getBounds().pad(0.2));
        map._hasData = true;
    }
}

// ─── SENSOR STRIP (STABLE DOM UPDATES) ───
function renderSensors(taps) {
    var strip = document.getElementById("sensor-strip");
    if (!strip) return;

    if (!taps.length) {
        // Clear all cached sensor cards
        sensorCards = {};
        strip.innerHTML = '<div class="empty-state">No sensors connected</div>';
        return;
    }

    // Remove empty state if present
    var emptyEl = strip.querySelector(".empty-state");
    if (emptyEl) emptyEl.remove();

    // Sort taps by name for consistent ordering
    taps.sort(function(a, b) {
        var nameA = (a.tap_name || a.tap_uuid || "").toLowerCase();
        var nameB = (b.tap_name || b.tap_uuid || "").toLowerCase();
        return nameA.localeCompare(nameB);
    });

    // Build set of current tap IDs
    var currentIds = {};
    taps.forEach(function(t) {
        var id = t.tap_uuid || t.tap_name;
        currentIds[id] = t;
    });

    // Remove cards for taps no longer present
    Object.keys(sensorCards).forEach(function(id) {
        if (!currentIds[id]) {
            if (sensorCards[id] && sensorCards[id].parentNode) {
                sensorCards[id].parentNode.removeChild(sensorCards[id]);
            }
            delete sensorCards[id];
        }
    });

    // Update or create sensor cards
    taps.forEach(function(t) {
        var id = t.tap_uuid || t.tap_name;
        if (sensorCards[id]) {
            // UPDATE existing card in-place
            updateSensorCard(sensorCards[id], t);
        } else {
            // CREATE new card
            var card = createSensorCard(t);
            sensorCards[id] = card;
            strip.appendChild(card);
        }
    });
}

function createSensorCard(t) {
    var card = document.createElement("div");
    var age = tapAge(t.timestamp);
    var status = age < 60 ? "live" : age < 120 ? "warn" : "dead";
    card.className = "sensor-card " + status;
    card.innerHTML = buildSensorCardInner(t);
    return card;
}

function buildSensorCardInner(t) {
    var age = tapAge(t.timestamp);
    var isOffline = age >= 120;
    var statusLabel = age < 60 ? "Online" : age < 120 ? "Stale" : "Offline";
    var name = t.tap_name || t.tap_uuid || "Unknown";
    var frames = fmtNum(t.frames_total || t.packets_captured || 0);
    var capturing = t.capture_running && !isOffline;  // Don't show capturing if offline
    var pps = t.packets_per_second != null ? t.packets_per_second.toFixed(1) : "0";

    // Capture status: Offline > Stopped > Capturing
    var captureClass = isOffline ? "bad" : (capturing ? "good" : "bad");
    var captureLabel = isOffline ? "Offline" : (capturing ? "Capturing" : "Stopped");

    return '<div class="sn-header">' +
            '<span class="sn-dot"></span>' +
            '<span class="sn-name">' + esc(name) + '</span>' +
            '<span class="sn-status">' + statusLabel + '</span>' +
        '</div>' +
        '<div class="sn-stats">' +
            '<span class="sn-frames">' + frames + ' frames</span>' +
            '<span class="sn-pps">' + pps + ' pps</span>' +
            '<span class="sn-capture ' + captureClass + '">' + captureLabel + '</span>' +
        '</div>';
}

function updateSensorCard(card, t) {
    var age = tapAge(t.timestamp);
    var isOffline = age >= 120;
    var status = age < 60 ? "live" : age < 120 ? "warn" : "dead";
    var statusLabel = age < 60 ? "Online" : age < 120 ? "Stale" : "Offline";

    // Update card class
    card.className = "sensor-card " + status;

    // Update name
    var nameEl = card.querySelector(".sn-name");
    if (nameEl) nameEl.textContent = t.tap_name || t.tap_uuid || "Unknown";

    // Update status label
    var statusEl = card.querySelector(".sn-status");
    if (statusEl) statusEl.textContent = statusLabel;

    // Update frames
    var framesEl = card.querySelector(".sn-frames");
    if (framesEl) framesEl.textContent = fmtNum(t.frames_total || t.packets_captured || 0) + " frames";

    // Update pps
    var ppsEl = card.querySelector(".sn-pps");
    if (ppsEl) ppsEl.textContent = (t.packets_per_second != null ? t.packets_per_second.toFixed(1) : "0") + " pps";

    // Update capture status - don't show "Capturing" if tap is offline
    var captureEl = card.querySelector(".sn-capture");
    if (captureEl) {
        var capturing = t.capture_running && !isOffline;
        var captureClass = isOffline ? "bad" : (capturing ? "good" : "bad");
        var captureLabel = isOffline ? "Offline" : (capturing ? "Capturing" : "Stopped");
        captureEl.className = "sn-capture " + captureClass;
        captureEl.textContent = captureLabel;
    }
}

// ─── UTILS ───
var _escDiv = document.createElement("div");
function esc(s) {
    _escDiv.textContent = s;
    return _escDiv.innerHTML;
}

function timeAgo(ts) {
    if (!ts) return "--";
    var diff = (Date.now() - new Date(ts).getTime()) / 1000;
    if (diff < 0) diff = 0;
    if (diff < 60) return Math.floor(diff) + "s ago";
    if (diff < 3600) return Math.floor(diff / 60) + "m ago";
    if (diff < 86400) return Math.floor(diff / 3600) + "h" + Math.floor((diff % 3600) / 60) + "m ago";
    var days = Math.floor(diff / 86400);
    var hours = Math.floor((diff % 86400) / 3600);
    return days + "d" + hours + "h ago";
}

function tapAge(ts) {
    if (!ts) return 999;
    return (Date.now() - new Date(ts).getTime()) / 1000;
}

// Simple hash for change detection (avoids unnecessary re-renders)
function quickHash(arr, keys) {
    return arr.map(function(item) {
        return keys.map(function(k) { return item[k]; }).join('|');
    }).join('~');
}

function fmtDuration(secs) {
    secs = Math.floor(secs);
    if (secs < 60) return secs + "s";
    if (secs < 3600) return Math.floor(secs / 60) + "m " + (secs % 60) + "s";
    var h = Math.floor(secs / 3600);
    var m = Math.floor((secs % 3600) / 60);
    if (h >= 24) {
        var days = Math.floor(h / 24);
        h = h % 24;
        return days + "d " + h + "h";
    }
    return h + "h " + m + "m";
}

// ─── NEW EVENT DETECTION ───
function checkNewEvents(uavs, stats) {
    if (firstPoll) {
        // Seed known UAVs on first poll — don't alarm on existing contacts
        uavs.forEach(function(u) { knownUavIds[u.identifier] = true; });
        prevPendingAlerts = (stats.alerts || {}).pending_alerts || 0;
        prevThreatLevel = (lastThreat || {}).threat_level || "LOW";
        return;
    }

    // Detect new UAV contacts
    var newContacts = [];
    uavs.forEach(function(u) {
        if (!knownUavIds[u.identifier]) {
            newContacts.push(u);
            knownUavIds[u.identifier] = true;
        }
    });

    if (newContacts.length > 0) {
        Audio.play("new_contact");
        // Browser notification for new contacts
        if ("Notification" in window && Notification.permission === "granted") {
            var name = newContacts[0].designation || newContacts[0].identifier || "Unknown";
            var body = newContacts.length === 1
                ? name + " detected"
                : newContacts.length + " new contacts detected";
            try { new Notification("SKYLENS — New Contact", { body: body, tag: "nz-contact" }); } catch(e) {}
        }
    }

    // Detect spoofing (any UAV with trust < 50 that has spoof flags)
    uavs.forEach(function(u) {
        if (u.spoof_flags && u.spoof_flags.length > 0 && (u.trust_score || 100) < 50) {
            // Only alert once per UAV (use a flag on the knownUavIds)
            if (knownUavIds[u.identifier] !== "spoofed") {
                knownUavIds[u.identifier] = "spoofed";
                Audio.play("spoofing");
            }
        }
    });

    // Detect threat level escalation
    var currentLevel = (lastThreat || {}).threat_level || "LOW";
    var levels = { LOW: 0, MODERATE: 1, HIGH: 2, CRITICAL: 3 };
    if ((levels[currentLevel] || 0) > (levels[prevThreatLevel] || 0)) {
        if (currentLevel === "CRITICAL") {
            Audio.play("critical");
        }
    }
    prevThreatLevel = currentLevel;

    // Track pending alert count for title
    prevPendingAlerts = (stats.alerts || {}).pending_alerts || 0;
}

// ─── SIDEBAR BADGES ───
function updateSidebarBadges(uavs, stats) {
    var pending = (stats.alerts || {}).pending_alerts || 0;

    // Alert badge
    var alertBadge = document.getElementById("sb-badge-alerts");
    if (alertBadge) {
        if (pending > 0) {
            alertBadge.textContent = pending > 99 ? "99+" : pending;
            alertBadge.className = "sb-badge visible alert";
        } else {
            alertBadge.className = "sb-badge";
        }
    }

    // UAV Fleet badge
    var fleetBadge = document.getElementById("sb-badge-fleet");
    if (fleetBadge) {
        if (uavs.length > 0) {
            fleetBadge.textContent = uavs.length;
            fleetBadge.className = "sb-badge visible info";
        } else {
            fleetBadge.className = "sb-badge";
        }
    }
}

// ─── PAGE TITLE WITH ALERT COUNT ───
function updatePageTitle(stats) {
    var pending = (stats.alerts || {}).pending_alerts || 0;
    document.title = pending > 0
        ? "(" + pending + ") skylens \u2014 Command Center"
        : "skylens \u2014 Command Center";
}

// ─── SETTINGS MODAL ───
var settingsData = null;

function initSettings() {
    var btn = document.getElementById("hdr-settings-btn");
    var modal = document.getElementById("settings-modal");
    var closeBtn = document.getElementById("settings-close");

    if (btn && modal) {
        btn.addEventListener("click", function() {
            showSettingsModal();
        });

        closeBtn.addEventListener("click", function() {
            modal.style.display = "none";
        });

        modal.addEventListener("click", function(e) {
            if (e.target === modal) {
                modal.style.display = "none";
            }
        });
    }
}

function showSettingsModal() {
    var modal = document.getElementById("settings-modal");
    if (!modal) return;

    modal.style.display = "flex";
    updateSettingsUI();
}

function updateSettingsUI() {
    // Use cached status data
    if (!settingsData) return;

    // Node info
    setText("settings-node-name", settingsData.node_name || "-");
    setText("settings-node-uuid", settingsData.node_uuid || "-");
    setText("settings-zmq-port", settingsData.zmq_port || "-");
    setText("settings-version", settingsData.version || "-");
}

function setText(id, text) {
    var el = document.getElementById(id);
    if (el) el.textContent = text;
}

function hideEl(id) {
    var el = document.getElementById(id);
    if (el) el.style.display = "none";
}

function showError(id, msg) {
    var el = document.getElementById(id);
    if (el) {
        el.textContent = msg;
        el.style.display = "block";
    }
}

function showSuccess(id, msg) {
    var el = document.getElementById(id);
    if (el) {
        el.textContent = msg;
        el.style.display = "block";
    }
}

// Store settings data when poll receives it
function storeSettingsData(d) {
    settingsData = d;
}

// ─── START ───
document.addEventListener("DOMContentLoaded", function() {
    init();
    initSettings();
});

})();
