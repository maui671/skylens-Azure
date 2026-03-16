package tak

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/K13094/skylens/internal/processor"
)

// CoT Event XML structure per MIL-STD-6017 / TAK specification
type Event struct {
	XMLName xml.Name `xml:"event"`
	Version string   `xml:"version,attr"`
	UID     string   `xml:"uid,attr"`
	Type    string   `xml:"type,attr"`
	How     string   `xml:"how,attr"`
	Time    string   `xml:"time,attr"`
	Start   string   `xml:"start,attr"`
	Stale   string   `xml:"stale,attr"`
	Point   Point    `xml:"point"`
	Detail  *Detail  `xml:"detail,omitempty"`
}

type Point struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
	Hae float64 `xml:"hae,attr"` // Height Above Ellipsoid (meters)
	Ce  float64 `xml:"ce,attr"`  // Circular Error (meters)
	Le  float64 `xml:"le,attr"`  // Linear Error (meters)
}

type Detail struct {
	Contact  *Contact  `xml:"contact,omitempty"`
	Track    *Track    `xml:"track,omitempty"`
	Remarks  *Remarks  `xml:"remarks,omitempty"`
	Usericon *Usericon `xml:"usericon,omitempty"`
}

type Contact struct {
	Callsign string `xml:"callsign,attr"`
}

type Track struct {
	Speed  float64 `xml:"speed,attr"`  // m/s
	Course float64 `xml:"course,attr"` // degrees true
}

type Remarks struct {
	Text string `xml:",chardata"`
}

type Usericon struct {
	Iconsetpath string `xml:"iconsetpath,attr"`
}

// CotType maps Skylens classification to TAK CoT type atom.
// Format: a-{affiliation}-A-M-H-Q (atom - affil - Air - Military - Helicopter - UAV)
func CotType(classification string) string {
	switch strings.ToUpper(classification) {
	case "HOSTILE":
		return "a-h-A-M-H-Q"
	case "SUSPECT":
		return "a-s-A-M-H-Q"
	case "NEUTRAL":
		return "a-n-A-M-H-Q"
	case "FRIENDLY":
		return "a-f-A-M-H-Q"
	default: // UNKNOWN, UNVERIFIED, etc.
		return "a-u-A-M-H-Q"
	}
}

// CotTypeOperator returns the CoT type for an operator position marker.
func CotTypeOperator() string {
	return "a-f-G" // friendly - ground
}

// BuildCallsign creates a TAK-friendly callsign from a drone.
func BuildCallsign(d *processor.Drone) string {
	// Use track number + designation if available
	if d.TrackNumber > 0 {
		if d.Designation != "" {
			return fmt.Sprintf("T%03d %s", d.TrackNumber, d.Designation)
		}
		if d.Manufacturer != "" && d.Model != "" {
			return fmt.Sprintf("T%03d %s %s", d.TrackNumber, d.Manufacturer, d.Model)
		}
		return fmt.Sprintf("T%03d UAV", d.TrackNumber)
	}
	// Fallback: use identifier prefix
	id := d.Identifier
	if len(id) > 12 {
		id = id[:12]
	}
	return "UAV-" + id
}

// FormatCotTime formats a time as CoT XML time string (ISO 8601 Zulu).
func FormatCotTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.00Z")
}

// BuildDroneEvent creates a CoT event for a drone detection.
func BuildDroneEvent(d *processor.Drone, staleSec int) Event {
	now := time.Now().UTC()
	stale := now.Add(time.Duration(staleSec) * time.Second)

	callsign := BuildCallsign(d)

	// Build remarks with extra intel
	var remarks []string
	if d.SerialNumber != "" {
		remarks = append(remarks, "SN:"+d.SerialNumber)
	}
	if d.Manufacturer != "" {
		remarks = append(remarks, "Mfg:"+d.Manufacturer)
	}
	if d.Model != "" {
		remarks = append(remarks, "Model:"+d.Model)
	}
	remarks = append(remarks, fmt.Sprintf("Class:%s TS:%d", d.Classification, d.TrustScore))
	if d.RSSI != 0 {
		remarks = append(remarks, fmt.Sprintf("RSSI:%ddBm", d.RSSI))
	}
	remarkText := "SKYLENS: " + strings.Join(remarks, " | ")

	ev := Event{
		Version: "2.0",
		UID:     "skylens-uav-" + d.Identifier,
		Type:    CotType(d.Classification),
		How:     "m-g", // machine-generated
		Time:    FormatCotTime(now),
		Start:   FormatCotTime(now),
		Stale:   FormatCotTime(stale),
		Point: Point{
			Lat: d.Latitude,
			Lon: d.Longitude,
			Hae: float64(d.AltitudeGeodetic),
			Ce:  50.0, // 50m horizontal uncertainty (RSSI-based)
			Le:  50.0,
		},
		Detail: &Detail{
			Contact: &Contact{Callsign: callsign},
			Track: &Track{
				Speed:  float64(d.Speed),
				Course: float64(d.Heading),
			},
			Remarks: &Remarks{Text: remarkText},
		},
	}

	return ev
}

// BuildOperatorEvent creates a CoT event for a drone operator position.
func BuildOperatorEvent(d *processor.Drone, staleSec int) Event {
	now := time.Now().UTC()
	stale := now.Add(time.Duration(staleSec) * time.Second)

	callsign := BuildCallsign(d) + " OPR"

	return Event{
		Version: "2.0",
		UID:     "skylens-opr-" + d.Identifier,
		Type:    CotTypeOperator(),
		How:     "m-g",
		Time:    FormatCotTime(now),
		Start:   FormatCotTime(now),
		Stale:   FormatCotTime(stale),
		Point: Point{
			Lat: d.OperatorLatitude,
			Lon: d.OperatorLongitude,
			Hae: float64(d.OperatorAltitude),
			Ce:  100.0,
			Le:  100.0,
		},
		Detail: &Detail{
			Contact: &Contact{Callsign: callsign},
			Remarks: &Remarks{Text: "SKYLENS: Operator position for " + d.Identifier},
		},
	}
}

// BuildDropOperatorEvent creates a drop event for an operator marker.
// UID must match the operator event's UID format: skylens-opr-{id}
func BuildDropOperatorEvent(droneID string) Event {
	now := time.Now().UTC()
	stale := now.Add(-1 * time.Second)

	return Event{
		Version: "2.0",
		UID:     "skylens-opr-" + droneID,
		Type:    CotTypeOperator(),
		How:     "m-g",
		Time:    FormatCotTime(now),
		Start:   FormatCotTime(now),
		Stale:   FormatCotTime(stale),
		Point: Point{
			Lat: 0, Lon: 0, Hae: 0, Ce: 999999, Le: 999999,
		},
	}
}

// BuildDropEvent creates a CoT "delete" event (stale < start signals removal in TAK).
func BuildDropEvent(droneID string) Event {
	now := time.Now().UTC()
	// Stale before start = TAK removes the marker
	stale := now.Add(-1 * time.Second)

	return Event{
		Version: "2.0",
		UID:     "skylens-uav-" + droneID,
		Type:    "a-u-A-M-H-Q",
		How:     "m-g",
		Time:    FormatCotTime(now),
		Start:   FormatCotTime(now),
		Stale:   FormatCotTime(stale),
		Point: Point{
			Lat: 0, Lon: 0, Hae: 0, Ce: 999999, Le: 999999,
		},
	}
}

// MarshalEvent serializes a CoT event to XML bytes.
func MarshalEvent(ev Event) ([]byte, error) {
	return xml.Marshal(ev)
}
