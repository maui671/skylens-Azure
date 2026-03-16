/* ===================================================================
   SKYLENS COMMAND CENTER -- ALERTS PAGE JS
   Polls /api/alerts, renders alert management table
   with filtering, sorting, and acknowledge support
   =================================================================== */

(function () {
"use strict";

var POLL_MS = 1000;  // Reduced from 500ms to prevent flicker
var ACK_STORAGE_KEY = "skylens_acked_alerts";

var lastData = null;
var pollFails = 0;
var allAlerts = [];
var filteredAlerts = [];

// Filter / sort state
var filterPriority = "ALL";
var filterType = "";
var filterSearch = "";
var sortCol = "timestamp";
var sortAsc = false;

// Priority weight for sorting (lower = higher priority)
var PRI_WEIGHT = { CRITICAL: 0, HIGH: 1, MEDIUM: 2, INFO: 3 };

// Stable DOM caching for alert rows (prevents flashing)
var alertRows = {};  // alertKey -> DOM element

// --- INIT ---
function init() {
    tick();
    poll();
    setInterval(poll, POLL_MS);
    setInterval(tick, 1000);
    bindFilters();
    bindSort();
    bindAckAll();
}

// --- CLOCK ---
function tick() {
    var now = new Date();
    var hh = String(now.getHours()).padStart(2, "0");
    var mm = String(now.getMinutes()).padStart(2, "0");
    var ss = String(now.getSeconds()).padStart(2, "0");
    var el = document.getElementById("hdr-clock");
    if (el) el.textContent = hh + ":" + mm + ":" + ss;
}

// --- POLL ---
function poll() {
    fetch("/api/alerts")
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (data) {
            pollFails = 0;
            setConnected(true);
            processAlerts(data);
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
    if (el) {
        el.innerHTML = ok
            ? '<span class="hdr-dot green"></span> Connected'
            : '<span class="hdr-dot red"></span> Disconnected';
    }
    var sb = document.getElementById("sb-node-status");
    if (sb) {
        sb.innerHTML = ok
            ? '<span class="sb-dot green"></span><span>Node Online</span>'
            : '<span class="sb-dot red"></span><span>Node Offline</span>';
    }
}

// --- PROCESS ALERTS ---
function processAlerts(alertsArray) {
    // /api/alerts returns a plain array of alert objects
    var raw = Array.isArray(alertsArray) ? alertsArray : [];
    var acked = getAcked();

    // Merge server-side acknowledged with localStorage acknowledged state.
    // Each alert keeps its acknowledged status: true if the server says so OR
    // if the user has locally acknowledged it.
    allAlerts = raw.map(function (a) {
        var key = alertKey(a);
        var localAck = acked[key];
        // Clone to avoid mutating the original
        var alert = {};
        for (var k in a) {
            if (a.hasOwnProperty(k)) alert[k] = a[k];
        }
        if (localAck !== undefined) {
            alert.acknowledged = !!localAck;
        }
        return alert;
    });

    // Update header subtitle
    var node = document.getElementById("hdr-node");
    if (node) {
        var unacked = allAlerts.filter(function (a) { return !a.acknowledged; }).length;
        node.textContent = allAlerts.length + " alert(s), " + unacked + " unacknowledged";
    }

    // Populate type dropdown with unique types
    populateTypeDropdown(allAlerts);

    // Apply filters and render
    applyFilters();
}

// --- TYPE DROPDOWN ---
function populateTypeDropdown(alerts) {
    var sel = document.getElementById("filter-type");
    if (!sel) return;

    var types = {};
    alerts.forEach(function (a) {
        var t = a.alert_type || "unknown";
        types[t] = true;
    });

    var current = sel.value;
    var sorted = Object.keys(types).sort();

    // Only rebuild if types changed
    var existing = [];
    for (var i = 1; i < sel.options.length; i++) {
        existing.push(sel.options[i].value);
    }
    if (existing.join(",") === sorted.join(",")) {
        return;
    }

    sel.innerHTML = '<option value="">All Types</option>';
    sorted.forEach(function (t) {
        var opt = document.createElement("option");
        opt.value = t;
        opt.textContent = t.replace(/_/g, " ");
        sel.appendChild(opt);
    });

    // Restore selection
    if (current) sel.value = current;
}

// --- FILTERS ---
function bindFilters() {
    // Priority pills
    var pills = document.getElementById("filter-pills");
    if (pills) {
        pills.addEventListener("click", function (e) {
            var btn = e.target.closest(".filter-pill");
            if (!btn) return;
            pills.querySelectorAll(".filter-pill").forEach(function (p) {
                p.classList.remove("active");
            });
            btn.classList.add("active");
            filterPriority = btn.getAttribute("data-pri");
            applyFilters();
        });
    }

    // Type dropdown
    var typeSel = document.getElementById("filter-type");
    if (typeSel) {
        typeSel.addEventListener("change", function () {
            filterType = typeSel.value;
            applyFilters();
        });
    }

    // Search input
    var searchInput = document.getElementById("filter-search");
    if (searchInput) {
        searchInput.addEventListener("input", function () {
            filterSearch = searchInput.value.toLowerCase().trim();
            applyFilters();
        });
    }
}

function applyFilters() {
    filteredAlerts = allAlerts.filter(function (a) {
        // Priority filter
        if (filterPriority !== "ALL" && (a.priority || "INFO") !== filterPriority) {
            return false;
        }
        // Type filter
        if (filterType && (a.alert_type || "") !== filterType) {
            return false;
        }
        // Text search on identifier and message
        if (filterSearch) {
            var haystack = [
                a.uav_identifier || "",
                a.message || ""
            ].join(" ").toLowerCase();
            if (haystack.indexOf(filterSearch) === -1) {
                return false;
            }
        }
        return true;
    });

    applySorting();
    renderSummary();
    renderTable();
}

// --- SORTING ---
function bindSort() {
    var thead = document.querySelector("#alerts-full-tbl thead");
    if (!thead) return;

    thead.addEventListener("click", function (e) {
        var th = e.target.closest("th[data-sort]");
        if (!th) return;

        var col = th.getAttribute("data-sort");
        if (sortCol === col) {
            sortAsc = !sortAsc;
        } else {
            sortCol = col;
            sortAsc = true;
        }

        // Update header visual state
        thead.querySelectorAll("th").forEach(function (h) {
            h.classList.remove("sorted");
            var arrow = h.querySelector(".sort-arrow");
            if (arrow) arrow.textContent = "\u25B2";
        });
        th.classList.add("sorted");
        var arrow = th.querySelector(".sort-arrow");
        if (arrow) arrow.textContent = sortAsc ? "\u25B2" : "\u25BC";

        applySorting();
        renderTable();
    });
}

function applySorting() {
    filteredAlerts.sort(function (a, b) {
        var va, vb;

        switch (sortCol) {
            case "priority":
                va = PRI_WEIGHT[a.priority] !== undefined ? PRI_WEIGHT[a.priority] : 99;
                vb = PRI_WEIGHT[b.priority] !== undefined ? PRI_WEIGHT[b.priority] : 99;
                break;
            case "type":
                va = (a.alert_type || "").toLowerCase();
                vb = (b.alert_type || "").toLowerCase();
                break;
            case "uav":
                va = (a.uav_identifier || "").toLowerCase();
                vb = (b.uav_identifier || "").toLowerCase();
                break;
            case "message":
                va = (a.message || "").toLowerCase();
                vb = (b.message || "").toLowerCase();
                break;
            case "timestamp":
            default:
                va = new Date(a.timestamp || 0).getTime();
                vb = new Date(b.timestamp || 0).getTime();
                break;
        }

        var cmp;
        if (typeof va === "number" && typeof vb === "number") {
            cmp = va - vb;
        } else {
            va = String(va);
            vb = String(vb);
            cmp = va < vb ? -1 : va > vb ? 1 : 0;
        }

        return sortAsc ? cmp : -cmp;
    });
}

// --- SUMMARY CARDS ---
function renderSummary() {
    var total = allAlerts.length;
    var unacked = 0;
    var critical = 0;
    var last24h = 0;
    var now = Date.now();
    var dayMs = 86400000;

    allAlerts.forEach(function (a) {
        if (!a.acknowledged) unacked++;
        if ((a.priority || "INFO") === "CRITICAL") critical++;
        var ts = new Date(a.timestamp || 0).getTime();
        if (now - ts < dayMs) last24h++;
    });

    setSummary("sum-total", total);
    setSummary("sum-unacked", unacked);
    setSummary("sum-critical", critical);
    setSummary("sum-24h", last24h);

    // Update badge
    var badge = document.getElementById("alert-count-badge");
    if (badge) {
        badge.textContent = unacked;
        badge.className = "badge" +
            (critical > 0 ? " crit" :
             allAlerts.some(function (a) { return a.priority === "HIGH" && !a.acknowledged; }) ? " warn" : "");
    }
}

function setSummary(id, val) {
    var el = document.getElementById(id);
    if (!el) return;
    var v = el.querySelector(".summary-val");
    if (v) v.textContent = val;
}

// --- RENDER TABLE (Stable DOM Updates) ---
function renderTable() {
    var body = document.getElementById("alerts-body");
    var empty = document.getElementById("alerts-empty");
    if (!body) return;

    // Build set of current alert keys for removal detection
    var currentKeys = {};
    filteredAlerts.forEach(function(a) {
        var key = alertKey(a);
        if (key) currentKeys[key] = true;
    });

    // Remove rows for alerts no longer in filtered list
    Object.keys(alertRows).forEach(function(key) {
        if (!currentKeys[key]) {
            var row = alertRows[key];
            if (row && row.parentNode) {
                row.parentNode.removeChild(row);
            }
            delete alertRows[key];
        }
    });

    if (!filteredAlerts.length) {
        if (empty) {
            empty.style.display = "";
            if (allAlerts.length > 0) {
                empty.textContent = "No alerts match current filters";
            } else {
                empty.textContent = "No alerts \u2014 airspace clear";
            }
        }
        return;
    }
    if (empty) empty.style.display = "none";

    // Preserve scroll position in the table wrapper
    var wrapper = body.closest(".full-tbl-wrap");
    var scrollTop = wrapper ? wrapper.scrollTop : 0;

    // Update or create rows (stable DOM updates)
    filteredAlerts.forEach(function(a) {
        var key = alertKey(a);
        if (!key) return;

        if (alertRows[key]) {
            // Update existing row in place
            updateAlertRow(alertRows[key], a);
        } else {
            // Create new row
            var row = createAlertRow(a);
            alertRows[key] = row;
            body.appendChild(row);
        }
    });

    // Reorder rows to match sorted order
    filteredAlerts.forEach(function(a) {
        var key = alertKey(a);
        if (key && alertRows[key]) {
            body.appendChild(alertRows[key]);
        }
    });

    // Restore scroll position
    if (wrapper) wrapper.scrollTop = scrollTop;
}

// Create a new alert row DOM element
function createAlertRow(a) {
    var row = document.createElement("tr");
    updateAlertRow(row, a);
    return row;
}

// Update alert row content in place
function updateAlertRow(row, a) {
    var pri = (a.priority || "INFO").toUpperCase();
    var priLow = pri.toLowerCase();
    var priClass = priLow === "critical" ? "crit"
                 : priLow === "high" ? "high"
                 : priLow === "medium" ? "med"
                 : "info";
    var type = (a.alert_type || "").replace(/_/g, " ");
    var uav = a.uav_identifier || "---";
    var msg = a.message || "";
    var timestamp = a.timestamp || "";
    var key = alertKey(a);
    var isAcked = !!a.acknowledged;
    var ackBadgeClass = isAcked ? "ack-yes" : "ack-no";
    var ackLabel = isAcked ? "ACK" : "NEW";
    var ackBtnLabel = isAcked ? "UNACK" : "ACK";

    row.setAttribute("data-key", key);
    row.className = isAcked ? "acked-row" : "";

    row.innerHTML =
        '<td><span class="pri-badge ' + priClass + '">' + esc(pri) + '</span></td>' +
        '<td class="alert-type">' + esc(type) + '</td>' +
        '<td class="alert-uav">' + esc(uav) + '</td>' +
        '<td class="alert-msg" title="' + escAttr(msg) + '">' + esc(truncate(msg, 80)) + '</td>' +
        '<td class="alert-time" title="' + escAttr(timestamp) + '">' + esc(timeAgo(timestamp)) + '</td>' +
        '<td><span class="ack-badge ' + ackBadgeClass + '">' + ackLabel + '</span></td>' +
        '<td><button class="btn-ack-sm" data-ack-key="' + escAttr(key) + '">' + ackBtnLabel + '</button></td>';
}

// --- ACKNOWLEDGE ---
function getAcked() {
    try {
        return JSON.parse(localStorage.getItem(ACK_STORAGE_KEY) || "{}");
    } catch (e) {
        return {};
    }
}

function setAcked(obj) {
    try {
        localStorage.setItem(ACK_STORAGE_KEY, JSON.stringify(obj));
    } catch (e) { /* storage full or unavailable */ }
}

function alertKey(a) {
    // Build a stable key from alert properties
    return (a.alert_type || "") + "|" +
           (a.priority || "") + "|" +
           (a.uav_identifier || "") + "|" +
           (a.timestamp || "");
}

function toggleAck(key) {
    var acked = getAcked();
    // Find the alert with this key
    var alert = null;
    for (var i = 0; i < allAlerts.length; i++) {
        if (alertKey(allAlerts[i]) === key) {
            alert = allAlerts[i];
            break;
        }
    }
    if (!alert) return;

    // Toggle the acknowledged state
    var newState = !alert.acknowledged;
    acked[key] = newState;
    setAcked(acked);

    // Update in-memory state
    alert.acknowledged = newState;

    // Re-render
    applyFilters();
}

// Use event delegation on the table body instead of inline onclick
function bindTableActions() {
    var body = document.getElementById("alerts-body");
    if (!body) return;
    body.addEventListener("click", function (e) {
        var btn = e.target.closest("button[data-ack-key]");
        if (!btn) return;
        var key = btn.getAttribute("data-ack-key");
        if (key) toggleAck(key);
    });
}

function bindAckAll() {
    var btn = document.getElementById("btn-ack-all");
    if (!btn) return;

    btn.addEventListener("click", function () {
        if (!filteredAlerts.length) return;

        var acked = getAcked();
        filteredAlerts.forEach(function (a) {
            var key = alertKey(a);
            acked[key] = true;
            a.acknowledged = true;
        });
        setAcked(acked);

        applyFilters();
    });

    // Also bind table body click delegation
    bindTableActions();
}

// --- UTILS ---
var _escEl = document.createElement("div");

function esc(s) {
    if (s === null || s === undefined) return "";
    _escEl.textContent = String(s);
    return _escEl.innerHTML;
}

function escAttr(s) {
    // Escape for use inside HTML attribute values (double-quoted)
    if (s === null || s === undefined) return "";
    return String(s)
        .replace(/&/g, "&amp;")
        .replace(/"/g, "&quot;")
        .replace(/'/g, "&#39;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;");
}

function truncate(s, maxLen) {
    if (!s) return "";
    if (s.length <= maxLen) return s;
    return s.substring(0, maxLen) + "\u2026";
}

function timeAgo(ts) {
    if (!ts) return "---";
    var then = new Date(ts).getTime();
    if (isNaN(then)) return "---";
    var diff = (Date.now() - then) / 1000;
    if (diff < 5) return "just now";
    if (diff < 60) return Math.floor(diff) + "s ago";
    if (diff < 3600) return Math.floor(diff / 60) + "m ago";
    if (diff < 86400) return Math.floor(diff / 3600) + "h" + Math.floor((diff % 3600) / 60) + "m ago";
    var days = Math.floor(diff / 86400);
    var hours = Math.floor((diff % 86400) / 3600);
    return days + "d" + hours + "h ago";
}

// --- SIDEBAR BADGES ---
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

// --- START ---
document.addEventListener("DOMContentLoaded", init);

})();
