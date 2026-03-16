/* ===================================================================
   SKYLENS COMMAND CENTER -- UAV FLEET PAGE JS
   Polls /api/status, renders UAV fleet cards with filtering, sorting,
   search, trust scoring, detection source badges, and real-time updates.
   =================================================================== */

(function () {
"use strict";

var POLL_MS = 1000;  // Reduced from 500ms to prevent flicker

// Status age thresholds (seconds since last timestamp)
var AIRBORNE_THRESHOLD = 30;   // <30s since timestamp => airborne
var GROUNDED_THRESHOLD = 120;  // <120s since timestamp => grounded
                               // >=120s => lost

var lastData = null;
var pollFails = 0;
var allUavs = [];
var filteredUavs = [];

// Filter state
var filterSearch = "";
var filterStatus = "";
var filterTrust = "";
var filterSource = "";
var filterClass = "";

// Sort state
var sortMode = "newest"; // default: newest first

// Stable DOM caching for UAV cards (prevents flashing)
var uavCards = {};  // identifier -> DOM element

// Cached DOM element for HTML escaping
var _escDiv = document.createElement("div");
function esc(s) {
    _escDiv.textContent = s == null ? "" : String(s);
    return _escDiv.innerHTML;
}

// JavaScript string escape for use in onclick handlers
function escJs(s) {
    if (s == null) return '';
    return String(s).replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/"/g, '\\"');
}

// Sanitize UTM/session IDs (reject binary garbage)
function sanitizeID(s) {
    if (!s) return "--";
    // Check if string contains non-printable characters
    for (var i = 0; i < s.length; i++) {
        var c = s.charCodeAt(i);
        if (c < 32 && c !== 9 && c !== 10 && c !== 13) return "--";
        if (c > 126 && c < 160) return "--";
    }
    // If string is mostly garbled (high ratio of unusual chars), hide it
    var unusual = 0;
    for (var j = 0; j < s.length; j++) {
        var cc = s.charCodeAt(j);
        if (cc > 127 || cc < 32) unusual++;
    }
    if (s.length > 0 && unusual / s.length > 0.3) return "--";
    return s;
}

// Build a useful display name for a drone
// Priority: manufacturer + model > manufacturer + serial > serial > identifier
function buildDroneName(u) {
    var mfg = u.manufacturer && u.manufacturer !== "UNKNOWN" ? u.manufacturer : null;
    var model = u.model && u.model !== "UNKNOWN" && u.model !== "" ? u.model.replace(" (Unknown)", "") : null;
    if (model && mfg && model === mfg) model = null; // avoid "DJI DJI"
    var serial = u.serial_number || null;
    var ident = u.identifier || "Unknown";
    var trk = u.track_number > 0 ? "TRK-" + String(u.track_number).padStart(3, "0") + " " : "";

    // If we have manufacturer and model: "TRK-001 DJI Mavic 3"
    if (mfg && model) {
        return trk + mfg + " " + model;
    }

    // If we have manufacturer and serial: "TRK-001 DJI • 1581F163..."
    if (mfg && serial) {
        var shortSerial = serial.length > 12 ? serial.substring(0, 12) + "…" : serial;
        return trk + mfg + " • " + shortSerial;
    }

    // If we just have manufacturer: "TRK-001 DJI"
    if (mfg) {
        return trk + mfg;
    }

    // If we have serial: show it
    if (serial) {
        return trk + (serial.length > 16 ? serial.substring(0, 16) + "…" : serial);
    }

    // Fallback to identifier (shortened)
    return trk + (ident.length > 16 ? ident.substring(0, 16) + "…" : ident);
}

// --- INIT ---
function init() {
    tick();
    poll();
    setInterval(poll, POLL_MS);
    setInterval(tick, 1000);
    bindFilters();
    bindSort();
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
    fetch("/api/status")
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (data) {
            pollFails = 0;
            lastData = data;
            setConnected(true);
            processData(data);
            if (typeof SkylensAuth !== 'undefined') SkylensAuth.revealPage();
        })
        .catch(function () {
            pollFails++;
            if (pollFails > 2) setConnected(false);
        });
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

// --- CLASSIFICATION (client-side) ---
var CLASS_KEYWORDS = {
    "Matrice": "enterprise", "M30": "enterprise", "M300": "enterprise", "M350": "enterprise",
    "Mavic": "prosumer", "Air": "consumer", "Mini": "consumer", "Spark": "consumer",
    "Phantom": "prosumer", "Inspire": "enterprise", "Avata": "consumer",
    "FPV": "fpv_racing", "Nazgul": "fpv_racing", "TinyHawk": "fpv_racing",
    "Anafi": "prosumer", "Disco": "consumer", "Bebop": "consumer",
    "EVO": "prosumer", "Dragonfish": "enterprise",
    "Skydio": "enterprise", "X2": "enterprise", "X10": "enterprise",
    "Wing": "delivery", "Zipline": "delivery",
    "Agras": "agricultural", "T40": "agricultural", "XAG": "agricultural"
};

function classifyDesignation(designation) {
    if (!designation || designation === "UNKNOWN") return "unknown";
    var upper = designation.toUpperCase();
    for (var keyword in CLASS_KEYWORDS) {
        if (upper.indexOf(keyword.toUpperCase()) !== -1) return CLASS_KEYWORDS[keyword];
    }
    return "unknown";
}

// --- SHOW CONTROLLERS SETTING ---
function loadShowControllers() {
    try {
        var v = localStorage.getItem("skylens_show_controllers");
        if (v !== null) return JSON.parse(v) === true;
        var blob = JSON.parse(localStorage.getItem("nz_settings") || "{}");
        if (blob.showControllers !== undefined) return !!blob.showControllers;
    } catch (e) {}
    return false;
}

// --- PROCESS DATA ---
function processData(d) {
    var showCtrl = loadShowControllers();
    var raw = d.uavs || [];
    if (!showCtrl) {
        raw = raw.filter(function(u) { return u.is_controller !== true; });
    }

    // Store taps list globally so card builder can look up tap names
    window.fleetTaps = d.taps || [];

    // Annotate each UAV with computed status and classification
    var now = Date.now();
    allUavs = raw.map(function (u) {
        var copy = {};
        for (var k in u) {
            if (u.hasOwnProperty(k)) copy[k] = u[k];
        }
        copy._status = computeStatus(u, now);
        copy._classification = classifyDesignation(u.designation);
        return copy;
    });

    // Update header subtitle
    var node = document.getElementById("hdr-node");
    if (node) {
        node.textContent = allUavs.length + " UAV(s) tracked";
    }
    var up = document.getElementById("hdr-uptime");
    if (up && d.uptime) up.textContent = "UP " + d.uptime;

    applyFilters();
}

// --- STATUS COMPUTATION ---
// Uses `timestamp` field (ISO 8601) from the in-memory UAV cache.
// Thresholds: <30s = airborne, <120s = grounded, >=120s = lost.
// If operational_status is explicitly set, it can override within the non-lost window.
function computeStatus(u, now) {
    // Read from `timestamp` (the actual API field), NOT `last_seen`
    var ts = u.timestamp;
    var age = 999;
    if (ts) {
        var tsMs = new Date(ts).getTime();
        if (!isNaN(tsMs)) {
            age = (now - tsMs) / 1000;
        }
    }

    // If stale beyond grounded threshold, always lost
    if (age >= GROUNDED_THRESHOLD) return "lost";

    // Check explicit operational_status from the drone's Remote ID broadcast
    var opStatus = (u.operational_status || "").toLowerCase();
    if (opStatus === "ground" || opStatus === "grounded") return "grounded";
    if (opStatus === "emergency") return "airborne"; // emergency is airborne
    if (opStatus === "airborne" || opStatus === "flying" || opStatus === "in_flight") return "airborne";

    // Within airborne window
    if (age < AIRBORNE_THRESHOLD) {
        // Infer from altitude/speed if operational_status is absent or unknown
        var alt = u.altitude_geodetic;
        var spd = u.speed;
        if ((alt != null && alt > 5) || (spd != null && spd > 0.5)) return "airborne";
        // Recently seen but no altitude/speed data -- assume grounded
        return "grounded";
    }

    // Between 30s and 120s with no explicit status
    return "grounded";
}

// --- FILTERS ---
function bindFilters() {
    var searchInput = document.getElementById("filter-search");
    if (searchInput) {
        searchInput.addEventListener("input", function () {
            filterSearch = searchInput.value.toLowerCase().trim();
            applyFilters();
        });
    }

    var statusSel = document.getElementById("filter-status");
    if (statusSel) {
        statusSel.addEventListener("change", function () {
            filterStatus = statusSel.value;
            applyFilters();
        });
    }

    var trustSel = document.getElementById("filter-trust");
    if (trustSel) {
        trustSel.addEventListener("change", function () {
            filterTrust = trustSel.value;
            applyFilters();
        });
    }

    var sourceSel = document.getElementById("filter-source");
    if (sourceSel) {
        sourceSel.addEventListener("change", function () {
            filterSource = sourceSel.value;
            applyFilters();
        });
    }

    var classSel = document.getElementById("filter-class");
    if (classSel) {
        classSel.addEventListener("change", function () {
            filterClass = classSel.value;
            applyFilters();
        });
    }
}

function bindSort() {
    var sortSel = document.getElementById("fleet-sort");
    if (sortSel) {
        sortSel.addEventListener("change", function () {
            sortMode = sortSel.value;
            applyFilters();
        });
    }
}

function applyFilters() {
    filteredUavs = allUavs.filter(function (u) {
        // Status filter
        if (filterStatus && u._status !== filterStatus) return false;

        // Trust filter (aligned with display thresholds: green>=80, yellow 50-79, red <50)
        if (filterTrust) {
            var trust = u.trust_score != null ? u.trust_score : 100;
            if (filterTrust === "trusted" && trust < 80) return false;
            if (filterTrust === "suspicious" && (trust < 50 || trust >= 80)) return false;
            if (filterTrust === "hostile" && trust >= 50) return false;
        }

        // Source filter
        if (filterSource && (u.detection_source || "") !== filterSource) return false;

        // Classification filter
        if (filterClass && (u._classification || "unknown") !== filterClass) return false;

        // Text search: searches designation, identifier, id_serial, mac, operator_id, etc.
        if (filterSearch) {
            var haystack = [
                u.identifier || "",
                u.designation || "",
                u.operator_id || "",
                u.serial_number || "",
                u.registration || "",
                u.uav_type || "",
                u.detection_source || "",
                u.mac || "",
                u.ssid || "",
                (u.spoof_flags || []).join(" "),
                u._classification || ""
            ].join(" ").toLowerCase();
            if (haystack.indexOf(filterSearch) === -1) return false;
        }

        return true;
    });

    // Sort based on selected mode
    sortUavs(filteredUavs);

    renderSummary();
    renderGrid();
    _updateSbBadges(lastData);
}

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

function sortUavs(list) {
    var statusOrder = { lost: 0, airborne: 1, grounded: 2 };

    // Stable tiebreaker: always use identifier as final sort key
    function tiebreaker(a, b) {
        var ia = a.identifier || "";
        var ib = b.identifier || "";
        return ia < ib ? -1 : ia > ib ? 1 : 0;
    }

    if (sortMode === "newest") {
        list.sort(function (a, b) {
            var ta = a.timestamp ? new Date(a.timestamp).getTime() : 0;
            var tb = b.timestamp ? new Date(b.timestamp).getTime() : 0;
            if (ta !== tb) return tb - ta; // newest first
            return tiebreaker(a, b);
        });
    } else if (sortMode === "name") {
        list.sort(function (a, b) {
            var na = (a.designation && a.designation !== "UNKNOWN" ? a.designation : a.identifier || "").toLowerCase();
            var nb = (b.designation && b.designation !== "UNKNOWN" ? b.designation : b.identifier || "").toLowerCase();
            if (na !== nb) return na < nb ? -1 : 1;
            return tiebreaker(a, b);
        });
    } else if (sortMode === "trust") {
        list.sort(function (a, b) {
            var ta = a.trust_score != null ? a.trust_score : 100;
            var tb = b.trust_score != null ? b.trust_score : 100;
            if (ta !== tb) return ta - tb; // lowest trust first (most suspicious)
            return tiebreaker(a, b);
        });
    } else if (sortMode === "rssi") {
        list.sort(function (a, b) {
            var ra = a.rssi != null ? a.rssi : -999;
            var rb = b.rssi != null ? b.rssi : -999;
            if (ra !== rb) return rb - ra; // strongest signal first
            return tiebreaker(a, b);
        });
    } else if (sortMode === "altitude") {
        list.sort(function (a, b) {
            var aa = a.altitude_geodetic != null ? a.altitude_geodetic : -1;
            var ab = b.altitude_geodetic != null ? b.altitude_geodetic : -1;
            if (aa !== ab) return ab - aa; // highest altitude first
            return tiebreaker(a, b);
        });
    } else {
        // Default: status priority (lost first), then trust ascending within same status
        list.sort(function (a, b) {
            var sa = statusOrder[a._status] !== undefined ? statusOrder[a._status] : 3;
            var sb2 = statusOrder[b._status] !== undefined ? statusOrder[b._status] : 3;
            if (sa !== sb2) return sa - sb2;
            var ta = a.trust_score != null ? a.trust_score : 100;
            var tb = b.trust_score != null ? b.trust_score : 100;
            if (ta !== tb) return ta - tb;
            return tiebreaker(a, b);
        });
    }
}

// --- SUMMARY CARDS ---
function renderSummary() {
    var total = allUavs.length;
    var airborne = 0;
    var grounded = 0;
    var lost = 0;
    var trustSum = 0;
    var trustCount = 0;
    var alerts = 0;
    var spoofSuspects = 0;
    var identified = 0;

    allUavs.forEach(function (u) {
        if (u._status === "airborne") airborne++;
        else if (u._status === "grounded") grounded++;
        else if (u._status === "lost") lost++;

        if (u._status === "lost") alerts++;
        if (u.trust_score != null) {
            trustSum += u.trust_score;
            trustCount++;
            if (u.trust_score < 50) { alerts++; spoofSuspects++; }
        }
        if ((u.spoof_flags || []).length > 0) spoofSuspects++;
        if (u.designation && u.designation !== "UNKNOWN") identified++;
    });

    var avgTrust = trustCount > 0 ? (trustSum / trustCount).toFixed(0) : "--";

    setSummary("sum-total", total);
    setSummary("sum-airborne", airborne);
    setSummary("sum-trust", avgTrust);
    setSummary("sum-spoof", spoofSuspects);
    setSummary("sum-identified", identified);
    setSummary("sum-alerts", alerts);

    // Update badge: show filtered/total
    var badge = document.getElementById("fleet-count-badge");
    if (badge) {
        if (filteredUavs.length === total) {
            badge.textContent = total;
        } else {
            badge.textContent = filteredUavs.length + " / " + total;
        }
        badge.className = "badge" + (alerts > 0 ? " warn" : "");
    }

    // Update total/filtered text in header
    var node = document.getElementById("hdr-node");
    if (node) {
        if (filteredUavs.length === total) {
            node.textContent = total + " UAV(s) tracked";
        } else {
            node.textContent = filteredUavs.length + " of " + total + " UAV(s) shown";
        }
    }
}

function setSummary(id, val) {
    var el = document.getElementById(id);
    if (!el) return;
    var v = el.querySelector(".summary-val");
    if (v) v.textContent = val;
}

// --- UAV CARD GRID (Stable DOM Updates) ---
function renderGrid() {
    var grid = document.getElementById("fleet-grid");
    var empty = document.getElementById("fleet-empty");
    if (!grid) return;

    // Save scroll position
    var scrollTop = grid.parentElement ? grid.parentElement.scrollTop : 0;

    // Build set of current UAV IDs for removal detection
    var currentIds = {};
    filteredUavs.forEach(function(u) {
        var id = u.identifier || u.mac || "";
        if (id) currentIds[id] = true;
    });

    // Remove cards for UAVs no longer in filtered list
    Object.keys(uavCards).forEach(function(id) {
        if (!currentIds[id]) {
            var card = uavCards[id];
            if (card && card.parentNode) {
                card.parentNode.removeChild(card);
            }
            delete uavCards[id];
        }
    });

    if (!filteredUavs.length) {
        if (empty) {
            empty.style.display = "";
            var emptyText = empty.querySelector(".fleet-empty-text");
            var emptySub = empty.querySelector(".fleet-empty-sub");
            if (allUavs.length === 0) {
                if (emptyText) emptyText.textContent = "No UAVs detected -- airspace clear";
                if (emptySub) emptySub.textContent = "Monitoring for Remote ID broadcasts";
            } else {
                if (emptyText) emptyText.textContent = "No UAVs match the current filters";
                if (emptySub) emptySub.textContent = allUavs.length + " UAV(s) tracked total -- adjust filters to see them";
            }
        }
        return;
    }
    if (empty) empty.style.display = "none";

    // Update or create cards (stable DOM updates)
    filteredUavs.forEach(function(u) {
        var id = u.identifier || u.mac || "";
        if (!id) return;

        if (uavCards[id]) {
            // Update existing card in place
            updateCardContent(uavCards[id], u);
        } else {
            // Create new card
            var card = createCard(u);
            uavCards[id] = card;
            grid.appendChild(card);
        }
    });

    // Reorder cards to match sorted order
    filteredUavs.forEach(function(u) {
        var id = u.identifier || u.mac || "";
        if (id && uavCards[id]) {
            grid.appendChild(uavCards[id]);
        }
    });

    // Restore scroll position
    if (grid.parentElement) grid.parentElement.scrollTop = scrollTop;
}

// Create a new UAV card DOM element
function createCard(u) {
    var card = document.createElement("div");
    card.className = "uav-card";
    card.setAttribute("data-id", u.identifier || u.mac || "");
    updateCardContent(card, u);
    return card;
}

// Update card content in place (prevents flashing)
function updateCardContent(card, u) {
    var status = u._status;
    var id = u.identifier || u.mac || "";

    // Update class
    card.className = "uav-card " + esc(status) + "-border";
    card.setAttribute("data-id", id);

    // Build inner HTML
    card.innerHTML = buildCardInnerHTML(u);
}

// Build inner HTML for card (without outer wrapper)
function buildCardInnerHTML(u) {
    var status = u._status;
    var trust = u.trust_score != null ? u.trust_score : 100;
    var trustColor = trustBadgeColor(trust);
    var trustBg = trustBadgeBg(trust);

    // Name: build from manufacturer + model/serial (NOT designation which is often useless)
    var name = buildDroneName(u);
    var ident = u.identifier || "--";

    // Manufacturer
    var mfgStr = u.manufacturer && u.manufacturer !== "UNKNOWN" ? u.manufacturer : "--";

    // Position
    var posStr = "--";
    var posMgrs = "--";
    if (u.latitude != null && u.longitude != null) {
        posStr = Number(u.latitude).toFixed(5) + ", " + Number(u.longitude).toFixed(5);
        posMgrs = MGRS.forward(u.latitude, u.longitude, 5);
    }

    // Operator position
    var opPosStr = "--";
    var opPosMgrs = "--";
    if (u.operator_latitude != null && u.operator_longitude != null) {
        opPosStr = Number(u.operator_latitude).toFixed(5) + ", " + Number(u.operator_longitude).toFixed(5);
        opPosMgrs = MGRS.forward(u.operator_latitude, u.operator_longitude, 5);
    }

    // Altitude
    var altStr = u.altitude_geodetic != null ? Number(u.altitude_geodetic).toFixed(1) + " m" : "--";
    var altPStr = u.altitude_pressure != null ? Number(u.altitude_pressure).toFixed(1) + " m" : null;

    // Speed
    var spdStr = u.speed != null ? Number(u.speed).toFixed(1) + " m/s" : "--";

    // Vertical speed
    var vsStr = u.vertical_speed != null ? Number(u.vertical_speed).toFixed(1) + " m/s" : "--";

    // Heading
    var hdgStr = u.ground_track != null ? Number(u.ground_track).toFixed(0) + "\u00B0" : "--";

    // RSSI
    var rssiStr = u.rssi != null ? u.rssi + " dBm" : "--";
    var rssiClass = "";
    if (u.rssi != null) {
        rssiClass = u.rssi > -50 ? "good" : u.rssi < -80 ? "bad" : "";
    }

    // Type info
    var typeStr = u.uav_type || "--";

    // Serial
    var serialStr = u.serial_number || "--";

    // Detection source
    var srcStr = u.detection_source || "--";
    var srcBadge = buildSourceBadge(u.detection_source);

    // MAC address
    var macStr = u.mac || "--";

    // Operator
    var opStr = u.operator_id || "--";
    var opLocType = u.operator_location_type || "";

    // Operational status (raw from drone)
    var opStatusStr = u.operational_status || "--";

    // Timestamp
    var tsStr = u.timestamp ? timeAgo(u.timestamp) : "--";

    // Tap - show tap name if available, otherwise ID
    var tapId = u.tap_id || "";
    var tapStr = "--";
    if (tapId) {
        // Try to find tap name from global taps list
        var tapInfo = (window.fleetTaps || []).find(function(t) { return t.tap_uuid === tapId; });
        if (tapInfo && tapInfo.tap_name) {
            tapStr = tapInfo.tap_name;
        } else {
            tapStr = tapId;
        }
    }

    // Taps seeing (array from merger, or 1 if we have a tap_id)
    var tapsSeeing = u._taps_seeing;
    var tapsStr = "--";
    if (tapsSeeing && Array.isArray(tapsSeeing)) {
        tapsStr = tapsSeeing.length + " tap" + (tapsSeeing.length !== 1 ? "s" : "");
    } else if (typeof tapsSeeing === "number" && tapsSeeing > 0) {
        tapsStr = tapsSeeing + " tap" + (tapsSeeing !== 1 ? "s" : "");
    } else if (tapId) {
        tapsStr = "1 tap";
    }

    // Movement and distance (from RSSI tracker)
    var movStr = u.movement || "--";
    var movClass = "";
    if (u.movement === "approaching") movClass = "bad";
    else if (u.movement === "departing") movClass = "good";

    var distStr = u.distance_est_m != null ? fmtDist(u.distance_est_m) : "--";

    // Spoof flags
    var flags = u.spoof_flags || [];
    var flagsStr = flags.length ? flags.join(", ") : "None";
    var flagsClass = flags.length > 1 ? "bad" : flags.length === 1 ? "warn" : "good";

    // Fingerprint confidence
    var fpConf = u.fingerprint_confidence || 0;
    var fpStr = fpConf > 0 ? fpConf + "%" : "--";
    var fpClass = fpConf >= 70 ? "good" : fpConf >= 40 ? "warn" : "";
    // Fingerprint reasons
    var fpReasons = u.fingerprint_reasons || [];

    // SSID
    var ssidStr = u.ssid || "--";

    // Classification - use server-side classification if available
    var classStr = u.classification && u.classification !== "UNKNOWN" && u.classification !== "unknown"
        ? u.classification.replace(/_/g, " ")
        : (u._classification || "unknown").replace(/_/g, " ");

    // Trust bar width
    var trustBarWidth = Math.max(0, Math.min(100, trust));

    // Registration / UTM / Session IDs (sanitize to hide binary garbage)
    var regStr = u.registration || null;
    var utmStr = sanitizeID(u.utm_id);
    var sessStr = sanitizeID(u.session_id);

    // Build card inner HTML (no outer wrapper)
    var html = '';

    // -- Header --
    html += '<div class="uav-card-hdr">';
    html += '<div class="uav-card-id">';
    html += '<span class="uav-status-dot ' + esc(status) + '"></span>';
    html += '<div style="min-width:0">';
    html += '<div class="uav-designation">' + esc(name) + '</div>';
    html += '<div class="uav-ident">' + esc(ident);
    html += '<span class="uav-class-tag">' + esc(classStr) + '</span>';
    html += '</div>';
    html += '</div></div>';

    // Tag badge (if set)
    var uavTag = u.tag || '';
    if (uavTag) {
        var tagColors = { friendly: '#00E676', suspicious: '#FF6D00', hostile: '#FF1744', monitored: '#AA00FF', vip: '#FF4081', ignored: '#78909C' };
        var tc = tagColors[uavTag] || '#78909C';
        html += '<span class="fleet-tag-badge" style="color:' + tc + ';background:color-mix(in srgb, ' + tc + ' 15%, transparent);border:1px solid color-mix(in srgb, ' + tc + ' 30%, transparent)">' + esc(uavTag.toUpperCase()) + '</span>';
    }

    // Trust badge with numeric score
    html += '<div class="uav-trust-wrap">';
    html += '<span class="uav-trust-badge" style="background:' + trustBg + ';color:' + trustColor + '">';
    html += 'TRUST ' + trust + '</span>';
    html += '<div class="uav-trust-bar"><div class="uav-trust-fill" style="width:' + trustBarWidth + '%;background:' + trustColor + '"></div></div>';
    html += '</div>';

    html += '</div>'; // end header

    // -- Status ribbon --
    html += '<div class="uav-ribbon">';
    html += '<span class="uav-ribbon-status ' + esc(status) + '">' + esc(statusLabel(status)) + '</span>';
    html += srcBadge;
    if (u.movement && u.movement !== "stable" && u.movement !== "unknown") {
        html += '<span class="uav-ribbon-tag ' + movClass + '">' + esc(movStr) + '</span>';
    }
    if (flags.length > 0) {
        html += '<span class="uav-ribbon-tag bad">SPOOF</span>';
    }
    html += '</div>';

    // -- Body: detail grid --
    html += '<div class="uav-card-body"><div class="uav-detail-grid">';

    var modelStr = (u.model || "--").replace(" (Unknown)", "");

    // Identity group
    html += uavField("Manufacturer", mfgStr);
    html += uavField("Model", modelStr);
    if (serialStr !== "--") html += uavField("Serial", serialStr);
    if (typeStr !== "--") html += uavField("Type", typeStr);

    // Divider
    html += '<div class="uav-card-divider"></div>';

    // Flight group
    html += uavField("Position", posStr);
    html += uavField("MGRS", posMgrs);
    html += uavField("Alt (Geo)", altStr);
    if (altPStr) html += uavField("Alt (Press)", altPStr);
    html += uavField("Speed", spdStr);
    if (vsStr !== "--") html += uavField("V/Speed", vsStr);
    html += uavField("Heading", hdgStr);

    // Divider
    html += '<div class="uav-card-divider"></div>';

    // Signal group
    html += uavFieldClass("RSSI", rssiStr, rssiClass);
    if (ssidStr !== "--") html += uavField("SSID", ssidStr);
    html += uavFieldClass("Movement", movStr, movClass);
    if (distStr !== "--") html += uavField("Distance Est", distStr);
    html += uavFieldClass("Spoof Flags", flagsStr, flagsClass);
    if (fpStr !== "--") html += uavFieldClass("Fingerprint", fpStr, fpClass);

    // Divider (only if operator data exists)
    if (opStr !== "--" || opPosStr !== "--") {
        html += '<div class="uav-card-divider"></div>';
        // Operator group
        if (opStr !== "--") html += uavField("Operator", opStr);
        if (opPosStr !== "--") {
            html += uavField("Op. Position", opPosStr);
            html += uavField("Op. MGRS", opPosMgrs);
        }
    }

    // Divider
    html += '<div class="uav-card-divider"></div>';

    // Meta group
    html += uavField("Last Seen", tsStr);
    html += uavField("Taps Seeing", tapsStr);
    html += uavField("Detected By", tapStr);
    if (macStr !== "--") html += uavField("MAC", macStr);
    if (regStr) html += uavField("Registration", regStr);

    html += '</div>'; // end detail-grid

    // Fingerprint reasons (if any)
    if (fpReasons.length > 0) {
        html += '<div class="uav-fp-reasons">';
        html += '<span class="uav-field-label">Fingerprint reasons:</span> ';
        html += '<span class="uav-field-val" style="font-size:10px">' + esc(fpReasons.join("; ")) + '</span>';
        html += '</div>';
    }

    // Spoof flag details
    if (flags.length > 0) {
        html += '<div class="uav-spoof-detail">';
        html += '<span class="uav-field-label" style="color:var(--critical)">Spoof flags:</span> ';
        html += '<span class="uav-field-val bad" style="font-size:10px">' + esc(flags.join(", ")) + '</span>';
        html += '</div>';
    }

    html += '</div>'; // end card-body

    // Card footer with TAG and CLASSIFY buttons
    var uavClass = (u.classification || 'UNKNOWN').toUpperCase();
    html += '<div class="uav-card-footer">';
    // Classification buttons
    html += '<div class="classify-btns">';
    html += '<span class="classify-label">Classify:</span>';
    html += '<button class="classify-btn friendly' + (uavClass === 'FRIENDLY' ? ' active' : '') + '" onclick="fleetClassify(\'' + escJs(ident) + '\', \'FRIENDLY\')">F</button>';
    html += '<button class="classify-btn neutral' + (uavClass === 'NEUTRAL' ? ' active' : '') + '" onclick="fleetClassify(\'' + escJs(ident) + '\', \'NEUTRAL\')">N</button>';
    html += '<button class="classify-btn hostile' + (uavClass === 'HOSTILE' ? ' active' : '') + '" onclick="fleetClassify(\'' + escJs(ident) + '\', \'HOSTILE\')">H</button>';
    html += '<button class="classify-btn unknown' + (uavClass === 'UNKNOWN' ? ' active' : '') + '" onclick="fleetClassify(\'' + escJs(ident) + '\', \'UNKNOWN\')">?</button>';
    html += '</div>';
    // Flight path button
    html += '<button class="fleet-tag-btn fleet-path-btn" onclick="fleetShowPath(\'' + escJs(ident) + '\', this)">SHOW PATH</button>';
    // Tag button
    html += '<button class="fleet-tag-btn" onclick="fleetSetTag(\'' + escJs(ident) + '\')">TAG</button>';
    if (uavTag) {
        html += '<button class="fleet-tag-btn fleet-tag-clear" onclick="fleetClearTag(\'' + escJs(ident) + '\')">CLEAR TAG</button>';
    }
    html += '</div>';

    return html;
}

function buildSourceBadge(source) {
    if (!source) return "";
    var cls = "src-default";
    if (source === "RemoteIdWiFi") cls = "src-remoteid";
    else if (source === "RemoteIdBLE") cls = "src-remoteid";
    else if (source === "Bluetooth 4" || source === "Bluetooth 5") cls = "src-ble";
    else if (source === "DJIDroneID") cls = "src-dji";
    else if (source === "WiFiFingerprint") cls = "src-wifi";
    else if (source === "TestInjection") cls = "src-test";
    return '<span class="uav-source-badge ' + cls + '">' + esc(source) + '</span>';
}

function uavField(label, val) {
    return '<div class="uav-field">' +
        '<span class="uav-field-label">' + esc(label) + '</span>' +
        '<span class="uav-field-val">' + esc(String(val)) + '</span>' +
    '</div>';
}

function uavFieldClass(label, val, cls) {
    return '<div class="uav-field">' +
        '<span class="uav-field-label">' + esc(label) + '</span>' +
        '<span class="uav-field-val' + (cls ? " " + cls : "") + '">' + esc(String(val)) + '</span>' +
    '</div>';
}

function statusLabel(s) {
    if (s === "airborne") return "Airborne";
    if (s === "grounded") return "Grounded";
    if (s === "lost") return "Lost";
    return s || "Unknown";
}

// --- TRUST SCORE COLORS ---
// Green >= 80, Yellow 50-79, Orange 30-49, Red < 30
function trustBadgeColor(score) {
    if (score < 30) return "#FF1744";
    if (score < 50) return "#FF6D00";
    if (score < 80) return "#FFB300";
    return "#00E676";
}

function trustBadgeBg(score) {
    if (score < 30) return "rgba(255,23,68,0.12)";
    if (score < 50) return "rgba(255,109,0,0.10)";
    if (score < 80) return "rgba(255,179,0,0.10)";
    return "rgba(0,230,118,0.10)";
}

// --- TIME / DISTANCE UTILITIES ---
function timeAgo(ts) {
    if (!ts) return "--";
    var diff = (Date.now() - new Date(ts).getTime()) / 1000;
    if (isNaN(diff)) return "--";
    if (diff < 0) return "just now";
    if (diff < 60) return Math.floor(diff) + "s ago";
    if (diff < 3600) return Math.floor(diff / 60) + "m ago";
    if (diff < 86400) return Math.floor(diff / 3600) + "h" + Math.floor((diff % 3600) / 60) + "m ago";
    var days = Math.floor(diff / 86400);
    var hours = Math.floor((diff % 86400) / 3600);
    return days + "d" + hours + "h ago";
}

function fmtDist(m) {
    if (m == null) return "--";
    if (m < 1000) return Math.round(m) + " m";
    return (m / 1000).toFixed(1) + " km";
}

function fmtDuration(secs) {
    secs = Math.floor(secs);
    if (secs < 60) return secs + "s";
    if (secs < 3600) return Math.floor(secs / 60) + "m " + (secs % 60) + "s";
    var h = Math.floor(secs / 3600);
    var m = Math.floor((secs % 3600) / 60);
    return h + "h " + m + "m";
}

// --- TAG SUPPORT ---
// Fleet page runs as a standalone IIFE, so it has its own tag functions
// exposed on window for onclick handlers in card HTML.
var TAG_OPTIONS = ["friendly", "suspicious", "hostile", "monitored", "vip", "ignored"];

function fleetShowToast(msg, type) {
    var container = document.getElementById("fleet-toast-container");
    if (!container) {
        container = document.createElement("div");
        container.id = "fleet-toast-container";
        container.className = "nz-toast-container";
        document.body.appendChild(container);
    }
    var toast = document.createElement("div");
    toast.className = "nz-toast " + (type || "success");
    toast.textContent = msg;
    container.appendChild(toast);
    setTimeout(function () {
        toast.classList.add("fade-out");
        setTimeout(function () { toast.remove(); }, 300);
    }, 3000);
}

function fleetSetTag(droneId) {
    var tag = prompt("Enter tag for " + droneId + ":\n(" + TAG_OPTIONS.join(", ") + ")");
    if (tag === null) return; // cancelled
    tag = tag.trim().toLowerCase();
    if (tag && TAG_OPTIONS.indexOf(tag) === -1) {
        fleetShowToast("Invalid tag. Use: " + TAG_OPTIONS.join(", "), "error");
        return;
    }
    SkylensAuth.fetch("/api/uav/" + encodeURIComponent(droneId) + "/tag", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ tag: tag || null })
    }).then(function (r) { return r.json(); }).then(function (d) {
        if (d.ok) {
            fleetShowToast(tag ? "Tagged " + droneId + " as " + tag.toUpperCase() : "Tag cleared for " + droneId, "success");
            poll();
        } else {
            fleetShowToast("Tag failed: " + (d.error || "unknown error"), "error");
        }
    }).catch(function (e) {
        fleetShowToast("Tag failed: " + e.message, "error");
    });
}

function fleetClearTag(droneId) {
    SkylensAuth.fetch("/api/uav/" + encodeURIComponent(droneId) + "/tag", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ tag: null })
    }).then(function (r) { return r.json(); }).then(function (d) {
        if (d.ok) {
            fleetShowToast("Tag cleared for " + droneId, "success");
            poll();
        } else {
            fleetShowToast("Clear tag failed: " + (d.error || "unknown error"), "error");
        }
    }).catch(function (e) {
        fleetShowToast("Clear tag failed: " + e.message, "error");
    });
}

// Classify drone as FRIENDLY, HOSTILE, NEUTRAL, or UNKNOWN
function fleetClassify(droneId, classification) {
    SkylensAuth.fetch("/api/uav/" + encodeURIComponent(droneId) + "/classify", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ classification: classification })
    }).then(function (r) { return r.json(); }).then(function (d) {
        if (d.ok) {
            var labels = { FRIENDLY: "Friendly", HOSTILE: "Hostile", NEUTRAL: "Neutral", UNKNOWN: "Unknown" };
            fleetShowToast("Classified " + droneId + " as " + (labels[classification] || classification), "success");
            poll();
        } else {
            fleetShowToast("Classification failed: " + (d.error || "unknown error"), "error");
        }
    }).catch(function (e) {
        fleetShowToast("Classification failed: " + e.message, "error");
    });
}

// Expose to window for onclick handlers in card HTML
window.fleetSetTag = fleetSetTag;
window.fleetClearTag = fleetClearTag;
window.fleetClassify = fleetClassify;

// --- SHOW FLIGHT PATH ---
function fleetShowPath(droneId, btn) {
    // NzMap is only available on the Airspace page; redirect if needed
    if (typeof NzMap === "undefined" || typeof NzMap.showFlightPath !== "function") {
        // Open in new tab or redirect to airspace page
        window.open("/?focus=" + encodeURIComponent(droneId) + "&showPath=1", "_blank");
        return;
    }
    NzMap.showFlightPath(droneId, function (showing) {
        if (btn) btn.textContent = showing ? "HIDE PATH" : "SHOW PATH";
        if (showing) {
            fleetShowToast("Showing flight path for " + droneId, "success");
        } else {
            fleetShowToast("Hiding flight path", "info");
        }
    });
}
window.fleetShowPath = fleetShowPath;

// --- START ---
document.addEventListener("DOMContentLoaded", init);

})();
