package geo

import (
	"fmt"
	"math"
	"strings"
)

const (
	deg2rad = math.Pi / 180
	a       = 6378137.0           // WGS84 semi-major axis
	f       = 1 / 298.257223563   // flattening
)

var (
	e   = math.Sqrt(2*f - f*f) // eccentricity
	e2  = e * e
	ep2 = e2 / (1 - e2)

	bandLetters = "CDEFGHJKLMNPQRSTUVWX"
	colLetters  = "ABCDEFGHJKLMNPQRSTUVWXYZ" // 24 letters (no I, O)
	rowLetters  = "ABCDEFGHJKLMNPQRSTUV"     // 20 letters
)

// LatLonToMGRS converts latitude/longitude to an MGRS grid reference string.
// Precision 1=10km, 2=1km, 3=100m, 4=10m, 5=1m.
// Returns "" for sentinel (0,0) or out-of-range coordinates.
func LatLonToMGRS(lat, lon float64, precision int) string {
	if lat == 0 && lon == 0 {
		return ""
	}
	if lat < -80 || lat > 84 {
		return ""
	}
	if precision < 1 {
		precision = 1
	}
	if precision > 5 {
		precision = 5
	}

	zone, band, easting, northing := latLonToUTM(lat, lon)
	col, row, eStr, nStr := utmToMGRS(zone, easting, northing, precision)

	return fmt.Sprintf("%d%c%c%c %s %s", zone, band, col, row, eStr, nStr)
}

// LatLonToMGRSCompact returns MGRS without spaces (e.g. "18TXR8352536414").
func LatLonToMGRSCompact(lat, lon float64, precision int) string {
	s := LatLonToMGRS(lat, lon, precision)
	return strings.ReplaceAll(s, " ", "")
}

func latLonToUTM(lat, lon float64) (zone int, band byte, easting, northing float64) {
	if lon > 180 {
		lon -= 360
	}
	if lon < -180 {
		lon += 360
	}

	zone = int(math.Floor((lon+180)/6)) + 1

	// Norway/Svalbard exceptions
	if lat >= 56 && lat < 64 && lon >= 3 && lon < 12 {
		zone = 32
	}
	if lat >= 72 && lat < 84 {
		if lon >= 0 && lon < 9 {
			zone = 31
		} else if lon >= 9 && lon < 21 {
			zone = 33
		} else if lon >= 21 && lon < 33 {
			zone = 35
		} else if lon >= 33 && lon < 42 {
			zone = 37
		}
	}

	lonOrigin := float64((zone-1)*6-180+3) * deg2rad
	latRad := lat * deg2rad
	lonRad := lon * deg2rad

	sinLat := math.Sin(latRad)
	cosLat := math.Cos(latRad)
	tanLat := math.Tan(latRad)

	n := a / math.Sqrt(1-e2*sinLat*sinLat)
	t := tanLat * tanLat
	c := ep2 * cosLat * cosLat
	aa := cosLat * (lonRad - lonOrigin)

	m := a * ((1-e2/4-3*e2*e2/64-5*e2*e2*e2/256)*latRad -
		(3*e2/8+3*e2*e2/32+45*e2*e2*e2/1024)*math.Sin(2*latRad) +
		(15*e2*e2/256+45*e2*e2*e2/1024)*math.Sin(4*latRad) -
		(35*e2*e2*e2/3072)*math.Sin(6*latRad))

	easting = 0.9996 * n * (aa +
		(1-t+c)*aa*aa*aa/6 +
		(5-18*t+t*t+72*c-58*ep2)*aa*aa*aa*aa*aa/120) + 500000

	northing = 0.9996 * (m + n*tanLat*(aa*aa/2+
		(5-t+9*c+4*c*c)*aa*aa*aa*aa/24+
		(61-58*t+t*t+600*c-330*ep2)*aa*aa*aa*aa*aa*aa/720))

	if lat < 0 {
		northing += 10000000
	}

	// Band letter
	var bandIdx int
	if lat >= -80 && lat < -72 {
		bandIdx = 0
	} else if lat >= 72 && lat <= 84 {
		bandIdx = 19
	} else {
		bandIdx = int(math.Floor((lat + 80) / 8))
	}
	if bandIdx < 0 {
		bandIdx = 0
	}
	if bandIdx > 19 {
		bandIdx = 19
	}
	band = bandLetters[bandIdx]

	return zone, band, easting, northing
}

func utmToMGRS(zone int, easting, northing float64, precision int) (col, row byte, eStr, nStr string) {
	setNumber := (zone - 1) % 6
	colIdx := (setNumber*8 + int(math.Floor(easting/100000)) - 1) % 24
	if colIdx < 0 {
		colIdx += 24
	}
	col = colLetters[colIdx]

	northing100k := int(math.Floor(math.Mod(northing, 2000000) / 100000))
	rowOffset := 0
	if zone%2 == 0 {
		rowOffset = 5
	}
	rowIdx := (northing100k + rowOffset) % 20
	row = rowLetters[rowIdx]

	e100k := int(math.Floor(math.Mod(easting, 100000)))
	n100k := int(math.Floor(math.Mod(northing, 100000)))

	divisor := int(math.Pow(10, float64(5-precision)))
	eDigits := e100k / divisor
	nDigits := n100k / divisor

	fmtStr := fmt.Sprintf("%%0%dd", precision)
	eStr = fmt.Sprintf(fmtStr, eDigits)
	nStr = fmt.Sprintf(fmtStr, nDigits)

	return col, row, eStr, nStr
}
