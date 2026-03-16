/* ═══════════════════════════════════════════════════════════════
   SKYLENS — MGRS Coordinate Conversion (Pure JS, no dependencies)
   NATO Military Grid Reference System alongside lat/lon display.
   Algorithm: lat/lon → UTM → MGRS
   ═══════════════════════════════════════════════════════════════ */

var MGRS = (function () {
    'use strict';

    var DEG2RAD = Math.PI / 180;
    var RAD2DEG = 180 / Math.PI;

    // WGS84 ellipsoid
    var A = 6378137.0;           // semi-major axis
    var F = 1 / 298.257223563;   // flattening
    var E = Math.sqrt(2 * F - F * F); // eccentricity
    var E2 = E * E;
    var EP2 = E2 / (1 - E2);    // e'^2

    // MGRS band letters (C-X, omitting I and O)
    var BAND_LETTERS = 'CDEFGHJKLMNPQRSTUVWX';

    // 100km grid square column letters (A-H, J-N, P-Z — 8 per set, cycling every 3 zones)
    var COL_LETTERS = 'ABCDEFGHJKLMNPQRSTUVWXYZ'; // 24 letters (no I, O)
    // 100km grid square row letters (A-V, cycling every 2M northing — 20 letters, no I, O)
    var ROW_LETTERS = 'ABCDEFGHJKLMNPQRSTUV'; // 20 letters

    /**
     * Convert lat/lon to UTM
     * Returns { zone, band, easting, northing, hemisphere }
     */
    function latLonToUTM(lat, lon) {
        // Clamp longitude
        if (lon > 180) lon -= 360;
        if (lon < -180) lon += 360;

        // UTM zone
        var zone = Math.floor((lon + 180) / 6) + 1;

        // Norway/Svalbard exceptions
        if (lat >= 56 && lat < 64 && lon >= 3 && lon < 12) zone = 32;
        if (lat >= 72 && lat < 84) {
            if (lon >= 0  && lon <  9) zone = 31;
            else if (lon >= 9  && lon < 21) zone = 33;
            else if (lon >= 21 && lon < 33) zone = 35;
            else if (lon >= 33 && lon < 42) zone = 37;
        }

        var lonOrigin = (zone - 1) * 6 - 180 + 3; // central meridian
        var latRad = lat * DEG2RAD;
        var lonRad = lon * DEG2RAD;
        var lonOrigRad = lonOrigin * DEG2RAD;

        var N = A / Math.sqrt(1 - E2 * Math.sin(latRad) * Math.sin(latRad));
        var T = Math.tan(latRad) * Math.tan(latRad);
        var C = EP2 * Math.cos(latRad) * Math.cos(latRad);
        var AA = Math.cos(latRad) * (lonRad - lonOrigRad);

        var M = A * (
            (1 - E2 / 4 - 3 * E2 * E2 / 64 - 5 * E2 * E2 * E2 / 256) * latRad -
            (3 * E2 / 8 + 3 * E2 * E2 / 32 + 45 * E2 * E2 * E2 / 1024) * Math.sin(2 * latRad) +
            (15 * E2 * E2 / 256 + 45 * E2 * E2 * E2 / 1024) * Math.sin(4 * latRad) -
            (35 * E2 * E2 * E2 / 3072) * Math.sin(6 * latRad)
        );

        var easting = 0.9996 * N * (
            AA +
            (1 - T + C) * AA * AA * AA / 6 +
            (5 - 18 * T + T * T + 72 * C - 58 * EP2) * AA * AA * AA * AA * AA / 120
        ) + 500000;

        var northing = 0.9996 * (
            M + N * Math.tan(latRad) * (
                AA * AA / 2 +
                (5 - T + 9 * C + 4 * C * C) * AA * AA * AA * AA / 24 +
                (61 - 58 * T + T * T + 600 * C - 330 * EP2) * AA * AA * AA * AA * AA * AA / 720
            )
        );

        if (lat < 0) northing += 10000000;

        // Band letter
        var bandIdx;
        if (lat >= -80 && lat < -72) bandIdx = 0;
        else if (lat >= 72 && lat <= 84) bandIdx = 19;
        else bandIdx = Math.floor((lat + 80) / 8);
        if (bandIdx < 0) bandIdx = 0;
        if (bandIdx > 19) bandIdx = 19;
        var band = BAND_LETTERS[bandIdx];

        return {
            zone: zone,
            band: band,
            easting: easting,
            northing: northing,
            hemisphere: lat >= 0 ? 'N' : 'S'
        };
    }

    /**
     * Convert UTM to MGRS grid reference
     * precision: 1=10km, 2=1km, 3=100m, 4=10m, 5=1m
     */
    function utmToMGRS(zone, band, easting, northing, precision) {
        // 100km column letter
        // Column letters repeat every 3 zones: sets 1-3, 4-6, etc.
        var setNumber = ((zone - 1) % 6);
        var colIdx = (setNumber * 8 + Math.floor(easting / 100000) - 1) % 24;
        if (colIdx < 0) colIdx += 24;
        var col = COL_LETTERS[colIdx];

        // 100km row letter
        // Row letters cycle through 20 letters; odd zones start at A, even at F
        var northing100k = Math.floor(northing % 2000000 / 100000);
        var rowOffset = (zone % 2 === 0) ? 5 : 0;
        var rowIdx = (northing100k + rowOffset) % 20;
        var row = ROW_LETTERS[rowIdx];

        // Numeric portion within 100km square
        var e100k = Math.floor(easting % 100000);
        var n100k = Math.floor(northing % 100000);

        // Truncate to precision
        var divisor = Math.pow(10, 5 - precision);
        var eDigits = Math.floor(e100k / divisor);
        var nDigits = Math.floor(n100k / divisor);

        var eStr = String(eDigits);
        var nStr = String(nDigits);
        while (eStr.length < precision) eStr = '0' + eStr;
        while (nStr.length < precision) nStr = '0' + nStr;

        return {
            zone: zone,
            band: band,
            col: col,
            row: row,
            easting: eStr,
            northing: nStr
        };
    }

    /**
     * MGRS.forward(lat, lon, precision) → spaced string like "18TXR 83525 36414"
     * Returns '--' for invalid/sentinel coordinates.
     */
    function forward(lat, lon, precision) {
        // Guard: null/undefined
        if (lat == null || lon == null) return '--';
        // Coerce to number
        lat = +lat;
        lon = +lon;
        // Guard: NaN
        if (isNaN(lat) || isNaN(lon)) return '--';
        // Guard: 0,0 sentinel (Skylens uses 0,0 as "no position available")
        if (lat === 0 && lon === 0) return '--';
        // Guard: MGRS latitude range (-80 to 84)
        if (lat < -80 || lat > 84) return '--';

        if (precision == null) precision = 5;
        if (precision < 1) precision = 1;
        if (precision > 5) precision = 5;

        var utm = latLonToUTM(lat, lon);
        var mgrs = utmToMGRS(utm.zone, utm.band, utm.easting, utm.northing, precision);

        return mgrs.zone + mgrs.band + mgrs.col + mgrs.row + ' ' +
               mgrs.easting + ' ' + mgrs.northing;
    }

    /**
     * MGRS.format(lat, lon, precision, compact)
     * compact=true removes spaces: "18TXR8352536414"
     * compact=false or omitted: same as forward()
     */
    function format(lat, lon, precision, compact) {
        var result = forward(lat, lon, precision);
        if (result === '--') return '--';
        if (compact) return result.replace(/ /g, '');
        return result;
    }

    return {
        forward: forward,
        format: format
    };
})();
