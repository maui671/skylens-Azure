/* ═══════════════════════════════════════════════════════════════
   SKYLENS COMMAND CENTER v2.0
   Performance-optimized single-page application
   ═══════════════════════════════════════════════════════════════ */

(function() {
'use strict';

// ─── STATE ───
const State = {
    uavs: [],
    taps: [],
    alerts: [],
    stats: {},
    uptime: '--',
    version: '--',
    connected: false,
    selectedDrone: null,
    activeTab: 'contacts',
    soundEnabled: true,
    pollInterval: 1000,
    userLocation: null
};

// ─── CACHE DOM ELEMENTS ───
const DOM = {};

// ─── MAP ───
let map = null;
let markers = {};
let trails = {};
let tapMarkers = {};

// ─── POLLING ───
let pollTimer = null;
let pollInFlight = false;

// ─── INIT ───
function init() {
    cacheDOMElements();
    initMap();
    initEventListeners();
    initClock();
    initGeolocation();
    startPolling();
}

function cacheDOMElements() {
    // Cache frequently accessed elements
    DOM.app = document.getElementById('app');
    DOM.clock = document.getElementById('clock');
    DOM.connDot = document.getElementById('conn-dot');
    DOM.threatLevel = document.getElementById('threat-level');
    DOM.threatText = document.getElementById('threat-text');
    DOM.statUavs = document.getElementById('stat-uavs');
    DOM.statSensors = document.getElementById('stat-sensors');
    DOM.statFrames = document.getElementById('stat-frames');
    DOM.contactList = document.getElementById('contact-list');
    DOM.contactCount = document.getElementById('contact-count');
    DOM.emptyContacts = document.getElementById('empty-contacts');
    DOM.sensorList = document.getElementById('sensor-list');
    DOM.emptySensors = document.getElementById('empty-sensors');
    DOM.alertList = document.getElementById('alert-list');
    DOM.emptyAlerts = document.getElementById('empty-alerts');
    DOM.alertBadge = document.getElementById('alert-badge');
    DOM.fleetBadge = document.getElementById('fleet-badge');
    DOM.detailView = document.getElementById('detail-view');
    DOM.tabContacts = document.getElementById('tab-contacts');
    DOM.tabSensors = document.getElementById('tab-sensors');
    DOM.tabAlerts = document.getElementById('tab-alerts');
    DOM.rightPanel = document.getElementById('right-panel');
    DOM.statusUptime = document.getElementById('status-uptime');
    DOM.statusVersion = document.getElementById('status-version');
    DOM.statusRate = document.getElementById('status-rate');
    DOM.statusTailscale = document.getElementById('status-tailscale');
    DOM.statusTsIp = document.getElementById('status-ts-ip');
    DOM.hudCoords = document.getElementById('hud-coords');
    DOM.hudZoom = document.getElementById('hud-zoom');
    DOM.modalBackdrop = document.getElementById('modal-backdrop');
    DOM.toastContainer = document.getElementById('toast-container');
}

// ─── MAP INITIALIZATION ───
function initMap() {
    var lat = parseFloat(localStorage.getItem('skylens_map_center_lat')) || 18.2528;
    var lng = parseFloat(localStorage.getItem('skylens_map_center_lng')) || -65.6433;
    var zoom = parseInt(localStorage.getItem('skylens_map_zoom')) || 14;

    map = L.map('map', {
        center: [lat, lng],
        zoom: zoom,
        zoomControl: true,
        attributionControl: true
    });

    // Satellite tiles (default)
    L.tileLayer('https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}', {
        attribution: 'ESRI',
        maxZoom: 19
    }).addTo(map);

    // Map events
    map.on('moveend', updateHUD);
    map.on('zoomend', updateHUD);
    map.on('mousemove', function(e) {
        DOM.hudCoords.textContent = e.latlng.lat.toFixed(5) + ', ' + e.latlng.lng.toFixed(5) + '  |  ' + MGRS.forward(e.latlng.lat, e.latlng.lng, 4);
    });

    updateHUD();
}

function updateHUD() {
    DOM.hudZoom.textContent = 'Zoom: ' + map.getZoom();
}

// ─── EVENT LISTENERS ───
function initEventListeners() {
    // Tab switching
    document.querySelectorAll('.panel-tab').forEach(tab => {
        tab.addEventListener('click', function() {
            switchTab(this.dataset.tab);
        });
    });

    // Nav items
    document.querySelectorAll('.nav-item').forEach(item => {
        item.addEventListener('click', function(e) {
            e.preventDefault();
            document.querySelectorAll('.nav-item').forEach(i => i.classList.remove('active'));
            this.classList.add('active');
        });
    });

    // Contact list click delegation
    DOM.contactList.addEventListener('click', function(e) {
        const card = e.target.closest('.contact-card');
        if (card) {
            selectDrone(card.dataset.id);
        }
    });

    // Detail close
    document.getElementById('detail-close').addEventListener('click', function() {
        selectDrone(null);
    });

    // Detail buttons
    document.getElementById('btn-locate').addEventListener('click', function() {
        if (State.selectedDrone) {
            const uav = State.uavs.find(u => getId(u) === State.selectedDrone);
            if (uav && uav.latitude && uav.longitude) {
                map.setView([uav.latitude, uav.longitude], 17);
            }
        }
    });

    document.getElementById('btn-hide').addEventListener('click', function() {
        if (State.selectedDrone) {
            hideDrone(State.selectedDrone);
        }
    });

    // Sound toggle
    document.getElementById('btn-sound').addEventListener('click', function() {
        State.soundEnabled = !State.soundEnabled;
        this.style.opacity = State.soundEnabled ? '1' : '0.5';
    });

    // Fullscreen
    document.getElementById('btn-fullscreen').addEventListener('click', function() {
        DOM.app.classList.toggle('panel-hidden');
        setTimeout(() => map.invalidateSize(), 200);
    });

    // Modal close buttons
    document.querySelectorAll('[data-close]').forEach(btn => {
        btn.addEventListener('click', closeModals);
    });

    DOM.modalBackdrop.addEventListener('click', closeModals);

    // Keyboard shortcuts
    document.addEventListener('keydown', handleKeyboard);
}

function handleKeyboard(e) {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT') return;

    switch(e.key) {
        case 'Escape':
            if (State.selectedDrone) {
                selectDrone(null);
            }
            closeModals();
            break;
        case '1':
            switchTab('contacts');
            break;
        case '2':
            switchTab('sensors');
            break;
        case '3':
            switchTab('alerts');
            break;
        case 'f':
        case 'F':
            fitAllMarkers();
            break;
    }
}

function switchTab(tabName) {
    State.activeTab = tabName;

    // Update tab buttons
    document.querySelectorAll('.panel-tab').forEach(t => {
        t.classList.toggle('active', t.dataset.tab === tabName);
    });

    // Show/hide content
    DOM.tabContacts.classList.toggle('hidden', tabName !== 'contacts');
    DOM.tabSensors.classList.toggle('hidden', tabName !== 'sensors');
    DOM.tabAlerts.classList.toggle('hidden', tabName !== 'alerts');

    // Hide detail view when switching tabs
    if (tabName !== 'contacts') {
        DOM.detailView.classList.add('hidden');
        DOM.contactList.classList.remove('hidden');
    }
}

// ─── CLOCK ───
function initClock() {
    function tick() {
        const now = new Date();
        const h = String(now.getHours()).padStart(2, '0');
        const m = String(now.getMinutes()).padStart(2, '0');
        const s = String(now.getSeconds()).padStart(2, '0');
        DOM.clock.textContent = h + ':' + m + ':' + s;
    }
    tick();
    setInterval(tick, 1000);
}

// ─── GEOLOCATION ───
function initGeolocation() {
    if (!navigator.geolocation) return;

    navigator.geolocation.watchPosition(
        function(pos) {
            State.userLocation = {
                lat: pos.coords.latitude,
                lng: pos.coords.longitude
            };
        },
        function() {},
        { enableHighAccuracy: false, timeout: 10000, maximumAge: 30000 }
    );
}

// ─── POLLING ───
function startPolling() {
    poll();
    pollTimer = setInterval(poll, State.pollInterval);
}

function poll() {
    if (pollInFlight) return;
    pollInFlight = true;

    fetch('/api/status')
        .then(r => {
            if (!r.ok) throw new Error('HTTP ' + r.status);
            return r.json();
        })
        .then(data => {
            State.connected = true;
            State.stats = data.stats || {};
            State.taps = data.taps || [];
            State.uavs = data.uavs || [];
            State.alerts = data.alerts_history || [];
            State.uptime = data.uptime || '--';
            State.version = data.version || '--';

            updateUI();
            updateMap();
        })
        .catch(err => {
            State.connected = false;
            updateConnectionStatus();
        })
        .finally(() => {
            pollInFlight = false;
        });
}

// ─── UI UPDATES ───
function updateUI() {
    updateConnectionStatus();
    updateThreatLevel();
    updateStats();
    updateContactList();
    updateSensorList();
    updateAlertList();
    updateStatusBar();
    updateBadges();

    if (State.selectedDrone) {
        updateDetailView();
    }
}

function updateConnectionStatus() {
    DOM.connDot.classList.toggle('offline', !State.connected);
}

function updateThreatLevel() {
    const lowTrust = State.uavs.filter(u => (u.trust_score || 100) < 50).length;
    const medTrust = State.uavs.filter(u => {
        const t = u.trust_score || 100;
        return t >= 50 && t < 80;
    }).length;

    let level = 'low';
    let text = 'LOW';

    if (lowTrust > 0) {
        level = 'critical';
        text = 'CRITICAL';
    } else if (medTrust > 0) {
        level = 'medium';
        text = 'ELEVATED';
    } else if (State.uavs.length > 0) {
        level = 'low';
        text = 'ACTIVE';
    }

    DOM.threatLevel.className = 'threat-level ' + level;
    DOM.threatText.textContent = text;
}

function updateStats() {
    const activeUavs = State.uavs.filter(u => u._contactStatus !== 'lost').length;
    const onlineTaps = State.taps.filter(t => {
        const age = tapAge(t.timestamp);
        return age < 60;
    }).length;

    let totalFrames = 0;
    State.taps.forEach(t => {
        totalFrames += t.frames_total || t.packets_captured || 0;
    });

    DOM.statUavs.textContent = activeUavs;
    DOM.statSensors.textContent = onlineTaps + '/' + State.taps.length;
    DOM.statFrames.textContent = formatNumber(totalFrames);
}

function updateContactList() {
    const uavs = State.uavs;

    if (uavs.length === 0) {
        DOM.emptyContacts.classList.remove('hidden');
        // Clear any existing cards
        const cards = DOM.contactList.querySelectorAll('.contact-card');
        cards.forEach(c => c.remove());
        DOM.contactCount.textContent = '0';
        return;
    }

    DOM.emptyContacts.classList.add('hidden');
    DOM.contactCount.textContent = uavs.length;

    // Build HTML efficiently
    const fragment = document.createDocumentFragment();

    uavs.forEach(uav => {
        const id = getId(uav);
        const trust = uav.trust_score != null ? uav.trust_score : 100;
        const trustClass = trust >= 80 ? 'high' : (trust >= 30 ? 'med' : 'low');
        const isLost = uav._contactStatus === 'lost';
        const isSelected = State.selectedDrone === id;

        const card = document.createElement('div');
        card.className = 'contact-card trust-' + trustClass;
        if (isLost) card.classList.add('lost');
        if (isSelected) card.classList.add('selected');
        card.dataset.id = id;

        const name = uav.designation || uav.model || id.substring(0, 12);
        const alt = uav.altitude != null ? Math.round(uav.altitude) + 'm' : '--';
        const speed = uav.speed != null ? uav.speed.toFixed(1) + ' m/s' : '--';
        const rssi = uav.rssi != null ? uav.rssi + ' dBm' : '--';

        let flagsHTML = '';
        if (!uav.has_rid) {
            flagsHTML += '<span class="contact-flag">NO RID</span>';
        }
        if (uav.spoof_score && uav.spoof_score > 50) {
            flagsHTML += '<span class="contact-flag">SPOOF</span>';
        }

        card.innerHTML = `
            <div class="contact-header">
                <span class="contact-name">${escapeHtml(name)}</span>
                <span class="contact-trust ${trustClass}">${Math.round(trust)}%</span>
            </div>
            <div class="contact-meta">
                <span class="contact-stat">${alt}</span>
                <span class="contact-stat">${speed}</span>
                <span class="contact-stat">${rssi}</span>
            </div>
            ${flagsHTML ? '<div class="contact-flags">' + flagsHTML + '</div>' : ''}
        `;

        fragment.appendChild(card);
    });

    // Clear and append in one operation
    const existingCards = DOM.contactList.querySelectorAll('.contact-card');
    existingCards.forEach(c => c.remove());
    DOM.contactList.appendChild(fragment);
}

function updateSensorList() {
    const taps = State.taps;

    if (taps.length === 0) {
        DOM.emptySensors.classList.remove('hidden');
        const cards = DOM.sensorList.querySelectorAll('.sensor-card');
        cards.forEach(c => c.remove());
        return;
    }

    DOM.emptySensors.classList.add('hidden');

    const fragment = document.createDocumentFragment();

    taps.forEach(tap => {
        const age = tapAge(tap.timestamp);
        const isOnline = age < 60;
        const isWarning = age >= 30 && age < 60;

        const card = document.createElement('div');
        card.className = 'sensor-card';
        if (!isOnline) card.classList.add('offline');
        else if (isWarning) card.classList.add('warning');

        const cpu = tap.cpu_percent != null ? tap.cpu_percent.toFixed(1) : '--';
        const mem = tap.memory_percent != null ? tap.memory_percent.toFixed(1) : '--';
        const temp = tap.temperature != null ? tap.temperature.toFixed(1) : '--';
        const frames = formatNumber(tap.frames_total || tap.packets_captured || 0);
        const channel = tap.current_channel || tap.channel || '--';

        const cpuClass = tap.cpu_percent > 85 ? 'danger' : (tap.cpu_percent > 70 ? 'warning' : 'safe');
        const memClass = tap.memory_percent > 85 ? 'danger' : (tap.memory_percent > 70 ? 'warning' : 'safe');
        const tempClass = tap.temperature > 85 ? 'danger' : (tap.temperature > 70 ? 'warning' : 'safe');

        card.innerHTML = `
            <div class="sensor-header">
                <span class="sensor-name">${escapeHtml(tap.tap_name || tap.tap_uuid || 'Unknown')}</span>
                <span class="sensor-status ${isOnline ? 'online' : 'offline'}">${isOnline ? 'ONLINE' : 'OFFLINE'}</span>
            </div>
            <div class="sensor-stats">
                <div class="sensor-stat">
                    <span class="sensor-stat-label">Channel</span>
                    <span class="sensor-stat-value">${channel}</span>
                </div>
                <div class="sensor-stat">
                    <span class="sensor-stat-label">Frames</span>
                    <span class="sensor-stat-value">${frames}</span>
                </div>
            </div>
            <div class="sensor-meters">
                <div class="sensor-meter">
                    <span class="sensor-meter-label">CPU</span>
                    <div class="sensor-meter-bar">
                        <div class="sensor-meter-fill ${cpuClass}" style="width:${Math.min(tap.cpu_percent || 0, 100)}%"></div>
                    </div>
                    <span class="sensor-meter-value">${cpu}%</span>
                </div>
                <div class="sensor-meter">
                    <span class="sensor-meter-label">MEM</span>
                    <div class="sensor-meter-bar">
                        <div class="sensor-meter-fill ${memClass}" style="width:${Math.min(tap.memory_percent || 0, 100)}%"></div>
                    </div>
                    <span class="sensor-meter-value">${mem}%</span>
                </div>
                <div class="sensor-meter">
                    <span class="sensor-meter-label">TEMP</span>
                    <div class="sensor-meter-bar">
                        <div class="sensor-meter-fill ${tempClass}" style="width:${Math.min((tap.temperature || 0) / 100 * 100, 100)}%"></div>
                    </div>
                    <span class="sensor-meter-value">${temp}C</span>
                </div>
            </div>
        `;

        fragment.appendChild(card);
    });

    const existingCards = DOM.sensorList.querySelectorAll('.sensor-card');
    existingCards.forEach(c => c.remove());
    DOM.sensorList.appendChild(fragment);
}

function updateAlertList() {
    const alerts = State.alerts.slice(0, 20); // Show max 20

    if (alerts.length === 0) {
        DOM.emptyAlerts.classList.remove('hidden');
        const items = DOM.alertList.querySelectorAll('.alert-item');
        items.forEach(i => i.remove());
        return;
    }

    DOM.emptyAlerts.classList.add('hidden');

    const fragment = document.createDocumentFragment();

    alerts.forEach(alert => {
        const item = document.createElement('div');
        item.className = 'alert-item';

        const time = alert.timestamp ? formatTime(alert.timestamp) : '--:--';
        const priority = (alert.priority || 'LOW').toUpperCase();
        const priClass = priority.toLowerCase();

        item.innerHTML = `
            <span class="alert-time">${time}</span>
            <span class="alert-priority ${priClass}">${priority}</span>
            <span class="alert-message">${escapeHtml(alert.message || alert.type || 'Alert')}</span>
            <button class="alert-ack" data-id="${alert.id || ''}">ACK</button>
        `;

        fragment.appendChild(item);
    });

    const existingItems = DOM.alertList.querySelectorAll('.alert-item');
    existingItems.forEach(i => i.remove());
    DOM.alertList.appendChild(fragment);
}

function updateStatusBar() {
    DOM.statusUptime.textContent = State.uptime;
    DOM.statusVersion.textContent = State.version;

    // Calculate rate from taps
    let totalPPS = 0;
    State.taps.forEach(t => {
        totalPPS += t.packets_per_second || 0;
    });
    DOM.statusRate.textContent = totalPPS.toFixed(0) + '/s';
}

function updateBadges() {
    const unackedAlerts = State.alerts.filter(a => !a.acknowledged).length;
    DOM.alertBadge.textContent = unackedAlerts;
    DOM.alertBadge.classList.toggle('hidden', unackedAlerts === 0);

    const uavCount = State.uavs.length;
    DOM.fleetBadge.textContent = uavCount;
    DOM.fleetBadge.classList.toggle('hidden', uavCount === 0);
}

// ─── DETAIL VIEW ───
function updateDetailView() {
    const uav = State.uavs.find(u => getId(u) === State.selectedDrone);
    if (!uav) {
        selectDrone(null);
        return;
    }

    const id = getId(uav);
    const trust = uav.trust_score != null ? uav.trust_score : 100;
    const alt = uav.altitude != null ? Math.round(uav.altitude) : 0;
    const speed = uav.speed != null ? uav.speed : 0;

    // Update header
    document.getElementById('detail-name').textContent = uav.designation || uav.model || id.substring(0, 16);
    document.getElementById('detail-id').textContent = id;

    // Update gauges
    updateGauge('gauge-trust', trust, 100);
    document.getElementById('gauge-trust-val').textContent = Math.round(trust);

    updateGauge('gauge-alt', alt, 500);
    document.getElementById('gauge-alt-val').textContent = alt;

    updateGauge('gauge-speed', speed, 30);
    document.getElementById('gauge-speed-val').textContent = speed.toFixed(1);

    // Update position
    document.getElementById('detail-lat').textContent = uav.latitude != null ? uav.latitude.toFixed(6) : '--';
    document.getElementById('detail-lng').textContent = uav.longitude != null ? uav.longitude.toFixed(6) : '--';
    document.getElementById('detail-mgrs').textContent = MGRS.forward(uav.latitude, uav.longitude, 5);
    document.getElementById('detail-heading').textContent = uav.heading != null ? Math.round(uav.heading) + '°' : '--';

    // Distance from user
    if (State.userLocation && uav.latitude && uav.longitude) {
        const dist = calcDistance(State.userLocation.lat, State.userLocation.lng, uav.latitude, uav.longitude);
        document.getElementById('detail-distance').textContent = formatDistance(dist);
    } else {
        document.getElementById('detail-distance').textContent = '--';
    }

    // Update identity
    document.getElementById('detail-proto').textContent = uav.protocol || '--';
    document.getElementById('detail-type').textContent = uav.ua_type || '--';
    document.getElementById('detail-model').textContent = (uav.model || '--').replace(' (Unknown)', '');
    document.getElementById('detail-rssi').textContent = uav.rssi != null ? uav.rssi + ' dBm' : '--';

    // Operator
    document.getElementById('detail-operator').textContent = uav.operator_id || '--';
    if (uav.operator_latitude && uav.operator_longitude) {
        document.getElementById('detail-op-loc').textContent =
            uav.operator_latitude.toFixed(4) + ', ' + uav.operator_longitude.toFixed(4);
        document.getElementById('detail-op-mgrs').textContent = MGRS.forward(uav.operator_latitude, uav.operator_longitude, 5);
    } else {
        document.getElementById('detail-op-loc').textContent = '--';
        document.getElementById('detail-op-mgrs').textContent = '--';
    }
}

function updateGauge(id, value, max) {
    const gauge = document.getElementById(id);
    if (!gauge) return;

    const circumference = 188.5; // 2 * PI * 30
    const pct = Math.min(value / max, 1);
    const offset = circumference * (1 - pct);
    gauge.style.strokeDashoffset = offset;

    // Color based on value
    gauge.classList.remove('safe', 'caution', 'danger');
    if (id === 'gauge-trust') {
        gauge.classList.add(value >= 80 ? 'safe' : (value >= 30 ? 'caution' : 'danger'));
    } else {
        gauge.classList.add('safe');
    }
}

function selectDrone(id) {
    State.selectedDrone = id;

    // Update contact list selection
    document.querySelectorAll('.contact-card').forEach(card => {
        card.classList.toggle('selected', card.dataset.id === id);
    });

    // Show/hide detail view
    if (id) {
        DOM.detailView.classList.remove('hidden');
        DOM.contactList.classList.add('hidden');
        updateDetailView();

        // Center map on drone
        const uav = State.uavs.find(u => getId(u) === id);
        if (uav && uav.latitude && uav.longitude) {
            map.setView([uav.latitude, uav.longitude], Math.max(map.getZoom(), 15));
        }
    } else {
        DOM.detailView.classList.add('hidden');
        DOM.contactList.classList.remove('hidden');
    }

    // Update map markers
    updateMarkerSelection();
}

function hideDrone(id) {
    fetch('/api/uav/' + encodeURIComponent(id) + '/hide', { method: 'POST' })
        .then(() => poll())
        .catch(console.error);

    if (State.selectedDrone === id) {
        selectDrone(null);
    }

    // Remove from local state
    State.uavs = State.uavs.filter(u => getId(u) !== id);
    updateUI();
    updateMap();
}

// ─── MAP UPDATES ───
function updateMap() {
    const currentIds = new Set(State.uavs.map(u => getId(u)));

    // Remove stale markers
    Object.keys(markers).forEach(id => {
        if (!currentIds.has(id)) {
            map.removeLayer(markers[id]);
            delete markers[id];
            if (trails[id]) {
                map.removeLayer(trails[id]);
                delete trails[id];
            }
        }
    });

    // Update/add markers
    State.uavs.forEach(uav => {
        const id = getId(uav);
        if (uav.latitude == null || uav.longitude == null) return;

        const trust = uav.trust_score != null ? uav.trust_score : 100;
        const color = trust >= 80 ? '#00E676' : (trust >= 50 ? '#FFB300' : (trust >= 30 ? '#FF6D00' : '#FF1744'));
        const isLost = uav._contactStatus === 'lost';
        const isSelected = State.selectedDrone === id;

        if (markers[id]) {
            // Update position
            markers[id].setLatLng([uav.latitude, uav.longitude]);
        } else {
            // Create new marker
            const icon = createDroneIcon(color, isLost, isSelected);
            markers[id] = L.marker([uav.latitude, uav.longitude], { icon: icon })
                .addTo(map)
                .on('click', () => selectDrone(id));
        }

        // Update trail
        if (!trails[id]) {
            trails[id] = L.polyline([], {
                color: color,
                weight: 2,
                opacity: 0.6
            }).addTo(map);
        }

        const points = trails[id].getLatLngs();
        const newPoint = L.latLng(uav.latitude, uav.longitude);
        if (points.length === 0 || !points[points.length - 1].equals(newPoint)) {
            points.push(newPoint);
            if (points.length > 100) points.shift();
            trails[id].setLatLngs(points);
        }
    });

    // Update tap markers
    updateTapMarkers();
}

function createDroneIcon(color, isLost, isSelected) {
    const size = isSelected ? 16 : 12;
    const opacity = isLost ? 0.5 : 1;
    const border = isSelected ? '2px solid #fff' : '1px solid rgba(0,0,0,0.5)';

    return L.divIcon({
        className: 'drone-marker',
        html: `<div style="
            width: ${size}px;
            height: ${size}px;
            background: ${color};
            border: ${border};
            border-radius: 50%;
            opacity: ${opacity};
            box-shadow: 0 0 ${isSelected ? 12 : 6}px ${color};
        "></div>`,
        iconSize: [size, size],
        iconAnchor: [size/2, size/2]
    });
}

function updateMarkerSelection() {
    Object.keys(markers).forEach(id => {
        const uav = State.uavs.find(u => getId(u) === id);
        if (!uav) return;

        const trust = uav.trust_score != null ? uav.trust_score : 100;
        const color = trust >= 80 ? '#00E676' : (trust >= 50 ? '#FFB300' : (trust >= 30 ? '#FF6D00' : '#FF1744'));
        const isLost = uav._contactStatus === 'lost';
        const isSelected = State.selectedDrone === id;

        markers[id].setIcon(createDroneIcon(color, isLost, isSelected));
    });
}

function updateTapMarkers() {
    const currentIds = new Set(State.taps.map(t => t.tap_uuid));

    // Remove stale
    Object.keys(tapMarkers).forEach(id => {
        if (!currentIds.has(id)) {
            map.removeLayer(tapMarkers[id]);
            delete tapMarkers[id];
        }
    });

    // Update/add
    State.taps.forEach(tap => {
        if (!tap.latitude || !tap.longitude || !tap.tap_uuid) return;

        const age = tapAge(tap.timestamp);
        const isOnline = age < 60;
        const color = isOnline ? '#3b82f6' : '#6b7280';

        if (tapMarkers[tap.tap_uuid]) {
            tapMarkers[tap.tap_uuid].setLatLng([tap.latitude, tap.longitude]);
        } else {
            const icon = L.divIcon({
                className: 'tap-marker',
                html: `<div style="
                    width: 10px;
                    height: 10px;
                    background: ${color};
                    border: 1px solid rgba(255,255,255,0.5);
                    border-radius: 2px;
                    transform: rotate(45deg);
                "></div>`,
                iconSize: [10, 10],
                iconAnchor: [5, 5]
            });

            tapMarkers[tap.tap_uuid] = L.marker([tap.latitude, tap.longitude], { icon: icon })
                .addTo(map)
                .bindPopup(`<b>${escapeHtml(tap.tap_name || 'Sensor')}</b><br>Channel: ${tap.current_channel || '--'}`);
        }
    });
}

function fitAllMarkers() {
    const bounds = [];

    Object.values(markers).forEach(m => {
        bounds.push(m.getLatLng());
    });
    Object.values(tapMarkers).forEach(m => {
        bounds.push(m.getLatLng());
    });

    if (bounds.length > 0) {
        map.fitBounds(L.latLngBounds(bounds), { padding: [50, 50] });
    }
}

// ─── MODALS ───
function openModal(id) {
    DOM.modalBackdrop.classList.add('active');
    document.getElementById(id).classList.add('active');
}

function closeModals() {
    DOM.modalBackdrop.classList.remove('active');
    document.querySelectorAll('.modal.active').forEach(m => m.classList.remove('active'));
}

// ─── TOASTS ───
function showToast(message, type = 'success') {
    const toast = document.createElement('div');
    toast.className = 'toast ' + type;
    toast.textContent = message;
    DOM.toastContainer.appendChild(toast);

    setTimeout(() => {
        toast.classList.add('fadeout');
        setTimeout(() => toast.remove(), 200);
    }, 3000);
}

// ─── UTILITIES ───
function getId(uav) {
    return uav.identifier || uav.mac || uav.serial_number || 'unknown';
}

function tapAge(ts) {
    if (!ts) return 999;
    return (Date.now() - new Date(ts).getTime()) / 1000;
}

function formatNumber(n) {
    if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
    return String(n);
}

function formatTime(ts) {
    const d = new Date(ts);
    return String(d.getHours()).padStart(2, '0') + ':' +
           String(d.getMinutes()).padStart(2, '0');
}

function formatDistance(m) {
    if (m >= 1000) return (m / 1000).toFixed(2) + ' km';
    return Math.round(m) + ' m';
}

function calcDistance(lat1, lng1, lat2, lng2) {
    const R = 6371000;
    const dLat = (lat2 - lat1) * Math.PI / 180;
    const dLng = (lng2 - lng1) * Math.PI / 180;
    const a = Math.sin(dLat/2) * Math.sin(dLat/2) +
              Math.cos(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) *
              Math.sin(dLng/2) * Math.sin(dLng/2);
    return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1-a));
}

const _escDiv = document.createElement('div');
function escapeHtml(s) {
    _escDiv.textContent = s == null ? '' : String(s);
    return _escDiv.innerHTML;
}

// ─── START ───
document.addEventListener('DOMContentLoaded', init);

// Expose for debugging
window.SkylensApp = {
    State,
    poll,
    selectDrone,
    fitAllMarkers
};

})();
