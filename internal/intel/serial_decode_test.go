package intel

import "testing"

func TestDecodeModelFromSerial_NzymeIntel(t *testing.T) {
    tests := []struct {
        serial   string
        expected string
    }{
        // Air family — verified Feb 2026
        {"1581F6N8C237H0031Z2M", "Air 2S"},          // Field confirmed: F6N8 = Air 2S (Feb 2026)
        {"1581F6N8A237DML37RTD", "Air 2S"},          // F6N8 prefix = Air 2S
        {"1581F6N8A241BML33MF2", "Air 2S"},          // F6N8 prefix = Air 2S
        {"1581F6QAD244N00C15LS", "Air 3S"},          // Nzyme confirmed
        {"1581F67QE237Q00A003W", "Mavic 3 Classic"}, // 67 prefix = Mavic 3 Classic (verified)
        {"1581F67QE238700A00KR", "Mavic 3 Classic"}, // 67 prefix = Mavic 3 Classic (verified)

        // Mavic 2 family — verified Feb 2026
        {"1581F163CH85R0A30JG0", "Mavic 2 Pro"},     // 163 = Mavic 2 Pro (community confirmed)

        // Mavic 3 family
        {"1581F5BKD224T00B4T8T", "Mavic 3 Cine"},   // Nzyme confirmed
        {"1581F5BKB241A00F01NE", "Mavic 3 Cine"},   // Nzyme confirmed
        {"1581F5FH7245N002E0HU", "Mavic 3 Enterprise"}, // Propeller Aero docs: 5FH = M3E

        // FPV/Avata family
        {"1581F4QWB234200300WQ", "Avata"},           // FAA DOC confirmed
        {"1581F4QZB21C61BE04MN", "Avata"},           // Nzyme confirmed
        {"1581F45T7228200SV14L", "Mavic 3"},         // FAA DOC: 45T = Mavic 3 (not FPV!)

        // Phantom family
        {"1581F895C2563007E969", "Phantom 4 Pro"},   // Field observed
        {"1581F8LQC2532002028U", "Matrice 30"},      // Field observed

        // Air 2S — verified Feb 2026
        {"1581F3YTDJ1V00385UH0", "Air 2S"},         // DJI FAQ: 3YT = Air 2S (not Agras!)

        // Inspire family
        {"1581F9DEC2594029W68Z", "Inspire 3"},       // Field observed

        // Matrice family
        {"1581F7FVC251A00CB04F", "Matrice 4T"},      // Nzyme confirmed

        // New verified entries — Feb 2026
        {"1581F0M6ABCD12345678", "Mavic 2 Zoom"},    // Community: 0M6 = Mavic 2 Zoom
        {"1581F11VABCD12345678", "Phantom 4 Pro V2"},// Community: 11V = Phantom 4 Pro V2
        {"1581F1SCABCD12345678", "Mavic Mini"},       // Community: 1SC = Mavic Mini
        {"1581F3NZABCD12345678", "Mini 2"},           // Community: 3NZ = Mini 2
        {"1581F6MKABCD12345678", "Mavic 3 Pro"},      // Community: 6MK = Mavic 3 Pro
        {"1581F1WNABCD12345678", "Mavic Air 2"},      // Community: 1WN = Mavic Air 2
    }

    for _, tc := range tests {
        t.Run(tc.serial[:10], func(t *testing.T) {
            result := DecodeModelFromSerial(tc.serial)
            if result != tc.expected {
                t.Errorf("DecodeModelFromSerial(%q) = %q, want %q", tc.serial, result, tc.expected)
            }
        })
    }
}
