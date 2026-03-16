/* ═══════════════════════════════════════════════════════════════════════════
   SKYLENS ACTIVITY TIMELINE HEATMAP v1.0
   7-day x 24-hour grid showing detection density
   Color gradient from quiet (dark blue) to active (red)
   Hover for exact count, click to filter map to time period
   ═══════════════════════════════════════════════════════════════════════════ */

var ActivityHeatmap = (function() {
    'use strict';

    // ─── CONFIGURATION ───
    var CONFIG = {
        days: 7,
        hours: 24,
        cellWidth: 20,
        cellHeight: 16,
        cellGap: 2,
        labelWidth: 50,
        topLabelHeight: 20,
        // Color gradient: quiet -> busy -> very active
        colors: [
            { threshold: 0, color: '#0A0E0D' },      // Empty
            { threshold: 1, color: '#1A2329' },      // Very low
            { threshold: 3, color: '#0D47A1' },      // Low (dark blue)
            { threshold: 6, color: '#1976D2' },      // Moderate-low
            { threshold: 10, color: '#0288D1' },     // Moderate
            { threshold: 15, color: '#00ACC1' },     // Moderate-high
            { threshold: 25, color: '#FFB300' },     // High (yellow)
            { threshold: 40, color: '#FF8F00' },     // Very high
            { threshold: 60, color: '#FF6D00' },     // Intense
            { threshold: 100, color: '#FF1744' }     // Critical (red)
        ],
        dayLabels: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
    };

    // ─── STATE ───
    var state = {
        mounted: false,
        container: null,
        data: null,          // 7x24 matrix of counts
        maxValue: 0,
        selectedCell: null,  // { day, hour }
        onCellClick: null    // Callback when cell clicked
    };

    // ─── INITIALIZE DATA MATRIX ───
    function initDataMatrix() {
        state.data = [];
        for (var d = 0; d < CONFIG.days; d++) {
            state.data[d] = [];
            for (var h = 0; h < CONFIG.hours; h++) {
                state.data[d][h] = 0;
            }
        }
        state.maxValue = 0;
    }

    // ─── ADD DETECTION TO HEATMAP ───
    function addDetection(timestamp) {
        if (!state.data) initDataMatrix();

        var date = timestamp instanceof Date ? timestamp : new Date(timestamp);
        if (isNaN(date.getTime())) return;

        var now = new Date();
        var daysDiff = Math.floor((now - date) / (1000 * 60 * 60 * 24));

        if (daysDiff < 0 || daysDiff >= CONFIG.days) return;

        var dayOfWeek = date.getDay();
        var hour = date.getHours();

        state.data[daysDiff][hour]++;
        if (state.data[daysDiff][hour] > state.maxValue) {
            state.maxValue = state.data[daysDiff][hour];
        }
    }

    // ─── LOAD FROM DETECTIONS ARRAY ───
    function loadDetections(detections) {
        initDataMatrix();

        (detections || []).forEach(function(d) {
            var ts = d.timestamp || d.last_seen || d.created_at;
            if (ts) addDetection(ts);
        });

        if (state.mounted) render();
    }

    // ─── LOAD FROM API ───
    function loadFromAPI(apiUrl) {
        apiUrl = apiUrl || '/api/detections/history';

        fetch(apiUrl)
            .then(function(r) { return r.json(); })
            .then(function(data) {
                if (data && Array.isArray(data.detections)) {
                    loadDetections(data.detections);
                } else if (Array.isArray(data)) {
                    loadDetections(data);
                }
            })
            .catch(function(err) {
                console.warn('ActivityHeatmap: Failed to load data:', err);
            });
    }

    // ─── GET COLOR FOR VALUE ───
    function getColor(value) {
        if (value === 0) return CONFIG.colors[0].color;

        for (var i = CONFIG.colors.length - 1; i >= 0; i--) {
            if (value >= CONFIG.colors[i].threshold) {
                return CONFIG.colors[i].color;
            }
        }
        return CONFIG.colors[0].color;
    }

    // ─── GET INTENSITY CLASS ───
    function getIntensityClass(value) {
        if (value === 0) return 'ah-empty';
        if (value < 5) return 'ah-low';
        if (value < 15) return 'ah-medium';
        if (value < 30) return 'ah-high';
        return 'ah-critical';
    }

    // ─── GET DAY LABEL ───
    function getDayLabel(daysAgo) {
        if (daysAgo === 0) return 'Today';
        if (daysAgo === 1) return 'Yester.';

        var date = new Date();
        date.setDate(date.getDate() - daysAgo);
        return CONFIG.dayLabels[date.getDay()];
    }

    // ─── RENDER ───
    function render() {
        if (!state.container || !state.data) return;

        var width = CONFIG.labelWidth + (CONFIG.cellWidth + CONFIG.cellGap) * CONFIG.hours + 10;
        var height = CONFIG.topLabelHeight + (CONFIG.cellHeight + CONFIG.cellGap) * CONFIG.days + 40;

        var html = '<div class="activity-heatmap">';

        // Header
        html += '<div class="ah-header">';
        html += '<span class="ah-title">DETECTION ACTIVITY</span>';
        html += '<span class="ah-subtitle">Last 7 days</span>';
        html += '</div>';

        // SVG container
        html += '<svg class="ah-svg" viewBox="0 0 ' + width + ' ' + height + '" width="100%" preserveAspectRatio="xMinYMin meet">';

        // Hour labels (top)
        for (var h = 0; h < CONFIG.hours; h++) {
            if (h % 3 === 0) {  // Show every 3 hours
                var x = CONFIG.labelWidth + h * (CONFIG.cellWidth + CONFIG.cellGap) + CONFIG.cellWidth / 2;
                html += '<text x="' + x + '" y="15" class="ah-hour-label">' +
                    (h === 0 ? '12a' : h === 12 ? '12p' : h < 12 ? h + 'a' : (h - 12) + 'p') +
                '</text>';
            }
        }

        // Day rows
        for (var d = 0; d < CONFIG.days; d++) {
            var rowY = CONFIG.topLabelHeight + d * (CONFIG.cellHeight + CONFIG.cellGap);

            // Day label
            html += '<text x="' + (CONFIG.labelWidth - 8) + '" y="' + (rowY + CONFIG.cellHeight - 3) + '" class="ah-day-label">' +
                getDayLabel(d) +
            '</text>';

            // Hour cells
            for (var h2 = 0; h2 < CONFIG.hours; h2++) {
                var cellX = CONFIG.labelWidth + h2 * (CONFIG.cellWidth + CONFIG.cellGap);
                var value = state.data[d][h2];
                var color = getColor(value);
                var intensityClass = getIntensityClass(value);

                html += '<rect x="' + cellX + '" y="' + rowY + '" ' +
                    'width="' + CONFIG.cellWidth + '" height="' + CONFIG.cellHeight + '" ' +
                    'fill="' + color + '" rx="2" ' +
                    'class="ah-cell ' + intensityClass + '" ' +
                    'data-day="' + d + '" data-hour="' + h2 + '" data-value="' + value + '"/>';
            }
        }

        html += '</svg>';

        // Legend
        html += '<div class="ah-legend">';
        html += '<span class="ah-legend-label">Less</span>';
        CONFIG.colors.slice(1).forEach(function(c) {
            html += '<span class="ah-legend-box" style="background:' + c.color + '" title="' + c.threshold + '+ detections"></span>';
        });
        html += '<span class="ah-legend-label">More</span>';
        html += '</div>';

        // Tooltip (hidden by default)
        html += '<div class="ah-tooltip" id="ah-tooltip"></div>';

        html += '</div>';

        state.container.innerHTML = html;

        // Bind events
        bindEvents();
    }

    // ─── BIND EVENTS ───
    function bindEvents() {
        if (!state.container) return;

        var cells = state.container.querySelectorAll('.ah-cell');
        var tooltip = state.container.querySelector('#ah-tooltip');

        cells.forEach(function(cell) {
            // Hover
            cell.addEventListener('mouseenter', function(e) {
                var day = parseInt(this.getAttribute('data-day'));
                var hour = parseInt(this.getAttribute('data-hour'));
                var value = parseInt(this.getAttribute('data-value'));

                showTooltip(e, day, hour, value, tooltip);
            });

            cell.addEventListener('mouseleave', function() {
                hideTooltip(tooltip);
            });

            // Click
            cell.addEventListener('click', function() {
                var day = parseInt(this.getAttribute('data-day'));
                var hour = parseInt(this.getAttribute('data-hour'));

                handleCellClick(day, hour, this);
            });
        });
    }

    // ─── SHOW TOOLTIP ───
    function showTooltip(event, day, hour, value, tooltip) {
        if (!tooltip) return;

        var date = new Date();
        date.setDate(date.getDate() - day);
        date.setHours(hour, 0, 0, 0);

        var dateStr = date.toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' });
        var timeStr = date.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', hour12: true });
        var endHour = new Date(date);
        endHour.setHours(hour + 1);
        var endTimeStr = endHour.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', hour12: true });

        tooltip.innerHTML = '<div class="ah-tip-header">' + dateStr + '</div>' +
            '<div class="ah-tip-time">' + timeStr + ' - ' + endTimeStr + '</div>' +
            '<div class="ah-tip-count">' + value + ' detection' + (value !== 1 ? 's' : '') + '</div>';

        tooltip.style.display = 'block';

        // Position near cursor
        var rect = state.container.getBoundingClientRect();
        var x = event.clientX - rect.left + 10;
        var y = event.clientY - rect.top - 40;

        // Keep in bounds
        if (x + 120 > rect.width) x = rect.width - 120;
        if (y < 0) y = event.clientY - rect.top + 15;

        tooltip.style.left = x + 'px';
        tooltip.style.top = y + 'px';
    }

    // ─── HIDE TOOLTIP ───
    function hideTooltip(tooltip) {
        if (tooltip) {
            tooltip.style.display = 'none';
        }
    }

    // ─── HANDLE CELL CLICK ───
    function handleCellClick(day, hour, cellElement) {
        // Update selection visual
        var prevSelected = state.container.querySelector('.ah-cell.selected');
        if (prevSelected) {
            prevSelected.classList.remove('selected');
        }
        cellElement.classList.add('selected');

        state.selectedCell = { day: day, hour: hour };

        // Calculate time range
        var startDate = new Date();
        startDate.setDate(startDate.getDate() - day);
        startDate.setHours(hour, 0, 0, 0);

        var endDate = new Date(startDate);
        endDate.setHours(hour + 1, 0, 0, 0);

        // Trigger callback
        if (state.onCellClick) {
            state.onCellClick({
                day: day,
                hour: hour,
                startTime: startDate,
                endTime: endDate,
                count: state.data[day][hour]
            });
        }

        // Emit custom event
        var event = new CustomEvent('heatmap-cell-click', {
            detail: {
                day: day,
                hour: hour,
                startTime: startDate.toISOString(),
                endTime: endDate.toISOString()
            }
        });
        state.container.dispatchEvent(event);
    }

    // ─── CLEAR SELECTION ───
    function clearSelection() {
        if (!state.container) return;

        var selected = state.container.querySelector('.ah-cell.selected');
        if (selected) {
            selected.classList.remove('selected');
        }
        state.selectedCell = null;
    }

    // ─── MOUNT ───
    function mount(containerId, options) {
        var container = document.getElementById(containerId);
        if (!container) {
            console.error('ActivityHeatmap: Container not found:', containerId);
            return false;
        }

        state.container = container;
        state.mounted = true;

        if (options) {
            if (options.onCellClick) state.onCellClick = options.onCellClick;
            if (options.days) CONFIG.days = options.days;
        }

        if (!state.data) initDataMatrix();
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
        state.selectedCell = null;
    }

    // ─── GET STATS ───
    function getStats() {
        if (!state.data) return null;

        var total = 0, max = 0, maxDay = 0, maxHour = 0;
        var busyHours = {};

        for (var d = 0; d < CONFIG.days; d++) {
            for (var h = 0; h < CONFIG.hours; h++) {
                var v = state.data[d][h];
                total += v;
                if (v > max) {
                    max = v;
                    maxDay = d;
                    maxHour = h;
                }
                busyHours[h] = (busyHours[h] || 0) + v;
            }
        }

        // Find busiest hour overall
        var busiestHour = 0, busiestHourCount = 0;
        for (var hh in busyHours) {
            if (busyHours[hh] > busiestHourCount) {
                busiestHourCount = busyHours[hh];
                busiestHour = parseInt(hh);
            }
        }

        return {
            totalDetections: total,
            maxInSingleHour: max,
            maxDay: maxDay,
            maxHour: maxHour,
            busiestHour: busiestHour,
            averagePerHour: Math.round(total / (CONFIG.days * CONFIG.hours) * 10) / 10
        };
    }

    // ─── PUBLIC API ───
    return {
        mount: mount,
        unmount: unmount,
        addDetection: addDetection,
        loadDetections: loadDetections,
        loadFromAPI: loadFromAPI,
        clearSelection: clearSelection,
        getStats: getStats,
        render: render,
        CONFIG: CONFIG
    };

})();

// Export for module systems
if (typeof module !== 'undefined' && module.exports) {
    module.exports = ActivityHeatmap;
}
