/* =====================================================
   SKYLENS COMMAND CENTER -- SETTINGS PAGE JS
   Manages display preferences, notifications, thresholds,
   system info display, and test data management.
   ===================================================== */

(function () {
"use strict";

// ─── STORAGE KEYS ───
var STORAGE_PREFIX = "skylens_";
var KEYS = {
    pollInterval:     STORAGE_PREFIX + "poll_interval",
    compactMode:      STORAGE_PREFIX + "compact_mode",
    autoRefresh:      STORAGE_PREFIX + "auto_refresh",
    spoofThreshold:   STORAGE_PREFIX + "spoof_threshold",
    authGrace:        STORAGE_PREFIX + "auth_grace",
    staleTimeout:     STORAGE_PREFIX + "stale_timeout",
    theme:            STORAGE_PREFIX + "theme",
    mapCenterLat:     STORAGE_PREFIX + "map_center_lat",
    mapCenterLng:     STORAGE_PREFIX + "map_center_lng",
    mapZoom:          STORAGE_PREFIX + "map_zoom",
    mapStyle:         STORAGE_PREFIX + "map_style",
    rangeRingRadius:  STORAGE_PREFIX + "range_ring_radius",
    trailLength:      STORAGE_PREFIX + "trail_length",
    soundAlerts:      STORAGE_PREFIX + "sound_alerts",
    showControllers:  STORAGE_PREFIX + "show_controllers"
};

// ─── DEFAULTS ───
var DEFAULTS = {
    pollInterval:     2,
    compactMode:      false,
    autoRefresh:      true,
    spoofThreshold:   50,
    authGrace:        30,
    staleTimeout:     120,
    theme:            "",
    mapCenterLat:     18.2528,
    mapCenterLng:     -65.6433,
    mapZoom:          13,
    mapStyle:         "satellite",
    rangeRingRadius:  500,
    trailLength:      100,
    soundAlerts:      true,
    showControllers:  false
};

// ─── DOM REFS ───
var els = {};

// ─── INIT ───
function init() {
    cacheElements();
    restoreSettings();
    bindEvents();
    fetchSystemInfo();
    setInterval(fetchSystemInfo, 5000);
    tick();
    setInterval(tick, 1000);
    checkTelegramStatus();
}

function cacheElements() {
    els.pollInterval      = document.getElementById("poll-interval");
    els.pollIntervalVal   = document.getElementById("poll-interval-val");
    els.compactMode       = document.getElementById("compact-mode");
    els.autoRefresh       = document.getElementById("auto-refresh");
    els.tgBotToken        = document.getElementById("tg-bot-token");
    els.tgChatId          = document.getElementById("tg-chat-id");
    els.tgNewDrone        = document.getElementById("tg-new-drone");
    els.tgSpoofing        = document.getElementById("tg-spoofing");
    els.tgDroneLost       = document.getElementById("tg-drone-lost");
    els.tgTapStatus       = document.getElementById("tg-tap-status");
    els.btnSaveTelegram   = document.getElementById("btn-save-telegram");
    els.spoofThreshold    = document.getElementById("spoof-threshold");
    els.spoofThresholdVal = document.getElementById("spoof-threshold-val");
    els.authGrace         = document.getElementById("auth-grace");
    els.staleTimeout      = document.getElementById("stale-timeout");
    els.btnTestTelegram   = document.getElementById("btn-test-telegram");
    els.btnClearData      = document.getElementById("btn-clear-data");
    els.confirmOverlay    = document.getElementById("confirm-overlay");
    els.confirmCancel     = document.getElementById("confirm-cancel");
    els.confirmProceed    = document.getElementById("confirm-proceed");
    els.mapCenterLat      = document.getElementById("map-center-lat");
    els.mapCenterLng      = document.getElementById("map-center-lng");
    els.mapZoom           = document.getElementById("map-zoom");
    els.mapZoomVal        = document.getElementById("map-zoom-val");
    els.mapStyle          = document.getElementById("map-style");
    els.rangeRingRadius   = document.getElementById("range-ring-radius");
    els.trailLength       = document.getElementById("trail-length");
    els.soundAlerts       = document.getElementById("sound-alerts");
    els.showControllers   = document.getElementById("show-controllers");
    els.toast             = document.getElementById("toast");
    els.hdrClock          = document.getElementById("hdr-clock");
    els.hdrConn           = document.getElementById("hdr-conn");
    els.hdrUptime         = document.getElementById("hdr-uptime");
    els.hdrServerTime     = document.getElementById("hdr-server-time");
    els.sbNodeStatus      = document.getElementById("sb-node-status");
    els.themeRadios       = document.querySelectorAll('input[name="theme"]');
    els.btnUpdateIntel    = document.getElementById("btn-update-intel");
    els.intelVersion      = document.getElementById("intel-version");
}

// ─── RESTORE SETTINGS FROM LOCALSTORAGE ───
function restoreSettings() {
    var pollVal = loadInt(KEYS.pollInterval, DEFAULTS.pollInterval, 1, 5);
    els.pollInterval.value = pollVal;
    els.pollIntervalVal.textContent = pollVal + "s";

    els.compactMode.checked  = loadBool(KEYS.compactMode, DEFAULTS.compactMode);
    els.autoRefresh.checked  = loadBool(KEYS.autoRefresh, DEFAULTS.autoRefresh);

    // Telegram settings are loaded from server (see checkTelegramStatus)

    var spoofVal = loadInt(KEYS.spoofThreshold, DEFAULTS.spoofThreshold, 0, 100);
    els.spoofThreshold.value = spoofVal;
    els.spoofThresholdVal.textContent = spoofVal;

    els.authGrace.value    = loadInt(KEYS.authGrace, DEFAULTS.authGrace, 0, 600);
    els.staleTimeout.value = loadInt(KEYS.staleTimeout, DEFAULTS.staleTimeout, 10, 600);

    // Map defaults
    els.mapCenterLat.value    = loadFloat(KEYS.mapCenterLat, DEFAULTS.mapCenterLat);
    els.mapCenterLng.value    = loadFloat(KEYS.mapCenterLng, DEFAULTS.mapCenterLng);
    var zoomVal = loadInt(KEYS.mapZoom, DEFAULTS.mapZoom, 3, 18);
    els.mapZoom.value         = zoomVal;
    els.mapZoomVal.textContent = zoomVal;
    els.mapStyle.value        = loadStr(KEYS.mapStyle, DEFAULTS.mapStyle);
    els.rangeRingRadius.value = loadInt(KEYS.rangeRingRadius, DEFAULTS.rangeRingRadius, 50, 5000);
    els.trailLength.value     = loadInt(KEYS.trailLength, DEFAULTS.trailLength, 10, 500);
    els.soundAlerts.checked   = loadBool(KEYS.soundAlerts, DEFAULTS.soundAlerts);
    els.showControllers.checked = loadBool(KEYS.showControllers, DEFAULTS.showControllers);

    var storedTheme = localStorage.getItem(KEYS.theme) || "";
    var themeVal = storedTheme || "tactical";
    var radio = document.getElementById("theme-" + themeVal);
    if (radio) radio.checked = true;
}

// ─── BIND ALL EVENTS ───
function bindEvents() {
    // Display preferences
    els.pollInterval.addEventListener("input", function () {
        var v = parseInt(this.value, 10);
        els.pollIntervalVal.textContent = v + "s";
        save(KEYS.pollInterval, v);
    });

    els.compactMode.addEventListener("change", function () {
        save(KEYS.compactMode, this.checked);
    });

    els.autoRefresh.addEventListener("change", function () {
        save(KEYS.autoRefresh, this.checked);
    });

    // Theme selector — use raw localStorage (no JSON.stringify)
    // so the <head> init script can read it directly
    els.themeRadios.forEach(function(radio) {
        radio.addEventListener("change", function() {
            var value = this.value;
            var html = document.documentElement;
            if (value === "tactical") {
                html.removeAttribute("data-theme");
                localStorage.removeItem(KEYS.theme);
            } else {
                html.setAttribute("data-theme", value);
                localStorage.setItem(KEYS.theme, value);
            }
            showToast("Theme changed to " + value, "ok");
        });
    });

    // Telegram save button
    els.btnSaveTelegram.addEventListener("click", saveTelegram);

    // Alert thresholds
    els.spoofThreshold.addEventListener("input", function () {
        var v = parseInt(this.value, 10);
        els.spoofThresholdVal.textContent = v;
        save(KEYS.spoofThreshold, v);
    });

    els.authGrace.addEventListener("change", function () {
        var v = clamp(parseInt(this.value, 10) || 0, 0, 600);
        this.value = v;
        save(KEYS.authGrace, v);
    });

    els.staleTimeout.addEventListener("change", function () {
        var v = clamp(parseInt(this.value, 10) || 10, 10, 600);
        this.value = v;
        save(KEYS.staleTimeout, v);
    });

    // Map defaults
    els.mapCenterLat.addEventListener("change", function () {
        var v = parseFloat(this.value);
        if (!isNaN(v) && v >= -90 && v <= 90) save(KEYS.mapCenterLat, v);
    });
    els.mapCenterLng.addEventListener("change", function () {
        var v = parseFloat(this.value);
        if (!isNaN(v) && v >= -180 && v <= 180) save(KEYS.mapCenterLng, v);
    });
    els.mapZoom.addEventListener("input", function () {
        var v = parseInt(this.value, 10);
        els.mapZoomVal.textContent = v;
        save(KEYS.mapZoom, v);
    });
    els.mapStyle.addEventListener("change", function () {
        save(KEYS.mapStyle, this.value);
    });
    els.rangeRingRadius.addEventListener("change", function () {
        var v = clamp(parseInt(this.value, 10) || 500, 50, 5000);
        this.value = v;
        save(KEYS.rangeRingRadius, v);
    });
    els.trailLength.addEventListener("change", function () {
        var v = clamp(parseInt(this.value, 10) || 100, 10, 500);
        this.value = v;
        save(KEYS.trailLength, v);
    });
    els.soundAlerts.addEventListener("change", function () {
        save(KEYS.soundAlerts, this.checked);
    });
    els.showControllers.addEventListener("change", function () {
        save(KEYS.showControllers, this.checked);
        syncShowControllersToAirspace(this.checked);
    });

    // Intel update
    if (els.btnUpdateIntel) {
        els.btnUpdateIntel.addEventListener("click", updateIntel);
    }

    // Actions
    els.btnTestTelegram.addEventListener("click", testTelegram);
    els.btnClearData.addEventListener("click", showConfirm);
    els.confirmCancel.addEventListener("click", hideConfirm);
    els.confirmProceed.addEventListener("click", clearTestData);

    // Close overlay on background click
    els.confirmOverlay.addEventListener("click", function (e) {
        if (e.target === els.confirmOverlay) hideConfirm();
    });

    // Close overlay on Escape key
    document.addEventListener("keydown", function (e) {
        if (e.key === "Escape" && els.confirmOverlay.classList.contains("active")) {
            hideConfirm();
        }
    });
}

// ─── CLOCK TICK ───
function tick() {
    var now = new Date();
    var hh = String(now.getHours()).padStart(2, "0");
    var mm = String(now.getMinutes()).padStart(2, "0");
    var ss = String(now.getSeconds()).padStart(2, "0");
    if (els.hdrClock) els.hdrClock.textContent = hh + ":" + mm + ":" + ss;
}

// ─── FETCH SYSTEM INFO ───
function fetchSystemInfo() {
    fetch("/api/status")
        .then(function (r) {
            if (!r.ok) throw new Error("HTTP " + r.status);
            return r.json();
        })
        .then(function (data) {
            setConnected(true);
            renderSystemInfo(data);
            _updateSbBadges(data);
        })
        .catch(function () {
            setConnected(false);
            renderSystemInfoOffline();
        });
}

function setConnected(ok) {
    if (els.hdrConn) {
        els.hdrConn.innerHTML = ok
            ? '<span class="hdr-dot green"></span> Connected'
            : '<span class="hdr-dot red"></span> Disconnected';
    }
    if (els.sbNodeStatus) {
        els.sbNodeStatus.innerHTML = ok
            ? '<span class="sb-dot green"></span><span>Node Online</span>'
            : '<span class="sb-dot red"></span><span>Node Offline</span>';
    }
}

function renderSystemInfo(data) {
    var s = data.stats || {};
    var w = s.writer || {};
    var fl = s.flight_logger || {};
    var dbErrors = w.errors || 0;
    var dbOk = dbErrors === 0;

    setText("sys-node-name", data.node_name || data.name || "--");
    setText("sys-node-uuid", data.node_uuid || data.uuid || "--");
    setText("sys-zmq-port", data.zmq_port || s.zmq_port || "--");

    var dbEl = document.getElementById("sys-db-status");
    if (dbEl) {
        dbEl.textContent = dbOk ? "Connected" : dbErrors + " error(s)";
        dbEl.className = "sysinfo-val " + (dbOk ? "ok" : "err");
    }

    setText("sys-version", data.version || "--");
    if (els.intelVersion && data.intel_version) {
        els.intelVersion.textContent = "v" + data.intel_version;
    }
    setText("sys-uptime", data.uptime || "--");

    // New live stats fields
    var msgsReceived = s.messages_received;
    setText("sys-messages-received", msgsReceived != null ? msgsReceived : "--");

    var uavsTracked = s.uavs_tracked;
    setText("sys-uavs-tracked", uavsTracked != null ? uavsTracked : "--");

    var tapsTracked = s.taps_tracked;
    setText("sys-taps-online", tapsTracked != null ? tapsTracked : "--");

    // DB Writes = uavs_written + vectors_written
    var uavsWritten = w.uavs_written || 0;
    var vectorsWritten = w.vectors_written || 0;
    if (w.uavs_written != null || w.vectors_written != null) {
        setText("sys-db-writes", uavsWritten + vectorsWritten);
    } else {
        setText("sys-db-writes", "--");
    }

    // Flight Logs = rows_written (active_files)
    if (fl.rows_written != null) {
        var flText = fl.rows_written + " rows";
        if (fl.active_files != null) {
            flText += " (" + fl.active_files + " active)";
        }
        setText("sys-flight-logs", flText);
    } else {
        setText("sys-flight-logs", "--");
    }

    // Server time
    setText("sys-server-time", data.server_time || "--");

    if (els.hdrUptime && data.uptime) {
        els.hdrUptime.textContent = "UP " + data.uptime;
    }

    // Show server_time in header to confirm live connection
    if (els.hdrServerTime && data.server_time) {
        els.hdrServerTime.textContent = "SRV " + data.server_time;
    }
}

function renderSystemInfoOffline() {
    setText("sys-node-name", "--");
    setText("sys-node-uuid", "--");
    setText("sys-zmq-port", "--");

    var dbEl = document.getElementById("sys-db-status");
    if (dbEl) {
        dbEl.textContent = "Unreachable";
        dbEl.className = "sysinfo-val err";
    }

    setText("sys-version", "--");
    setText("sys-uptime", "--");
    setText("sys-messages-received", "--");
    setText("sys-uavs-tracked", "--");
    setText("sys-taps-online", "--");
    setText("sys-db-writes", "--");
    setText("sys-flight-logs", "--");
    setText("sys-server-time", "--");

    if (els.hdrServerTime) {
        els.hdrServerTime.textContent = "";
    }
}

// ─── TELEGRAM ───
function checkTelegramStatus() {
    fetch("/api/telegram/status")
    .then(function (r) { return r.json(); })
    .then(function (data) {
        var statusEl = document.getElementById("tg-server-status");
        if (statusEl) {
            if (data.enabled) {
                statusEl.textContent = "Active — notifications will be sent on alerts";
                statusEl.className = "field-hint tg-status-ok";
            } else {
                statusEl.textContent = "Not configured";
                statusEl.className = "field-hint tg-status-off";
            }
        }
        // Populate form fields from server config
        if (els.tgBotToken) els.tgBotToken.value = data.bot_token || "";
        if (els.tgChatId) els.tgChatId.value = data.chat_id || "";
        if (els.tgNewDrone) els.tgNewDrone.checked = data.notify_new_drone !== false;
        if (els.tgSpoofing) els.tgSpoofing.checked = data.notify_spoofing !== false;
        if (els.tgDroneLost) els.tgDroneLost.checked = data.notify_drone_lost !== false;
        if (els.tgTapStatus) els.tgTapStatus.checked = data.notify_tap_status !== false;
    })
    .catch(function () {});
}

function saveTelegram() {
    var payload = {
        bot_token: els.tgBotToken.value.trim(),
        chat_id: els.tgChatId.value.trim(),
        notify_new_drone: els.tgNewDrone.checked,
        notify_spoofing: els.tgSpoofing.checked,
        notify_drone_lost: els.tgDroneLost.checked,
        notify_tap_status: els.tgTapStatus.checked
    };

    els.btnSaveTelegram.disabled = true;
    els.btnSaveTelegram.textContent = "Saving...";

    fetch("/api/telegram/status", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
    })
    .then(function (r) { return r.json(); })
    .then(function (data) {
        if (data.ok) {
            showToast("Telegram settings saved", "ok");
            // Refresh status display
            checkTelegramStatus();
        } else {
            showToast("Save failed: " + (data.error || "unknown"), "error");
        }
    })
    .catch(function (err) {
        showToast("Save failed: " + err.message, "error");
    })
    .finally(function () {
        els.btnSaveTelegram.disabled = false;
        els.btnSaveTelegram.textContent = "Save";
    });
}

function testTelegram() {
    els.btnTestTelegram.disabled = true;
    els.btnTestTelegram.textContent = "Sending...";

    fetch("/api/telegram/test", { method: "POST" })
    .then(function (r) { return r.json(); })
    .then(function (data) {
        if (data.ok) {
            showToast("Test message sent!", "ok");
        } else {
            showToast("Failed: " + (data.error || "unknown"), "error");
        }
    })
    .catch(function (err) {
        showToast("Failed: " + err.message, "error");
    })
    .finally(function () {
        els.btnTestTelegram.disabled = false;
        els.btnTestTelegram.textContent = "Send Test";
    });
}

// ─── INTEL UPDATE ───
function updateIntel() {
    var btn = els.btnUpdateIntel;
    if (!btn) return;
    btn.disabled = true;
    btn.textContent = "Updating...";

    fetch("/api/intel/update", {
        method: "POST",
        credentials: "same-origin"
    })
    .then(function(r) { return r.json(); })
    .then(function(data) {
        if (data.ok) {
            var msg = "Intel DB updated to v" + data.version;
            if (data.new_ouis > 0) {
                msg += " (+" + data.new_ouis + " OUIs)";
            } else {
                msg += " (up to date)";
            }
            showToast(msg, "ok");
            if (els.intelVersion) {
                els.intelVersion.textContent = "v" + data.version;
            }
        } else {
            showToast("Update failed: " + (data.error || "unknown"), "error");
        }
    })
    .catch(function() { showToast("Network error", "error"); })
    .finally(function() {
        btn.disabled = false;
        btn.textContent = "Update Now";
    });
}

// ─── CLEAR TEST DATA ───
function showConfirm() {
    els.confirmOverlay.classList.add("active");
}

function hideConfirm() {
    els.confirmOverlay.classList.remove("active");
}

function clearTestData() {
    hideConfirm();

    els.btnClearData.disabled = true;
    els.btnClearData.textContent = "Clearing...";

    fetch("/api/test/clear", { method: "POST" })
        .then(function (r) {
            if (!r.ok) throw new Error("HTTP " + r.status);
            return r.json();
        })
        .then(function (data) {
            var msg;
            if (data.ok && data.removed != null) {
                msg = "Cleared " + data.removed + " rows";
            } else {
                msg = data.message || "Test data cleared successfully";
            }
            showToast(msg, "ok");
            // Refresh system info after clearing
            fetchSystemInfo();
        })
        .catch(function (err) {
            showToast("Clear failed: " + err.message, "error");
        })
        .finally(function () {
            els.btnClearData.disabled = false;
            els.btnClearData.textContent = "Clear Test Data";
        });
}

// ─── TOAST NOTIFICATIONS ───
var toastTimer = null;

function showToast(message, type) {
    if (!els.toast) return;

    if (toastTimer) clearTimeout(toastTimer);

    els.toast.textContent = message;
    els.toast.className = "toast";

    if (type === "error") {
        els.toast.classList.add("error");
    } else if (type === "warn") {
        els.toast.classList.add("warn");
    }

    // Force reflow before adding visible class
    void els.toast.offsetWidth;
    els.toast.classList.add("visible");

    toastTimer = setTimeout(function () {
        els.toast.classList.remove("visible");
        toastTimer = null;
    }, 3500);
}

// ─── LOCALSTORAGE HELPERS ───
function save(key, value) {
    try {
        localStorage.setItem(key, JSON.stringify(value));
    } catch (e) {
        // Storage full or unavailable
    }
    if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferences) SkylensAuth.savePreferences();
}

function loadStr(key, fallback) {
    try {
        var raw = localStorage.getItem(key);
        if (raw === null) return fallback;
        return JSON.parse(raw);
    } catch (e) {
        return fallback;
    }
}

function loadBool(key, fallback) {
    try {
        var raw = localStorage.getItem(key);
        if (raw === null) return fallback;
        return JSON.parse(raw) === true;
    } catch (e) {
        return fallback;
    }
}

function loadInt(key, fallback, min, max) {
    try {
        var raw = localStorage.getItem(key);
        if (raw === null) return fallback;
        var v = parseInt(JSON.parse(raw), 10);
        if (isNaN(v)) return fallback;
        return clamp(v, min, max);
    } catch (e) {
        return fallback;
    }
}

function loadFloat(key, fallback) {
    try {
        var raw = localStorage.getItem(key);
        if (raw === null) return fallback;
        var v = parseFloat(JSON.parse(raw));
        if (isNaN(v)) return fallback;
        return v;
    } catch (e) {
        return fallback;
    }
}

// Sync showControllers into nz_settings blob so the Airspace page picks it up
function syncShowControllersToAirspace(on) {
    try {
        var blob = JSON.parse(localStorage.getItem("nz_settings") || "{}");
        blob.showControllers = on;
        localStorage.setItem("nz_settings", JSON.stringify(blob));
    } catch (e) { /* ignore */ }
    if (typeof SkylensAuth !== 'undefined' && SkylensAuth.savePreferences) SkylensAuth.savePreferences();
}

// ─── UTILS ───
function clamp(v, min, max) {
    return Math.max(min, Math.min(max, v));
}

function setText(id, text) {
    var el = document.getElementById(id);
    if (el) el.textContent = text;
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
document.addEventListener("DOMContentLoaded", function() {
    if (typeof SkylensAuth !== 'undefined' && SkylensAuth.whenReady) {
        SkylensAuth.whenReady().then(function () { init(); });
    } else {
        init();
    }
});

})();
