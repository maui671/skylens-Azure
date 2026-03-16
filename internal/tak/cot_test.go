package tak

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/K13094/skylens/internal/processor"
)

func TestCotType(t *testing.T) {
	tests := []struct {
		classification string
		want           string
	}{
		{"HOSTILE", "a-h-A-M-H-Q"},
		{"SUSPECT", "a-s-A-M-H-Q"},
		{"NEUTRAL", "a-n-A-M-H-Q"},
		{"FRIENDLY", "a-f-A-M-H-Q"},
		{"UNKNOWN", "a-u-A-M-H-Q"},
		{"UNVERIFIED", "a-u-A-M-H-Q"},
		{"", "a-u-A-M-H-Q"},
	}
	for _, tt := range tests {
		got := CotType(tt.classification)
		if got != tt.want {
			t.Errorf("CotType(%q) = %q, want %q", tt.classification, got, tt.want)
		}
	}
}

func TestBuildCallsign(t *testing.T) {
	d := &processor.Drone{
		TrackNumber:  42,
		Designation:  "Mavic 3",
		Manufacturer: "DJI",
		Model:        "Mavic 3",
	}
	cs := BuildCallsign(d)
	if cs != "T042 Mavic 3" {
		t.Errorf("BuildCallsign() = %q, want %q", cs, "T042 Mavic 3")
	}

	// No designation
	d2 := &processor.Drone{
		TrackNumber:  7,
		Manufacturer: "DJI",
		Model:        "Air 2S",
	}
	cs2 := BuildCallsign(d2)
	if cs2 != "T007 DJI Air 2S" {
		t.Errorf("BuildCallsign() = %q, want %q", cs2, "T007 DJI Air 2S")
	}

	// No track number
	d3 := &processor.Drone{Identifier: "ABCDEF123456789"}
	cs3 := BuildCallsign(d3)
	if cs3 != "UAV-ABCDEF123456" {
		t.Errorf("BuildCallsign() = %q, want %q", cs3, "UAV-ABCDEF123456")
	}
}

func TestFormatCotTime(t *testing.T) {
	ts := time.Date(2026, 3, 5, 14, 30, 45, 0, time.UTC)
	got := FormatCotTime(ts)
	want := "2026-03-05T14:30:45.00Z"
	if got != want {
		t.Errorf("FormatCotTime() = %q, want %q", got, want)
	}
}

func TestBuildDroneEvent(t *testing.T) {
	d := &processor.Drone{
		Identifier:       "TEST-001",
		TrackNumber:      1,
		Designation:      "Mavic 3",
		Manufacturer:     "DJI",
		Model:            "Mavic 3",
		SerialNumber:     "SN12345",
		Latitude:         18.253,
		Longitude:        -65.644,
		AltitudeGeodetic: 100,
		Speed:            5.5,
		Heading:          270,
		Classification:   "HOSTILE",
		TrustScore:       20,
		RSSI:             -70,
	}

	ev := BuildDroneEvent(d, 30)

	if ev.Version != "2.0" {
		t.Errorf("Version = %q, want 2.0", ev.Version)
	}
	if ev.UID != "skylens-uav-TEST-001" {
		t.Errorf("UID = %q", ev.UID)
	}
	if ev.Type != "a-h-A-M-H-Q" {
		t.Errorf("Type = %q, want hostile", ev.Type)
	}
	if ev.How != "m-g" {
		t.Errorf("How = %q", ev.How)
	}
	if ev.Point.Lat != 18.253 || ev.Point.Lon != -65.644 {
		t.Errorf("Point = %+v", ev.Point)
	}
	if ev.Detail == nil || ev.Detail.Contact == nil {
		t.Fatal("Missing detail/contact")
	}
	if ev.Detail.Contact.Callsign != "T001 Mavic 3" {
		t.Errorf("Callsign = %q", ev.Detail.Contact.Callsign)
	}
	if ev.Detail.Track.Speed != 5.5 {
		t.Errorf("Speed = %f", ev.Detail.Track.Speed)
	}

	// Marshal to XML
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	xmlStr := string(data)
	if !strings.Contains(xmlStr, "SKYLENS:") {
		t.Error("XML should contain SKYLENS remark")
	}
	if !strings.Contains(xmlStr, "a-h-A-M-H-Q") {
		t.Error("XML should contain hostile type")
	}

	// Verify it's valid XML
	var parsed Event
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Errorf("Invalid XML: %v", err)
	}
}

func TestBuildDropEvent(t *testing.T) {
	ev := BuildDropEvent("TEST-001")
	if ev.UID != "skylens-uav-TEST-001" {
		t.Errorf("UID = %q", ev.UID)
	}

	// Stale must be before start (TAK drop signal)
	staleTime, _ := time.Parse("2006-01-02T15:04:05.00Z", ev.Stale)
	startTime, _ := time.Parse("2006-01-02T15:04:05.00Z", ev.Start)
	if !staleTime.Before(startTime) {
		t.Error("Stale should be before Start for drop events")
	}
}

func TestBuildDropOperatorEvent(t *testing.T) {
	ev := BuildDropOperatorEvent("TEST-001")
	// UID must match operator event format (skylens-opr-), NOT drone format (skylens-uav-)
	if ev.UID != "skylens-opr-TEST-001" {
		t.Errorf("UID = %q, want skylens-opr-TEST-001 (must match operator event UID)", ev.UID)
	}
	if ev.Type != "a-f-G" {
		t.Errorf("Type = %q, want operator type a-f-G", ev.Type)
	}

	// Stale must be before start
	staleTime, _ := time.Parse("2006-01-02T15:04:05.00Z", ev.Stale)
	startTime, _ := time.Parse("2006-01-02T15:04:05.00Z", ev.Start)
	if !staleTime.Before(startTime) {
		t.Error("Stale should be before Start for drop events")
	}
}

func TestBuildOperatorEvent(t *testing.T) {
	d := &processor.Drone{
		Identifier:       "TEST-001",
		TrackNumber:      1,
		Designation:      "Mavic 3",
		OperatorLatitude: 18.25,
		OperatorLongitude: -65.64,
		OperatorAltitude: 5,
	}
	ev := BuildOperatorEvent(d, 30)
	if ev.UID != "skylens-opr-TEST-001" {
		t.Errorf("UID = %q", ev.UID)
	}
	if ev.Type != "a-f-G" {
		t.Errorf("Type = %q, want operator type", ev.Type)
	}
	if ev.Point.Lat != 18.25 {
		t.Errorf("Lat = %f", ev.Point.Lat)
	}
}
