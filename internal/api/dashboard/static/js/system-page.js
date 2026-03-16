/* ═══════════════════════════════════════════════════════
   SKYLENS COMMAND CENTER — SYSTEM PAGE JS
   Operations-center dashboard with node banner, health KPIs,
   sensor monitoring, and real-time activity tracking.
   FIXED: Stable DOM updates, correct API field names
   ═══════════════════════════════════════════════════════ */

(function () {
"use strict";

var POLL_MS = 1000;  // Reduced poll rate for stability
var MAX_EVENTS = 100;
var CHART_POINTS = 60;

var lastData = null;
var pollFails = 0;
var events = [];
var msgHistory = [];          // { ts, drones, taps, frames }
var chartCanvas = null;
var chartCtx = null;
var eventSeq = 0;
var lastRenderedSeq = -1;
var lastRenderedFilter = "all";
var eventFilter = "all";

// Stable DOM caching for sensor cards
var sensorCards = {};  // tap_uuid -> DOM element
var sensorOrder = [];  // ordered list of tap_uuids

// ─── INIT ───
function init() {
    chartCanvas = document.getElementById("activity-canvas");
    if (chartCanvas) {
        chartCtx = chartCanvas.getContext("2d");
        resizeCanvas();
        window.addEventListener("resize", resizeCanvas);
    }

    // Event filter tabs
    var tabs = document.querySelectorAll(".evt-tab");
    for (var i = 0; i < tabs.length; i++) {
        tabs[i].addEventListener("click", handleTabClick);
    }

    tick();
    poll();
    setInterval(poll, POLL_MS);
    setInterval(tick, 1000);

    addEvent("info", "System page initialized");
}

function handleTabClick() {
    eventFilter = this.getAttribute("data-filter");
    var allTabs = document.querySelectorAll(".evt-tab");
    for (var j = 0; j < allTabs.length; j++) {
        allTabs[j].classList.remove("active");
    }
    this.classList.add("active");
    lastRenderedSeq = -1;
    renderEventLog();
}

function resizeCanvas() {
    if (!chartCanvas) return;
    var rect = chartCanvas.parentElement.getBoundingClientRect();
    chartCanvas.width = rect.width - 24;
    chartCanvas.height = 160;
    drawChart();
}

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
    fetch("/api/status")
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (data) {
            var wasDisconnected = pollFails > 2;
            pollFails = 0;
            if (wasDisconnected) addEvent("info", "Connection restored to node API");
            detectEvents(data);
            lastData = data;
            setConnected(true);
            render(data);
            trackActivity(data);
            if (typeof SkylensAuth !== 'undefined') SkylensAuth.revealPage();
        })
        .catch(function () {
            pollFails++;
            if (pollFails > 2) setConnected(false);
            if (pollFails === 3) addEvent("error", "Lost connection to node API");
        });
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

    // Update banner status indicator
    var dot = document.querySelector(".nb-dot");
    var txt = document.querySelector(".nb-status-text");
    if (dot) {
        if (ok) { dot.classList.remove("offline"); }
        else { dot.classList.add("offline"); }
    }
    if (txt) {
        txt.textContent = ok ? "ONLINE" : "OFFLINE";
        if (ok) { txt.classList.remove("offline"); }
        else { txt.classList.add("offline"); }
    }
}

// ─── RENDER ───
function render(d) {
    var s = d.stats || {};
    var taps = d.taps || [];
    renderHeader(d);
    renderNodeBanner(d);
    renderKPIs(d, taps);
    renderSensors(taps);
    renderSystemStats(d, taps);
    renderEventLog();
    drawChart();
    _updateSbBadges(d);
}

// ─── HEADER ───
function renderHeader(d) {
    var node = document.getElementById("hdr-node");
    if (node) node.textContent = d.node_name || "skylens-node";

    var up = document.getElementById("hdr-uptime");
    if (up && d.uptime) up.textContent = "UP " + d.uptime;

    var sbVer = document.querySelector(".sb-version");
    if (sbVer && d.version) sbVer.textContent = "v" + d.version;

    var clock = document.getElementById("hdr-clock");
    if (clock && d.server_time) {
        var m = d.server_time.match(/T(\d{2}:\d{2}:\d{2})/);
        if (m) clock.textContent = m[1] + " UTC";
    }
}

// ─── NODE BANNER ───
function renderNodeBanner(d) {
    var nameEl = document.getElementById("nb-name");
    var metaEl = document.getElementById("nb-meta");
    if (!nameEl || !metaEl) return;

    nameEl.textContent = d.node_name || "Skylens Node";

    var html = "";
    if (d.version) {
        html += '<span class="nb-meta-item"><span class="nb-meta-label">VER</span> v' + esc(d.version) + '</span>';
    }
    if (d.node_uuid) {
        var uid = d.node_uuid;
        var uidShort = uid.length > 8 ? uid.substring(0, 8) + "\u2026" : uid;
        html += '<span class="nb-meta-item" title="' + esc(uid) + '"><span class="nb-meta-label">UUID</span> ' + esc(uidShort) + '</span>';
    }
    if (d.zmq_port) {
        html += '<span class="nb-meta-item"><span class="nb-meta-label">ZMQ</span> :' + esc(String(d.zmq_port)) + '</span>';
    }
    if (d.uptime) {
        html += '<span class="nb-meta-item"><span class="nb-meta-label">UP</span> ' + esc(d.uptime) + '</span>';
    }
    metaEl.innerHTML = html;
}

// ─── KPI ROW ───
function renderKPIs(d, taps) {
    var s = d.stats || {};

    // Calculate frame rate from taps — only include live taps in aggregates
    var totalPPS = 0;
    var totalFrames = 0;
    var liveTaps = 0;

    for (var i = 0; i < taps.length; i++) {
        var t = taps[i];
        if (tapAge(t.timestamp) < 60) {
            liveTaps++;
            totalPPS += (t.packets_per_second || 0);
            totalFrames += (t.frames_total || t.packets_captured || 0);
        }
    }

    // Update individual KPIs (in-place to avoid flicker)
    updateKPI("kpi-msg-rate", totalPPS.toFixed(1) + "/s", totalPPS > 0 ? "var(--safe)" : "var(--t3)");
    updateKPI("kpi-active-drones", fmtNum(s.drones_active || 0), "var(--accent)");
    updateKPI("kpi-active-sensors", liveTaps + " / " + taps.length, liveTaps === taps.length && taps.length > 0 ? "var(--safe)" : liveTaps > 0 ? "var(--caution)" : "var(--critical)");
    updateKPI("kpi-total-frames", fmtNum(totalFrames), "var(--info)");
    updateKPI("kpi-low-trust", fmtNum(s.low_trust_count || 0), (s.low_trust_count || 0) > 0 ? "var(--critical)" : "var(--safe)");
    updateKPI("kpi-total-taps", fmtNum(s.taps_total || taps.length), "var(--info)");
}

function updateKPI(id, val, color) {
    var el = document.getElementById(id);
    if (!el) return;
    var valEl = el.querySelector(".sys-kpi-val");
    var barEl = el.querySelector(".sys-kpi-bar");
    if (valEl) {
        if (valEl.textContent !== val) {
            valEl.textContent = val;
        }
        valEl.style.color = color;
    }
    if (barEl) {
        barEl.style.background = color;
    }
}

// ─── SENSOR CARDS (STABLE DOM UPDATES) ───
function renderSensors(taps) {
    var grid = document.getElementById("sensor-grid");
    var badge = document.getElementById("sensor-count");
    if (!grid) return;

    if (badge) badge.textContent = taps.length;

    if (!taps.length) {
        // Clear all cards
        Object.keys(sensorCards).forEach(function(id) {
            if (sensorCards[id] && sensorCards[id].parentNode) {
                sensorCards[id].parentNode.removeChild(sensorCards[id]);
            }
        });
        sensorCards = {};
        sensorOrder = [];
        grid.innerHTML = '<div class="sys-sensor-empty">No sensors connected</div>';
        return;
    }

    // Remove empty state if present
    var emptyEl = grid.querySelector(".sys-sensor-empty");
    if (emptyEl) emptyEl.remove();

    // Sort taps by name for consistent ordering
    taps.sort(function(a, b) {
        var nameA = (a.tap_name || a.tap_uuid || "").toLowerCase();
        var nameB = (b.tap_name || b.tap_uuid || "").toLowerCase();
        return nameA.localeCompare(nameB);
    });

    // Build current tap IDs set
    var currentIds = {};
    taps.forEach(function(t) {
        var id = t.tap_uuid || t.tap_name || ("tap-" + Math.random());
        currentIds[id] = t;
    });

    // Remove cards for taps that no longer exist
    Object.keys(sensorCards).forEach(function(id) {
        if (!currentIds[id]) {
            if (sensorCards[id] && sensorCards[id].parentNode) {
                sensorCards[id].parentNode.removeChild(sensorCards[id]);
            }
            delete sensorCards[id];
            var orderIdx = sensorOrder.indexOf(id);
            if (orderIdx > -1) sensorOrder.splice(orderIdx, 1);
        }
    });

    // Update or create cards
    taps.forEach(function(t) {
        var id = t.tap_uuid || t.tap_name || ("tap-" + Math.random());

        if (sensorCards[id]) {
            // UPDATE existing card in-place
            updateSensorCard(sensorCards[id], t);
        } else {
            // CREATE new card
            var card = createSensorCard(t);
            sensorCards[id] = card;
            sensorOrder.push(id);
            grid.appendChild(card);
        }
    });
}

function createSensorCard(t) {
    var card = document.createElement("div");
    card.className = "sys-sensor-card";
    card.innerHTML = buildSensorCardInner(t);
    return card;
}

function buildSensorCardInner(t) {
    var age = tapAge(t.timestamp);
    var status, statusClass;
    if (age < 60) { status = "LIVE"; statusClass = "live"; }
    else if (age < 120) { status = "STALE"; statusClass = "stale"; }
    else { status = "OFFLINE"; statusClass = "offline"; }

    var cpu = t.cpu_percent != null ? t.cpu_percent : null;
    var mem = t.memory_percent != null ? t.memory_percent : null;
    var temp = t.temperature != null ? t.temperature : null;

    var frames = fmtNum(t.frames_total || t.packets_captured || 0);
    var ch = t.current_channel != null ? "CH " + t.current_channel : (t.channel != null ? "CH " + t.channel : "--");
    var pps = t.packets_per_second != null ? t.packets_per_second.toFixed(1) + " pps" : "0 pps";
    var uptime = t.tap_uptime != null ? fmtDuration(t.tap_uptime) : "--";

    return '<div class="sys-sensor-hdr">' +
            '<span class="sys-sensor-name">' + esc(t.tap_name || "Unnamed") + '</span>' +
            '<span class="sys-sensor-badge ' + statusClass + '">' + status + '</span>' +
        '</div>' +
        miniBar("CPU", cpu, 100) +
        miniBar("MEM", mem, 100) +
        miniBar("TMP", temp, 100) +
        '<div class="sys-sensor-stats">' +
            '<span class="sys-stat-frames">' + frames + ' frames</span>' +
            '<span class="sys-stat-ch">' + esc(ch) + '</span>' +
            '<span class="sys-stat-pps">' + pps + '</span>' +
            '<span class="sys-stat-uptime">' + esc(uptime) + '</span>' +
        '</div>';
}

function updateSensorCard(card, t) {
    var age = tapAge(t.timestamp);
    var status, statusClass;
    if (age < 60) { status = "LIVE"; statusClass = "live"; }
    else if (age < 120) { status = "STALE"; statusClass = "stale"; }
    else { status = "OFFLINE"; statusClass = "offline"; }

    // Update name
    var nameEl = card.querySelector(".sys-sensor-name");
    if (nameEl) nameEl.textContent = t.tap_name || "Unnamed";

    // Update status badge
    var badgeEl = card.querySelector(".sys-sensor-badge");
    if (badgeEl) {
        badgeEl.textContent = status;
        badgeEl.className = "sys-sensor-badge " + statusClass;
    }

    // Update bars
    updateMiniBar(card, "CPU", t.cpu_percent, 100);
    updateMiniBar(card, "MEM", t.memory_percent, 100);
    updateMiniBar(card, "TMP", t.temperature, 100);

    // Update stats
    var framesEl = card.querySelector(".sys-stat-frames");
    if (framesEl) framesEl.textContent = fmtNum(t.frames_total || t.packets_captured || 0) + " frames";

    var chEl = card.querySelector(".sys-stat-ch");
    if (chEl) {
        var ch = t.current_channel != null ? "CH " + t.current_channel : (t.channel != null ? "CH " + t.channel : "--");
        chEl.textContent = ch;
    }

    var ppsEl = card.querySelector(".sys-stat-pps");
    if (ppsEl) ppsEl.textContent = (t.packets_per_second != null ? t.packets_per_second.toFixed(1) : "0") + " pps";

    var uptimeEl = card.querySelector(".sys-stat-uptime");
    if (uptimeEl) uptimeEl.textContent = t.tap_uptime != null ? fmtDuration(t.tap_uptime) : "--";
}

function updateMiniBar(card, label, val, max) {
    var rows = card.querySelectorAll(".sys-bar-row");
    for (var i = 0; i < rows.length; i++) {
        var labelEl = rows[i].querySelector(".sys-bar-label");
        if (labelEl && labelEl.textContent === label) {
            var fillEl = rows[i].querySelector(".sys-bar-fill");
            var valEl = rows[i].querySelector(".sys-bar-val");

            if (val == null) {
                if (fillEl) fillEl.style.width = "0";
                if (valEl) valEl.textContent = "--";
            } else {
                var pct = Math.min((val / max) * 100, 100);
                var color = pct < 70 ? "var(--safe)" : pct < 85 ? "var(--caution)" : "var(--critical)";
                var display = label === "TMP" ? val.toFixed(0) + "\u00B0" : val.toFixed(0) + "%";

                if (fillEl) {
                    fillEl.style.width = pct.toFixed(1) + "%";
                    fillEl.style.background = color;
                }
                if (valEl) valEl.textContent = display;
            }
            break;
        }
    }
}

function miniBar(label, val, max) {
    if (val == null) {
        return '<div class="sys-bar-row">' +
            '<span class="sys-bar-label">' + label + '</span>' +
            '<div class="sys-bar-track"><div class="sys-bar-fill" style="width:0"></div></div>' +
            '<span class="sys-bar-val">--</span>' +
        '</div>';
    }
    var pct = Math.min((val / max) * 100, 100);
    var color = pct < 70 ? "var(--safe)" : pct < 85 ? "var(--caution)" : "var(--critical)";
    var display = label === "TMP" ? val.toFixed(0) + "\u00B0" : val.toFixed(0) + "%";

    return '<div class="sys-bar-row">' +
        '<span class="sys-bar-label">' + label + '</span>' +
        '<div class="sys-bar-track">' +
            '<div class="sys-bar-fill" style="width:' + pct.toFixed(1) + '%;background:' + color + '"></div>' +
        '</div>' +
        '<span class="sys-bar-val">' + display + '</span>' +
    '</div>';
}

// ─── SYSTEM STATS (simplified - shows what we actually have) ───
function renderSystemStats(d, taps) {
    var el = document.getElementById("pipeline-flow");
    if (!el) return;

    var s = d.stats || {};
    var uavs = d.uavs || [];

    // Calculate totals from taps
    var totalFrames = 0;
    var totalFiltered = 0;
    var totalDetections = 0;
    var totalPPS = 0;

    taps.forEach(function(t) {
        totalFrames += (t.frames_total || t.packets_captured || 0);
        totalFiltered += (t.packets_filtered || 0);
        totalDetections += (t.detections_sent || 0);
        totalPPS += (t.packets_per_second || 0);
    });

    // UAV stats
    var activeUavs = 0;
    var lostUavs = 0;
    var lowTrust = 0;
    var hostileCount = 0;

    uavs.forEach(function(u) {
        if (u._contactStatus === "lost" || u.status === "lost") {
            lostUavs++;
        } else {
            activeUavs++;
        }
        if ((u.trust_score || 100) < 50) lowTrust++;
        if (u.classification === "HOSTILE") hostileCount++;
    });

    el.innerHTML =
        // INTAKE column
        '<div class="pf-column">' +
            '<div class="pf-col-header">INTAKE</div>' +
            stageCard("Tap Network", taps.length, "sensors online",
                taps.length > 0 ? "ok" : "warn", [
                    { l: "Frames",     v: totalFrames },
                    { l: "Filtered",   v: totalFiltered },
                    { l: "Rate",       v: totalPPS.toFixed(1) + " pps", isStr: true }
                ]) +
            stageCard("Detection", totalDetections, "detections sent",
                totalDetections > 0 ? "ok" : "warn", [
                    { l: "From Taps",  v: taps.length },
                    { l: "Avg/Tap",    v: taps.length > 0 ? (totalDetections / taps.length).toFixed(0) : 0 }
                ]) +
        '</div>' +
        // PROCESSING column
        '<div class="pf-column">' +
            '<div class="pf-col-header">PROCESSING</div>' +
            stageCard("UAV Tracking", activeUavs, "active contacts",
                activeUavs > 0 ? "ok" : "warn", [
                    { l: "Active",    v: activeUavs },
                    { l: "Lost",      v: lostUavs, warn: lostUavs > 0 },
                    { l: "Total",     v: s.drones_total || uavs.length }
                ]) +
            stageCard("Trust Analysis", lowTrust, "low trust contacts",
                lowTrust > 0 ? "err" : "ok", [
                    { l: "Hostile",   v: hostileCount, err: hostileCount > 0 },
                    { l: "Low Trust", v: lowTrust, warn: lowTrust > 0 },
                    { l: "Analyzed",  v: uavs.length }
                ]) +
        '</div>' +
        // OUTPUT column
        '<div class="pf-column">' +
            '<div class="pf-col-header">OUTPUT</div>' +
            stageCard("Alerts", (s.alerts || {}).pending_alerts || 0, "pending alerts",
                ((s.alerts || {}).pending_alerts || 0) > 10 ? "warn" : "ok", [
                    { l: "Pending",   v: (s.alerts || {}).pending_alerts || 0, warn: ((s.alerts || {}).pending_alerts || 0) > 10 },
                    { l: "Generated", v: (s.alerts || {}).alerts_generated || 0 }
                ]) +
            stageCard("System Health", pollFails === 0 ? 100 : 0, pollFails === 0 ? "healthy" : "degraded",
                pollFails === 0 ? "ok" : "err", [
                    { l: "API",       v: pollFails === 0 ? "OK" : "FAIL", isStr: true, err: pollFails > 0 },
                    { l: "Taps",      v: taps.length > 0 ? "OK" : "NONE", isStr: true, warn: taps.length === 0 },
                    { l: "Uptime",    v: d.uptime || "--", isStr: true }
                ]) +
        '</div>';
}

function stageCard(name, hero, heroLabel, health, stats) {
    var heroDisplay = typeof hero === "number" ? fmtNum(hero) : hero;

    var html = '<div class="pf-stage">' +
        '<div class="pf-stage-hdr">' +
            '<span class="pf-stage-name">' + esc(name) + '</span>' +
            '<span class="pf-badge ' + health + '">' + health.toUpperCase() + '</span>' +
        '</div>' +
        '<div class="pf-hero">' + heroDisplay + '</div>' +
        '<div class="pf-hero-label">' + esc(heroLabel) + '</div>';

    if (stats && stats.length) {
        html += '<div class="pf-stats">';
        for (var i = 0; i < stats.length; i++) {
            var st = stats[i];
            var cls = st.err ? " error" : st.warn ? " warn" : "";
            var val = st.isStr ? st.v : fmtNum(st.v);
            html += '<div class="pf-stat">' +
                '<span class="pf-stat-label">' + esc(st.l) + '</span>' +
                '<span class="pf-stat-val' + cls + '">' + val + '</span>' +
            '</div>';
        }
        html += '</div>';
    }

    html += '</div>';
    return html;
}

// ─── ACTIVITY TRACKING ───
function trackActivity(d) {
    var taps = d.taps || [];
    var uavs = d.uavs || [];

    // Calculate total frames from taps
    var totalFrames = 0;
    taps.forEach(function(t) {
        totalFrames += (t.frames_total || t.packets_captured || 0);
    });

    var now = Date.now();
    msgHistory.push({
        ts: now,
        frames: totalFrames,
        drones: uavs.length,
        taps: taps.length
    });

    while (msgHistory.length > CHART_POINTS + 1) msgHistory.shift();

    // Update current rate display
    var rateEl = document.getElementById("chart-rate");
    if (rateEl && msgHistory.length >= 2) {
        var prev = msgHistory[msgHistory.length - 2];
        var curr = msgHistory[msgHistory.length - 1];
        var dtSec = (curr.ts - prev.ts) / 1000;
        if (dtSec > 0) {
            var rate = ((curr.frames - prev.frames) / dtSec).toFixed(1);
            rateEl.textContent = rate + " frames/s";
        }
    }
}

// ─── ACTIVITY CHART (SPARKLINE) ───
function drawChart() {
    if (!chartCtx || !chartCanvas) return;

    var w = chartCanvas.width;
    var h = chartCanvas.height;
    var ctx = chartCtx;

    ctx.clearRect(0, 0, w, h);

    // Build rates array from history
    var rates = [];
    for (var i = 1; i < msgHistory.length; i++) {
        var dt = (msgHistory[i].ts - msgHistory[i - 1].ts) / 1000;
        if (dt > 0) {
            rates.push((msgHistory[i].frames - msgHistory[i - 1].frames) / dt);
        } else {
            rates.push(0);
        }
    }

    // Pad to CHART_POINTS
    while (rates.length < CHART_POINTS) {
        rates.unshift(0);
    }

    var maxRate = Math.max.apply(null, rates);
    if (maxRate < 1) maxRate = 1;

    // Draw horizontal grid lines
    ctx.strokeStyle = "rgba(42, 56, 50, 0.5)";
    ctx.lineWidth = 1;
    for (var g = 0; g < 4; g++) {
        var gy = Math.round(h * (g / 4)) + 0.5;
        ctx.beginPath();
        ctx.moveTo(0, gy);
        ctx.lineTo(w, gy);
        ctx.stroke();
    }

    // Draw vertical grid (every 10 seconds)
    for (var v = 0; v < CHART_POINTS; v += 10) {
        var vx = Math.round((v / (CHART_POINTS - 1)) * w) + 0.5;
        ctx.beginPath();
        ctx.moveTo(vx, 0);
        ctx.lineTo(vx, h);
        ctx.stroke();
    }

    if (rates.length < 2) return;

    // Draw fill gradient
    var gradient = ctx.createLinearGradient(0, 0, 0, h);
    gradient.addColorStop(0, "rgba(76, 175, 80, 0.25)");
    gradient.addColorStop(1, "rgba(76, 175, 80, 0.02)");

    ctx.beginPath();
    ctx.moveTo(0, h);
    for (var fi = 0; fi < rates.length; fi++) {
        var fx = (fi / (rates.length - 1)) * w;
        var fy = h - (rates[fi] / maxRate) * (h - 8) - 4;
        ctx.lineTo(fx, fy);
    }
    ctx.lineTo(w, h);
    ctx.closePath();
    ctx.fillStyle = gradient;
    ctx.fill();

    // Draw line
    ctx.beginPath();
    ctx.strokeStyle = "#4CAF50";
    ctx.lineWidth = 2;
    ctx.lineJoin = "round";
    ctx.lineCap = "round";

    for (var li = 0; li < rates.length; li++) {
        var lx = (li / (rates.length - 1)) * w;
        var ly = h - (rates[li] / maxRate) * (h - 8) - 4;
        if (li === 0) {
            ctx.moveTo(lx, ly);
        } else {
            ctx.lineTo(lx, ly);
        }
    }
    ctx.stroke();

    // Draw glow on the last point
    if (rates.length > 0) {
        var lastX = w;
        var lastY = h - (rates[rates.length - 1] / maxRate) * (h - 8) - 4;
        ctx.beginPath();
        ctx.arc(lastX, lastY, 3, 0, Math.PI * 2);
        ctx.fillStyle = "#00E676";
        ctx.fill();
        ctx.beginPath();
        ctx.arc(lastX, lastY, 6, 0, Math.PI * 2);
        ctx.fillStyle = "rgba(0, 230, 118, 0.3)";
        ctx.fill();
    }
}

// ─── EVENT DETECTION ───
function detectEvents(data) {
    if (!lastData) return;

    var s = data.stats || {};
    var ls = lastData.stats || {};
    var taps = data.taps || [];
    var lastTaps = lastData.taps || [];
    var uavs = data.uavs || [];
    var lastUavs = lastData.uavs || [];

    // Detect new taps
    var lastTapIds = {};
    lastTaps.forEach(function (t) { lastTapIds[t.tap_uuid] = t; });
    taps.forEach(function (t) {
        if (!lastTapIds[t.tap_uuid]) {
            addEvent("info", "New tap connected: " + (t.tap_name || t.tap_uuid));
        }
    });

    // Detect lost taps
    var curTapIds = {};
    taps.forEach(function (t) { curTapIds[t.tap_uuid] = t; });
    lastTaps.forEach(function (t) {
        if (!curTapIds[t.tap_uuid]) {
            addEvent("warn", "Tap disconnected: " + (t.tap_name || t.tap_uuid));
        }
    });

    // Detect new drones
    var lastUavIds = {};
    lastUavs.forEach(function (u) { lastUavIds[u.identifier || u.mac] = u; });
    uavs.forEach(function (u) {
        var id = u.identifier || u.mac;
        if (id && !lastUavIds[id]) {
            addEvent("info", "New drone detected: " + (u.designation || id));
        }
    });

    // Detect lost drones
    var curUavIds = {};
    uavs.forEach(function (u) { curUavIds[u.identifier || u.mac] = u; });
    lastUavs.forEach(function (u) {
        var id = u.identifier || u.mac;
        if (id && !curUavIds[id]) {
            addEvent("info", "Drone lost: " + (u.designation || id));
        }
    });

    // Detect hostile contacts
    uavs.forEach(function (u) {
        var id = u.identifier || u.mac;
        var lastU = lastUavIds[id];
        if (u.classification === "HOSTILE" && (!lastU || lastU.classification !== "HOSTILE")) {
            addEvent("error", "Hostile contact: " + (u.designation || id));
        }
    });

    // Detect low trust
    uavs.forEach(function (u) {
        var id = u.identifier || u.mac;
        var lastU = lastUavIds[id];
        var trust = u.trust_score || 100;
        var lastTrust = lastU ? (lastU.trust_score || 100) : 100;
        if (trust < 50 && lastTrust >= 50) {
            addEvent("warn", "Low trust warning: " + (u.designation || id) + " (" + trust + "%)");
        }
    });
}

// ─── EVENT LOG ───
function addEvent(level, message) {
    var now = new Date();
    var ts = String(now.getHours()).padStart(2, "0") + ":" +
             String(now.getMinutes()).padStart(2, "0") + ":" +
             String(now.getSeconds()).padStart(2, "0");

    events.push({ ts: ts, msg: message, level: level });
    eventSeq++;

    while (events.length > MAX_EVENTS) {
        events.shift();
    }
}

function renderEventLog() {
    var wrap = document.getElementById("event-log");
    var badge = document.getElementById("event-count");
    if (!wrap) return;

    // Filter events
    var filtered = eventFilter === "all"
        ? events
        : events.filter(function (e) { return e.level === eventFilter; });

    if (badge) badge.textContent = filtered.length;

    if (!filtered.length) {
        wrap.innerHTML = '<div class="event-log-empty">' +
            (events.length ? "No " + eventFilter.toUpperCase() + " events" : "Waiting for events...") +
        '</div>';
        return;
    }

    // Skip rebuild if nothing changed
    if (eventSeq === lastRenderedSeq && eventFilter === lastRenderedFilter) return;
    lastRenderedSeq = eventSeq;
    lastRenderedFilter = eventFilter;

    var html = "";
    for (var i = 0; i < filtered.length; i++) {
        var e = filtered[i];
        html += '<div class="event-log-row">' +
            '<span class="event-log-ts">' + esc(e.ts) + '</span>' +
            '<span class="evt-severity ' + e.level + '">' + e.level.toUpperCase() + '</span>' +
            '<span class="event-log-msg">' + esc(e.msg) + '</span>' +
        '</div>';
    }

    wrap.innerHTML = html;
    wrap.scrollTop = wrap.scrollHeight;
}

// ─── UTILS ───
function fmtNum(n) {
    if (typeof n !== "number" || isNaN(n)) return "0";
    if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
    if (n >= 10000) return (n / 1000).toFixed(1) + "K";
    if (Number.isInteger(n)) return n.toLocaleString("en-US");
    return n.toFixed(2);
}

function tapAge(ts) {
    if (!ts) return 999;
    return (Date.now() - new Date(ts).getTime()) / 1000;
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
        return d + "d " + h + "h";
    }
    return h + "h " + m + "m";
}

var _escDiv = document.createElement("div");
function esc(s) {
    _escDiv.textContent = s;
    return _escDiv.innerHTML;
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

// ─── START ───
document.addEventListener("DOMContentLoaded", init);

})();
