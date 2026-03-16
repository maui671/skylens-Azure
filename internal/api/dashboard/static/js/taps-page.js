/* ═══════════════════════════════════════════════════════
   SKYLENS COMMAND CENTER — TAPS PAGE JS
   Polls /api/taps, renders tap summary KPIs and
   detailed per-tap cards with health scoring
   FIXED: Stable DOM updates prevent flashing/jumping
   ═══════════════════════════════════════════════════════ */

(function () {
"use strict";

var POLL_MS = 1000;        // Reduced frequency to prevent flicker
var LIVE_THRESHOLD = 60;   // seconds — heartbeat considered live
var STALE_WARN     = 120;  // seconds — heartbeat considered critical/offline

var lastData = null;
var pollFails = 0;
var tapCards = {};         // tap_uuid -> DOM element (for stable updates)
var tapOrder = [];         // ordered list of tap_uuids (prevents reordering)

// Cached DOM escape element
var _escDiv = document.createElement("div");
function esc(s) {
    _escDiv.textContent = s;
    return _escDiv.innerHTML;
}

// ─── INIT ───
function init() {
    tick();
    poll();
    setInterval(poll, POLL_MS);
    setInterval(tick, 1000);
}

// ─── CLOCK ───
function tick() {
    var now = new Date();
    var hh = String(now.getHours()).padStart(2, "0");
    var mm = String(now.getMinutes()).padStart(2, "0");
    var ss = String(now.getSeconds()).padStart(2, "0");
    var el = document.getElementById("hdr-clock");
    if (el) el.textContent = hh + ":" + mm + ":" + ss;
}

// ─── POLL ───
function poll() {
    fetch("/api/taps")
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (data) {
            pollFails = 0;
            // /api/taps returns a flat array of tap heartbeat objects
            var taps = Array.isArray(data) ? data : (data.taps || []);
            // Sort taps by name for consistent ordering (prevents jumping)
            taps.sort(function(a, b) {
                var nameA = (a.tap_name || a.tap_uuid || "").toLowerCase();
                var nameB = (b.tap_name || b.tap_uuid || "").toLowerCase();
                return nameA.localeCompare(nameB);
            });
            lastData = taps;
            setConnected(true);
            render(taps);
            if (typeof SkylensAuth !== 'undefined') SkylensAuth.revealPage();
        })
        .catch(function () {
            pollFails++;
            if (pollFails > 2) setConnected(false);
        });
    // Lightweight badge poll
    fetch("/api/status").then(function(r){return r.json()}).then(_updateSbBadges).catch(function(){});
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
function render(taps) {
    renderSummary(taps);
    renderTapCards(taps);
}

// ─── SUMMARY KPIs ───
function renderSummary(taps) {
    var total = taps.length;
    var online = 0;
    var totalFrames = 0;
    var totalDetections = 0;
    var totalPPS = 0;

    for (var i = 0; i < taps.length; i++) {
        var t = taps[i];
        var age = tapAge(t.timestamp);
        if (age < LIVE_THRESHOLD) online++;

        // Use correct Go API field names
        totalFrames += (t.frames_total || t.packets_captured || 0);
        totalDetections += (t.detections_sent || 0);
        totalPPS += (t.packets_per_second || 0);
    }

    setKPI("kpi-total-taps", total);
    setKPI("kpi-online-taps", online);
    setKPI("kpi-total-frames", fmtNum(totalFrames));
    setKPI("kpi-avg-parse", fmtNum(totalDetections) + " det");
    setKPI("kpi-total-errors", totalPPS.toFixed(1) + " pps");

    var badge = document.getElementById("tap-count-badge");
    if (badge) badge.textContent = total;

    // Color the online count
    var onlineEl = document.getElementById("kpi-online-taps");
    if (onlineEl) {
        onlineEl.style.color = (total > 0 && online === 0)
            ? "var(--critical)"
            : (online < total ? "var(--warning)" : "");
    }
}

function setKPI(id, val) {
    var el = document.getElementById(id);
    if (el) el.textContent = val;
}

// ─── TAP DETAIL CARDS (STABLE DOM UPDATES) ───
function renderTapCards(taps) {
    var grid = document.getElementById("tap-detail-grid");
    var empty = document.getElementById("tap-empty");
    if (!grid) return;

    if (!taps.length) {
        // Clear all cards
        Object.keys(tapCards).forEach(function(id) {
            if (tapCards[id] && tapCards[id].parentNode) {
                tapCards[id].parentNode.removeChild(tapCards[id]);
            }
        });
        tapCards = {};
        tapOrder = [];
        if (empty) empty.style.display = "";
        return;
    }
    if (empty) empty.style.display = "none";

    // Build a set of current tap IDs
    var currentIds = {};
    taps.forEach(function(t) {
        var id = t.tap_uuid || t.tap_name || ("tap-" + Math.random());
        currentIds[id] = t;
    });

    // Remove cards for taps that no longer exist
    Object.keys(tapCards).forEach(function(id) {
        if (!currentIds[id]) {
            if (tapCards[id] && tapCards[id].parentNode) {
                tapCards[id].parentNode.removeChild(tapCards[id]);
            }
            delete tapCards[id];
            var orderIdx = tapOrder.indexOf(id);
            if (orderIdx > -1) tapOrder.splice(orderIdx, 1);
        }
    });

    // Update or create cards (maintaining order)
    taps.forEach(function(t) {
        var id = t.tap_uuid || t.tap_name || ("tap-" + Math.random());

        if (tapCards[id]) {
            // UPDATE existing card in-place (no DOM rebuild = no flicker)
            updateTapCard(tapCards[id], t);
        } else {
            // CREATE new card
            var card = createTapCard(t);
            tapCards[id] = card;
            tapOrder.push(id);
            grid.appendChild(card);
        }
    });
}

// Create a new tap card DOM element
function createTapCard(t) {
    var card = document.createElement("div");
    card.className = "tap-detail-card";
    card.innerHTML = buildTapCardInner(t);

    // Attach command handlers
    var tapId = t.tap_uuid || t.tap_name || t.id;
    attachCardHandlers(card, tapId);

    return card;
}

// Update an existing tap card in-place (prevents flicker)
function updateTapCard(card, t) {
    var age = tapAge(t.timestamp);
    var live = age < LIVE_THRESHOLD;
    var health = calcHealth(t, age);

    // Check if BLE section needs to appear (TAP started BLE after card was created)
    var hasBLE = t.ble_scanning || (t.ble_advertisements || 0) > 0 ||
                 (t.ble_detections || 0) > 0 || (t.ble_interface && t.ble_interface !== "");
    var hasBLESection = card.querySelector(".tap-ble-section") !== null;
    if (hasBLE && !hasBLESection) {
        // Rebuild card HTML to include BLE section, re-attach handlers
        card.innerHTML = buildTapCardInner(t);
        var tapId = t.tap_uuid || t.tap_name || t.id;
        attachCardHandlers(card, tapId);
        return;
    }

    // Update health bar class
    var healthBar = card.querySelector(".tap-health-bar");
    if (healthBar) {
        healthBar.className = "tap-health-bar " + health;
    }

    // Update status dot
    var dot = card.querySelector(".tap-detail-dot");
    if (dot) {
        dot.className = "tap-detail-dot " + (live ? "live" : "stale");
    }

    // Update name
    var nameEl = card.querySelector(".tap-detail-name");
    if (nameEl) {
        var dotHtml = '<span class="tap-detail-dot ' + (live ? "live" : "stale") + '"></span>';
        nameEl.innerHTML = dotHtml + esc(t.tap_name || "Unnamed Tap");
    }

    // Update version
    var verEl = card.querySelector(".tap-detail-ver");
    if (verEl) verEl.textContent = "v" + (t.version || "?");

    // Update age text
    var ageEl = card.querySelector(".tap-detail-age");
    if (ageEl) {
        var ageText = fmtDuration(age);
        var ageClass = age > STALE_WARN ? "bad" : (age >= LIVE_THRESHOLD ? "warn" : "good");
        ageEl.className = "tap-detail-age " + ageClass;
        ageEl.textContent = "Last seen " + ageText + " ago";
    }

    // Update all field values (pass age for offline detection)
    var fields = card.querySelectorAll(".tap-detail-field");
    fields.forEach(function(fieldEl) {
        var label = fieldEl.querySelector(".tap-detail-field-label");
        var val = fieldEl.querySelector(".tap-detail-field-val");
        if (!label || !val) return;

        var labelText = label.textContent;
        var newVal = getFieldValue(t, labelText, age);
        var newClass = getFieldClass(t, labelText, age);

        if (val.textContent !== newVal) {
            val.textContent = newVal;
        }
        var baseClass = "tap-detail-field-val";
        if (newClass) baseClass += " " + newClass;
        if (val.className !== baseClass) {
            val.className = baseClass;
        }
    });

    // Update location
    var locVal = card.querySelector(".tap-detail-location-val");
    if (locVal) {
        var hasLoc = t.latitude != null && t.longitude != null;
        if (hasLoc) {
            locVal.textContent = t.latitude.toFixed(6) + ", " + t.longitude.toFixed(6) + "  |  " + MGRS.forward(t.latitude, t.longitude, 4);
            locVal.className = "tap-detail-location-val";
        } else {
            locVal.textContent = "No location";
            locVal.className = "tap-detail-location-val none";
        }
    }
}

// Get field value based on label (accepts optional age for offline detection)
function getFieldValue(t, label, age) {
    var isOffline = age != null && age > STALE_WARN;
    switch (label) {
        case "Channel":
            return t.current_channel != null ? String(t.current_channel) : (t.channel != null ? String(t.channel) : "--");
        case "Capture":
            return isOffline ? "Offline" : (t.capture_running ? "Running" : "Stopped");
        case "Frames":
        case "Frames Total":
            return fmtNum(t.frames_total || t.packets_captured || 0);
        case "Filtered":
            return fmtNum(t.packets_filtered || 0);
        case "Detections":
            return fmtNum(t.detections_sent || 0);
        case "Rate":
            return (t.packets_per_second != null ? t.packets_per_second.toFixed(1) : "0") + " pps";
        case "CPU":
        case "CPU %":
            return t.cpu_percent != null ? t.cpu_percent.toFixed(1) + "%" : "--";
        case "Memory":
        case "Mem %":
            return t.memory_percent != null ? t.memory_percent.toFixed(1) + "%" : "--";
        case "Temp":
            return t.temperature != null ? t.temperature.toFixed(1) + "\u00B0C" : "\u2014";
        case "Uptime":
            return t.tap_uptime != null ? fmtDuration(t.tap_uptime) : "\u2014";
        case "Version":
            return "v" + (t.version || "?");
        case "Capture Status":
            return isOffline ? "Offline" : (t.capture_running ? "Running" : "Stopped");
        // BLE fields
        case "Status":
            return t.ble_scanning ? "Scanning" : "Off";
        case "Advertisements":
            return fmtNum(t.ble_advertisements || 0);
        case "BLE Detections":
            return fmtNum(t.ble_detections || 0);
        case "Interface":
            return t.ble_interface || "--";
        default:
            return "--";
    }
}

// Get field CSS class based on label and value (accepts optional age for offline detection)
function getFieldClass(t, label, age) {
    var isOffline = age != null && age > STALE_WARN;
    switch (label) {
        case "Capture":
        case "Capture Status":
            return isOffline ? "bad" : (t.capture_running ? "good" : "bad");
        case "CPU":
        case "CPU %":
            return colorClass(t.cpu_percent, 70, 85);
        case "Memory":
        case "Mem %":
            return colorClass(t.memory_percent, 70, 85);
        case "Temp":
            return colorClass(t.temperature, 70, 85);
        case "Detections":
            return (t.detections_sent || 0) > 0 ? "good" : "";
        // BLE fields
        case "Status":
            return t.ble_scanning ? "good" : "";
        case "BLE Detections":
            return (t.ble_detections || 0) > 0 ? "good" : "";
        default:
            return "";
    }
}

// Build inner HTML for tap card (used for initial creation)
function buildTapCardInner(t) {
    var age = tapAge(t.timestamp);
    var live = age < LIVE_THRESHOLD;

    // ── Health score calculation ──
    var health = calcHealth(t, age);

    // ── UUID (truncated to first 8 chars) ──
    var uuid = t.tap_uuid || "unknown";
    var uuidShort = uuid.length > 8 ? uuid.substring(0, 8) + "\u2026" : uuid;

    // ── Connection age with color coding ──
    var ageText = fmtDuration(age);
    var ageClass = age > STALE_WARN ? "bad" : (age >= LIVE_THRESHOLD ? "warn" : "good");

    // ── CPU metrics ──
    var cpuPct = t.cpu_percent != null ? t.cpu_percent.toFixed(1) + "%" : "--";
    var cpuPctVal = t.cpu_percent != null ? t.cpu_percent : null;

    // ── Memory metrics ──
    var memPct = t.memory_percent != null ? t.memory_percent.toFixed(1) + "%" : "--";
    var memPctVal = t.memory_percent != null ? t.memory_percent : null;

    // ── Temperature ──
    var temp = t.temperature != null ? t.temperature.toFixed(1) + "\u00B0C" : "\u2014";
    var tempVal = t.temperature != null ? t.temperature : null;

    // ── Network / capture (use Go API field names) ──
    var channel = t.current_channel != null ? t.current_channel : (t.channel != null ? t.channel : "--");
    var framesTotal = t.frames_total || t.packets_captured || 0;
    var packetsFiltered = t.packets_filtered || 0;
    var detectionsSent = t.detections_sent || 0;
    var pps = t.packets_per_second != null ? t.packets_per_second.toFixed(1) : "0";
    var uptime = t.tap_uptime != null ? fmtDuration(t.tap_uptime) : "\u2014";

    // ── Color classes using spec thresholds: green <70%, yellow 70-85%, red >85% ──
    var cpuClass  = colorClass(cpuPctVal, 70, 85);
    var memClass  = colorClass(memPctVal, 70, 85);
    var tempClass = colorClass(tempVal, 70, 85);

    // If tap is offline (stale), don't show "Running" - show "Offline"
    var isOffline = age > STALE_WARN;
    var captureClass = isOffline ? "bad" : (t.capture_running ? "good" : "bad");
    var captureText  = isOffline ? "Offline" : (t.capture_running ? "Running" : "Stopped");

    // ── Location ──
    var hasLocation = t.latitude != null && t.longitude != null;
    var locationHtml = hasLocation
        ? '<span class="tap-detail-location-val">' + t.latitude.toFixed(6) + ", " + t.longitude.toFixed(6) + '  |  ' + MGRS.forward(t.latitude, t.longitude, 4) + '</span>'
        : '<span class="tap-detail-location-val none">No location</span>';

    // ── BLE stats ──
    var bleScanning = t.ble_scanning || false;
    var bleAdverts = t.ble_advertisements || 0;
    var bleDets = t.ble_detections || 0;
    var hasBLE = bleScanning || bleAdverts > 0 || bleDets > 0 || (t.ble_interface && t.ble_interface !== "");

    return '<div class="tap-health-bar ' + health + '"></div>' +
        '<div class="tap-detail-body">' +
            // Header
            '<div class="tap-detail-hdr">' +
                '<div class="tap-detail-name">' +
                    '<span class="tap-detail-dot ' + (live ? "live" : "stale") + '"></span>' +
                    esc(t.tap_name || "Unnamed Tap") +
                '</div>' +
                '<span class="tap-detail-ver">v' + esc(t.version || "?") + '</span>' +
            '</div>' +
            // UUID + heartbeat age row
            '<div class="tap-detail-meta">' +
                '<span class="tap-detail-uuid" title="' + esc(uuid) + '">' + esc(uuidShort) + '</span>' +
                '<span class="tap-detail-age ' + ageClass + '">Last seen ' + ageText + ' ago</span>' +
            '</div>' +
            // Two-column sections (Capture + System + optional BLE)
            '<div class="tap-detail-sections">' +
                // WiFi Capture section
                '<div class="tap-detail-section">' +
                    '<div class="tap-detail-section-title">WiFi Capture</div>' +
                    field("Channel", String(channel), "") +
                    field("Capture", captureText, captureClass) +
                    field("Frames", fmtNum(framesTotal), "") +
                    field("Filtered", fmtNum(packetsFiltered), "") +
                    field("Detections", fmtNum(detectionsSent), detectionsSent > 0 ? "good" : "") +
                    field("Rate", pps + " pps", "") +
                '</div>' +
                // System section
                '<div class="tap-detail-section">' +
                    '<div class="tap-detail-section-title">System</div>' +
                    field("CPU", cpuPct, cpuClass) +
                    field("Memory", memPct, memClass) +
                    field("Temp", temp, tempClass) +
                    field("Uptime", uptime, "") +
                    field("Version", "v" + esc(t.version || "?"), "") +
                '</div>' +
            '</div>' +
            // BLE section (only shown if TAP has BLE capability)
            (hasBLE ? '<div class="tap-detail-sections tap-ble-section">' +
                '<div class="tap-detail-section" style="flex:1">' +
                    '<div class="tap-detail-section-title">BLE Scanner</div>' +
                    field("Status", bleScanning ? "Scanning" : "Off", bleScanning ? "good" : "") +
                    field("Advertisements", fmtNum(bleAdverts), "") +
                    field("BLE Detections", fmtNum(bleDets), bleDets > 0 ? "good" : "") +
                    (t.ble_interface ? field("Interface", esc(t.ble_interface), "") : "") +
                '</div>' +
            '</div>' : "") +
            // Location row
            '<div class="tap-detail-location">' +
                '<span class="tap-detail-location-label">&#9906; Location</span>' +
                locationHtml +
            '</div>' +
            // Action buttons
            '<div class="tap-detail-actions">' +
                '<button class="tap-cmd-btn tap-cmd-ping" title="Send ping to test latency">Ping</button>' +
                '<button class="tap-cmd-btn tap-cmd-config" title="Edit TAP configuration">Config</button>' +
                '<button class="tap-cmd-btn tap-cmd-restart danger" title="Restart tap service">Restart</button>' +
            '</div>' +
        '</div>';
}

function field(label, val, cls) {
    return '<div class="tap-detail-field">' +
        '<span class="tap-detail-field-label">' + label + '</span>' +
        '<span class="tap-detail-field-val' + (cls ? " " + cls : "") + '">' + val + '</span>' +
    '</div>';
}

// ─── COLOR CLASS HELPER ───
// green <low, yellow low..high, red >high
function colorClass(val, low, high) {
    if (val == null) return "";
    if (val > high) return "bad";
    if (val >= low) return "warn";
    return "good";
}

// ─── HEALTH CALCULATION ───
// Returns "good", "warn", or "critical"
function calcHealth(t, age) {
    // Critical: capture not running or heartbeat very stale
    if (!t.capture_running) return "critical";
    if (age > STALE_WARN) return "critical";

    var warnings = 0;

    if (age >= LIVE_THRESHOLD) warnings++;

    var cpuVal = t.cpu_percent != null ? t.cpu_percent : null;
    if (cpuVal != null && cpuVal >= 70) warnings++;
    if (cpuVal != null && cpuVal > 85) return "critical";

    var memVal = t.memory_percent != null ? t.memory_percent : null;
    if (memVal != null && memVal >= 70) warnings++;
    if (memVal != null && memVal > 85) return "critical";

    var tempVal = t.temperature != null ? t.temperature : null;
    if (tempVal != null && tempVal >= 70) warnings++;
    if (tempVal != null && tempVal > 85) return "critical";

    if (warnings > 0) return "warn";
    return "good";
}

// ─── UTILS ───
function tapAge(ts) {
    if (!ts) return 999;
    return (Date.now() - new Date(ts).getTime()) / 1000;
}

function fmtNum(n) {
    if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
    if (n >= 1000) return (n / 1000).toFixed(1) + "K";
    return String(n);
}

function fmtDuration(secs) {
    secs = Math.floor(secs);
    if (secs < 0) secs = 0;
    if (secs < 60) return secs + "s";
    if (secs < 3600) return Math.floor(secs / 60) + "m " + (secs % 60) + "s";
    var h = Math.floor(secs / 3600);
    var m = Math.floor((secs % 3600) / 60);
    if (h >= 24) {
        var d = Math.floor(h / 24);
        h = h % 24;
        return d + "d " + h + "h " + m + "m";
    }
    return h + "h " + m + "m";
}

// ─── SIDEBAR BADGES ───
function _updateSbBadges(d) {
    if (!d) return;
    var p = ((d.stats || {}).alerts || {}).pending_alerts || 0;
    var rawUavs = d.uavs || [];
    var _sc = false; try { var _v = localStorage.getItem("skylens_show_controllers"); _sc = _v !== null ? JSON.parse(_v) === true : false; } catch(e) {}
    var u = _sc ? rawUavs.length : rawUavs.filter(function(x) { return x.is_controller !== true; }).length;
    var ab = document.getElementById("sb-badge-alerts");
    var fb = document.getElementById("sb-badge-fleet");
    if (ab) { ab.textContent = p > 99 ? "99+" : p; ab.className = "sb-badge" + (p > 0 ? " visible alert" : ""); }
    if (fb) { fb.textContent = u; fb.className = "sb-badge" + (u > 0 ? " visible info" : ""); }
}

// ═══════════════════════════════════════════════════════
// TAP COMMANDS — Ping, Restart, Channel Config
// ═══════════════════════════════════════════════════════

// Show result toast
function showCmdResult(message, isSuccess) {
    var existing = document.querySelector(".tap-cmd-result");
    if (existing) existing.remove();

    var toast = document.createElement("div");
    toast.className = "tap-cmd-result " + (isSuccess ? "success" : "error");
    toast.textContent = message;
    document.body.appendChild(toast);

    setTimeout(function() {
        toast.style.opacity = "0";
        toast.style.transform = "translateX(100%)";
        setTimeout(function() { toast.remove(); }, 300);
    }, 3000);
}

// Ping tap
function pingTap(tapId, btn) {
    if (!tapId) return;
    btn.classList.add("loading");
    btn.textContent = "...";

    fetch("/api/tap/" + encodeURIComponent(tapId) + "/ping", { method: "POST" })
        .then(function(r) { return r.json(); })
        .then(function(d) {
            btn.classList.remove("loading");
            if (d.ok) {
                btn.classList.add("success");
                btn.textContent = d.latency_ms ? d.latency_ms.toFixed(1) + "ms" : "OK";
                showCmdResult("Ping to " + tapId + ": " + (d.latency_ms ? d.latency_ms.toFixed(1) + "ms" : "OK"), true);
            } else {
                btn.classList.add("error");
                btn.textContent = "FAIL";
                showCmdResult("Ping failed: " + (d.error || "Unknown error"), false);
            }
            setTimeout(function() {
                btn.classList.remove("success", "error");
                btn.textContent = "Ping";
            }, 2000);
        })
        .catch(function(e) {
            btn.classList.remove("loading");
            btn.classList.add("error");
            btn.textContent = "ERR";
            showCmdResult("Ping failed: " + e.message, false);
            setTimeout(function() {
                btn.classList.remove("error");
                btn.textContent = "Ping";
            }, 2000);
        });
}

// Restart tap
function restartTap(tapId, btn, graceful) {
    if (!tapId) return;
    if (!confirm("Restart tap " + tapId + "?\n\nThis will briefly interrupt detection.")) return;

    btn.classList.add("loading");
    btn.textContent = "...";

    fetch("/api/tap/" + encodeURIComponent(tapId) + "/restart", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ graceful: graceful !== false })
    })
        .then(function(r) { return r.json(); })
        .then(function(d) {
            btn.classList.remove("loading");
            if (d.ok) {
                btn.classList.add("success");
                btn.textContent = "SENT";
                showCmdResult("Restart command sent to " + tapId, true);
            } else {
                btn.classList.add("error");
                btn.textContent = "FAIL";
                showCmdResult("Restart failed: " + (d.error || "Unknown error"), false);
            }
            setTimeout(function() {
                btn.classList.remove("success", "error");
                btn.textContent = "Restart";
            }, 2000);
        })
        .catch(function(e) {
            btn.classList.remove("loading");
            btn.classList.add("error");
            btn.textContent = "ERR";
            showCmdResult("Restart failed: " + e.message, false);
            setTimeout(function() {
                btn.classList.remove("error");
                btn.textContent = "Restart";
            }, 2000);
        });
}

// Attach event handlers to tap card
function attachCardHandlers(card, tapId) {
    // Ping button
    var pingBtn = card.querySelector(".tap-cmd-ping");
    if (pingBtn) {
        pingBtn.onclick = function(e) {
            e.preventDefault();
            pingTap(tapId, pingBtn);
        };
    }

    // Config button
    var configBtn = card.querySelector(".tap-cmd-config");
    if (configBtn) {
        configBtn.onclick = function(e) {
            e.preventDefault();
            openTapConfig(tapId);
        };
    }

    // Restart button
    var restartBtn = card.querySelector(".tap-cmd-restart");
    if (restartBtn) {
        restartBtn.onclick = function(e) {
            e.preventDefault();
            restartTap(tapId, restartBtn, true);
        };
    }
}

// ═══════════════════════════════════════════════════════
// TAP CONFIG MODAL
// ═══════════════════════════════════════════════════════

var _configTapId = null;

function openTapConfig(tapId) {
    _configTapId = tapId;
    var bg = document.getElementById("tap-config-bg");
    var modal = document.getElementById("tap-config-modal");
    var nameEl = document.getElementById("tap-config-name");

    if (nameEl) nameEl.textContent = tapId;

    // Fetch current config from API
    fetch("/api/tap/" + encodeURIComponent(tapId) + "/config")
        .then(function(r) { return r.json(); })
        .then(function(d) {
            if (d.ok && d.config) {
                var cfg = d.config;
                if (cfg.tap_name) nameEl.textContent = cfg.tap_name;

                // Populate channels
                var chEl = document.getElementById("cfg-channels");
                if (chEl) {
                    if (cfg.channels && cfg.channels.length > 0) {
                        chEl.value = cfg.channels.join(",");
                    } else if (cfg.current_channel) {
                        chEl.value = String(cfg.current_channel);
                    } else {
                        chEl.value = "";
                    }
                }

                // Populate hop interval
                var hopEl = document.getElementById("cfg-hop");
                var hopVal = document.getElementById("cfg-hop-val");
                if (hopEl) {
                    var hop = cfg.hop_interval_ms || 200;
                    hopEl.value = hop;
                    if (hopVal) hopVal.textContent = hop + "ms";
                }

                // Populate BLE
                var bleEl = document.getElementById("cfg-ble");
                if (bleEl) bleEl.checked = !!cfg.ble_enabled;

                // Populate log level
                var logEl = document.getElementById("cfg-loglevel");
                if (logEl && cfg.log_level) logEl.value = cfg.log_level;
            }
        })
        .catch(function(e) {
            console.warn("Failed to fetch tap config:", e);
        });

    // Show modal
    if (bg) bg.classList.add("open");
    if (modal) modal.classList.add("open");
}

function closeTapConfig() {
    _configTapId = null;
    var bg = document.getElementById("tap-config-bg");
    var modal = document.getElementById("tap-config-modal");
    if (bg) bg.classList.remove("open");
    if (modal) modal.classList.remove("open");
}

function saveTapConfig() {
    if (!_configTapId) return;

    var saveBtn = document.getElementById("cfg-save-btn");
    if (saveBtn) {
        saveBtn.classList.add("saving");
        saveBtn.textContent = "Saving...";
    }

    // Read form values
    var channelsRaw = (document.getElementById("cfg-channels") || {}).value || "";
    var channels = channelsRaw.split(",")
        .map(function(s) { return parseInt(s.trim(), 10); })
        .filter(function(n) { return !isNaN(n) && n > 0; });

    var hopInterval = parseInt((document.getElementById("cfg-hop") || {}).value, 10) || 200;
    var bleEnabled = !!(document.getElementById("cfg-ble") || {}).checked;
    var logLevel = (document.getElementById("cfg-loglevel") || {}).value || "info";

    var payload = {
        channels: channels,
        hop_interval_ms: hopInterval,
        ble_enabled: bleEnabled,
        log_level: logLevel
    };

    var tapId = _configTapId;

    fetch("/api/tap/" + encodeURIComponent(tapId) + "/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
    })
        .then(function(r) { return r.json(); })
        .then(function(d) {
            if (saveBtn) {
                saveBtn.classList.remove("saving");
                saveBtn.textContent = "Save";
            }

            if (d.ok) {
                showCmdResult(d.message || "Config sent to " + tapId, true);
                closeTapConfig();

                // Prompt to restart
                if (confirm("Restart TAP to apply changes?")) {
                    fetch("/api/tap/" + encodeURIComponent(tapId) + "/restart", {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ graceful: true })
                    })
                        .then(function(r) { return r.json(); })
                        .then(function(rd) {
                            if (rd.ok) {
                                showCmdResult("Restart command sent to " + tapId, true);
                            } else {
                                showCmdResult("Restart failed: " + (rd.error || "Unknown"), false);
                            }
                        })
                        .catch(function(e) {
                            showCmdResult("Restart failed: " + e.message, false);
                        });
                }
            } else {
                showCmdResult("Config failed: " + (d.error || "Unknown error"), false);
            }
        })
        .catch(function(e) {
            if (saveBtn) {
                saveBtn.classList.remove("saving");
                saveBtn.textContent = "Save";
            }
            showCmdResult("Config failed: " + e.message, false);
        });
}

// Close modal on backdrop click
(function() {
    var bg = document.getElementById("tap-config-bg");
    if (bg) bg.addEventListener("click", closeTapConfig);
})();

// Close modal on Escape key
document.addEventListener("keydown", function(e) {
    if (e.key === "Escape" && _configTapId) closeTapConfig();
});

// Expose config functions globally for onclick handlers
window.openTapConfig = openTapConfig;
window.closeTapConfig = closeTapConfig;
window.saveTapConfig = saveTapConfig;

// ─── START ───
document.addEventListener("DOMContentLoaded", init);

})();
