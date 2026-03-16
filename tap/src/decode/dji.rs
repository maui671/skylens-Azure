use tracing::trace;

/// Decoded DJI DroneID data from proprietary vendor IE
#[derive(Debug, Clone, Default)]
pub struct DjiDroneIdData {
    pub serial_number: Option<String>,
    pub latitude: Option<f64>,
    pub longitude: Option<f64>,
    /// Barometric/pressure altitude in meters (DJI reports barometric, not geodetic)
    pub altitude_pressure: Option<f32>,
    /// Height above ground level (AGL) in meters
    pub height_agl: Option<f32>,
    pub speed_north: Option<f32>,
    pub speed_east: Option<f32>,
    pub speed_up: Option<f32>,
    pub heading: Option<f32>,
    pub pitch: Option<f32>,
    pub roll: Option<f32>,
    pub home_latitude: Option<f64>,
    pub home_longitude: Option<f64>,
    pub product_type: Option<u8>,
    pub uuid: Option<String>,
    /// State info flags (DJI proprietary, reverse-engineered — layout varies by source)
    /// dep13 interpretation: bit 0=motor, bits 1-2=in_air
    /// Kismet/kaitai: bit 0=serial_valid, bit 4=motor, bit 5=airborne
    pub state_info: u16,
    /// Sequence number for tracking frame order
    pub sequence_number: u16,
    /// Protocol version
    pub version: u8,
}

/// DJI DroneID coordinate conversion factor
/// raw_int32 / 174533.0 = degrees (NOT radians)
/// Per Kismet kaitai, anarkiwi decoder, and DroneSecurity (RUB-SysSec, NDSS 2023)
const DJI_COORD_FACTOR: f64 = 174533.0;

/// Decode DJI DroneID from vendor IE data.
/// The `data` parameter is the vendor IE payload AFTER OUI+type bytes.
/// Structure based on Kismet's dot11_ie_221_dji_droneid parser.
///
/// Two DJI vendor IE formats exist:
/// - OUI 26:37:12 / 26:6F:48: oui_type IS the subcommand (0x10/0x11), data = payload
/// - OUI 60:60:1F (OcuSync): oui_type = 0x13 (vendor type), data[0] = subcommand, data[1..] = payload
pub fn decode_dji_droneid(data: &[u8], oui_type: u8) -> Option<DjiDroneIdData> {
    trace!(len = data.len(), oui_type, "Decoding DJI DroneID");

    // For OUI 60:60:1F format: oui_type=0x13 is the DJI vendor type marker,
    // actual subcommand is in data[0], and payload starts at data[1]
    if oui_type == 0x13 {
        if data.is_empty() {
            return None;
        }
        let subcommand = data[0];
        let payload = &data[1..];
        trace!(subcommand, payload_len = payload.len(), "DJI OcuSync format: extracted subcommand from data");
        return match subcommand {
            0x10 => decode_flight_reg(payload),
            0x11 => decode_flight_purpose(payload),
            _ => decode_flight_reg(payload),
        };
    }

    // For OUI 26:37:12 / 26:6F:48 format: oui_type IS the subcommand directly
    match oui_type {
        0x10 => decode_flight_reg(data),
        0x11 => decode_flight_purpose(data),
        _ => {
            // Try flight reg as fallback
            decode_flight_reg(data)
        }
    }
}

/// Decode DJI DroneID flight registration/telemetry packet (subcommand 0x10)
/// Offsets from Kismet kaitai definition:
///   0: version (1)
///   1: seq (2, le)
///   3: state_info (2, le)
///   5: serial (16, ascii)
///  21: raw_lon (4, s32le) — divide by 174533.0 for degrees
///  25: raw_lat (4, s32le) — divide by 174533.0 for degrees
///  29: altitude (2, s16le) — meters
///  31: height (2, s16le) — meters AGL
///  33: v_north (2, s16le) — cm/s
///  35: v_east (2, s16le) — cm/s
///  37: v_up (2, s16le) — cm/s
///  39: raw_pitch (2, s16le)
///  41: raw_roll (2, s16le)
///  43: raw_yaw (2, s16le) — centidegrees, divide by 100.0 for degrees
///  45: raw_home_lon (4, s32le)
///  49: raw_home_lat (4, s32le)
///  53: product_type (1)
///  54: uuid_len (1)
///  55: uuid (20)
fn decode_flight_reg(data: &[u8]) -> Option<DjiDroneIdData> {
    if data.len() < 53 {
        trace!(len = data.len(), "DJI flight reg payload too short (need 53+)");
        return None;
    }

    let mut result = DjiDroneIdData::default();

    // Version: offset 0
    result.version = data[0];

    // Sequence number: offset 1, uint16 LE
    result.sequence_number = u16::from_le_bytes([data[1], data[2]]);

    // State info: offset 3, uint16 LE — bit 0 = motor on, bits 1-2 = in air
    result.state_info = u16::from_le_bytes([data[3], data[4]]);

    // Serial number: offset 5, 16 bytes, ASCII null-terminated
    let serial = extract_ascii(&data[5..21]);
    if !serial.is_empty() {
        result.serial_number = Some(serial);
    }

    // DJI DroneID V2 (OcuSync 3/O4, firmware mid-2023+) encrypts telemetry fields.
    // Serial number (offset 5-20) remains cleartext, but coordinates, velocity,
    // altitude, and heading are ciphertext. Parsing them produces garbage.
    // Only trust serial_number, product_type, state_info, and sequence_number for V2+.
    if result.version >= 2 {
        trace!(version = result.version, "DJI V2+ detected: telemetry fields may be encrypted, keeping serial only");

        // Product type: offset 53 (still cleartext in V2)
        if data.len() >= 54 {
            result.product_type = Some(data[53]);
        }

        // Return early — coordinate/velocity fields are encrypted gibberish
        return if result.serial_number.is_some() || result.product_type.is_some() {
            Some(result)
        } else {
            None
        };
    }

    // Longitude: offset 21, int32 LE — raw / 174533.0 = degrees
    let raw_lon = i32::from_le_bytes([data[21], data[22], data[23], data[24]]);
    if raw_lon != 0 {
        let lon_deg = raw_lon as f64 / DJI_COORD_FACTOR;
        if (-180.0..=180.0).contains(&lon_deg) {
            result.longitude = Some(lon_deg);
        }
    }

    // Latitude: offset 25, int32 LE — raw / 174533.0 = degrees
    let raw_lat = i32::from_le_bytes([data[25], data[26], data[27], data[28]]);
    if raw_lat != 0 {
        let lat_deg = raw_lat as f64 / DJI_COORD_FACTOR;
        if (-90.0..=90.0).contains(&lat_deg) {
            result.latitude = Some(lat_deg);
        }
    }

    // Altitude: offset 29, int16 LE, meters
    // NOTE: DJI reports barometric/pressure altitude, NOT geodetic (WGS84 ellipsoid)
    // This is consistent with how DJI drones internally measure altitude via barometer
    let alt = i16::from_le_bytes([data[29], data[30]]);
    if alt != 0 {
        result.altitude_pressure = Some(alt as f32);
    }

    // Height AGL: offset 31, int16 LE, meters above takeoff/ground
    let height = i16::from_le_bytes([data[31], data[32]]);
    if height != 0 {
        result.height_agl = Some(height as f32);
    }

    // Velocity north: offset 33, int16 LE, cm/s → m/s
    let v_north = i16::from_le_bytes([data[33], data[34]]);
    result.speed_north = Some(v_north as f32 / 100.0);

    // Velocity east: offset 35, int16 LE, cm/s → m/s
    let v_east = i16::from_le_bytes([data[35], data[36]]);
    result.speed_east = Some(v_east as f32 / 100.0);

    // Velocity up: offset 37, int16 LE, cm/s → m/s
    let v_up = i16::from_le_bytes([data[37], data[38]]);
    result.speed_up = Some(v_up as f32 / 100.0);

    // Pitch: offset 39, int16 LE, centidegrees → degrees
    if data.len() >= 41 {
        let raw_pitch = i16::from_le_bytes([data[39], data[40]]);
        result.pitch = Some(raw_pitch as f32 / 100.0);
    }

    // Roll: offset 41, int16 LE, centidegrees → degrees
    if data.len() >= 43 {
        let raw_roll = i16::from_le_bytes([data[41], data[42]]);
        result.roll = Some(raw_roll as f32 / 100.0);
    }

    // Yaw/heading: offset 43, int16 LE, centidegrees → degrees (raw / 100.0)
    if data.len() >= 45 {
        let raw_yaw = i16::from_le_bytes([data[43], data[44]]);
        let yaw_deg = raw_yaw as f64 / 100.0;
        // Normalize to [0, 360)
        let heading = ((yaw_deg % 360.0) + 360.0) % 360.0;
        if heading < 360.0 {
            result.heading = Some(heading as f32);
        }
    }

    // Home longitude: offset 45, int32 LE — raw / 174533.0 = degrees
    if data.len() >= 49 {
        let raw_home_lon = i32::from_le_bytes([data[45], data[46], data[47], data[48]]);
        if raw_home_lon != 0 {
            let lon_deg = raw_home_lon as f64 / DJI_COORD_FACTOR;
            if (-180.0..=180.0).contains(&lon_deg) {
                result.home_longitude = Some(lon_deg);
            }
        }
    }

    // Home latitude: offset 49, int32 LE — raw / 174533.0 = degrees
    if data.len() >= 53 {
        let raw_home_lat = i32::from_le_bytes([data[49], data[50], data[51], data[52]]);
        if raw_home_lat != 0 {
            let lat_deg = raw_home_lat as f64 / DJI_COORD_FACTOR;
            if (-90.0..=90.0).contains(&lat_deg) {
                result.home_latitude = Some(lat_deg);
            }
        }
    }

    // Product type: offset 53
    if data.len() >= 54 {
        result.product_type = Some(data[53]);
    }

    // UUID: offset 55, up to 20 bytes
    if data.len() >= 56 {
        let uuid_len = data[54] as usize;
        let uuid_end = (55 + uuid_len).min(data.len()).min(75);
        if uuid_end > 55 {
            let uuid = extract_ascii(&data[55..uuid_end]);
            if !uuid.is_empty() {
                result.uuid = Some(uuid);
            }
        }
    }

    // Return if we got ANY meaningful data — even product_type alone is useful
    // for detecting drones without location (encrypted DroneID, partial data)
    if result.serial_number.is_some()
        || result.latitude.is_some()
        || result.altitude_pressure.is_some()
        || result.product_type.is_some()
        || result.heading.is_some()
        || result.speed_north.is_some()
        || result.home_latitude.is_some()
    {
        Some(result)
    } else {
        None
    }
}

/// Decode DJI DroneID flight purpose packet (subcommand 0x11)
/// Contains user-entered information about the drone and flight.
fn decode_flight_purpose(data: &[u8]) -> Option<DjiDroneIdData> {
    if data.len() < 21 {
        return None;
    }

    let mut result = DjiDroneIdData::default();

    // Serial number is also present in purpose packets at offset 5
    let serial = extract_ascii(&data[5..21.min(data.len())]);
    if !serial.is_empty() {
        result.serial_number = Some(serial);
    }

    if result.serial_number.is_some() {
        Some(result)
    } else {
        None
    }
}

fn extract_ascii(data: &[u8]) -> String {
    let end = data.iter().position(|&b| b == 0).unwrap_or(data.len());
    let s: String = data[..end]
        .iter()
        .filter(|&&b| b >= 0x20 && b < 0x7F)
        .map(|&b| b as char)
        .collect();
    s.trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_extract_ascii_simple() {
        let data = b"1ZNBH2K00C0001\x00\x00";
        let result = extract_ascii(data);
        assert_eq!(result, "1ZNBH2K00C0001");
    }

    #[test]
    fn test_extract_ascii_with_control_chars() {
        let data = [0x31, 0x5A, 0x4E, 0x42, 0x00, 0x01, 0x02]; // "1ZNB" + nulls
        let result = extract_ascii(&data);
        assert_eq!(result, "1ZNB");
    }

    #[test]
    fn test_extract_ascii_empty() {
        let data = [0x00, 0x00, 0x00];
        let result = extract_ascii(&data);
        assert_eq!(result, "");
    }

    #[test]
    fn test_decode_dji_payload_too_short() {
        // Payload shorter than minimum 53 bytes
        let short_data = vec![0u8; 20];
        let result = decode_dji_droneid(&short_data, 0x10);
        assert!(result.is_none());
    }

    #[test]
    fn test_decode_dji_valid_structure() {
        // Create a valid DJI DroneID payload (minimum 53 bytes)
        let mut data = vec![0u8; 75];

        // Version
        data[0] = 0x01;

        // Sequence number (2 bytes LE at offset 1)
        data[1] = 0x01;
        data[2] = 0x00;

        // State info (2 bytes LE at offset 3)
        data[3] = 0x07; // Motor on, in air
        data[4] = 0x00;

        // Serial number (16 bytes at offset 5)
        let serial = b"1ZNBH2K00C0001  "; // 16 chars
        data[5..21].copy_from_slice(serial);

        // Longitude (4 bytes s32le at offset 21) - approx -74.006 degrees
        // raw = degrees * (PI/180) * 174533.0
        let lon_raw: i32 = ((-74.006_f64) * 174533.0) as i32;
        data[21..25].copy_from_slice(&lon_raw.to_le_bytes());

        // Latitude (4 bytes s32le at offset 25) - approx 40.713 degrees
        let lat_raw: i32 = ((40.713_f64) * 174533.0) as i32;
        data[25..29].copy_from_slice(&lat_raw.to_le_bytes());

        // Altitude (2 bytes s16le at offset 29) - 50 meters
        let alt: i16 = 50;
        data[29..31].copy_from_slice(&alt.to_le_bytes());

        // Height AGL (2 bytes s16le at offset 31) - 45 meters
        let height: i16 = 45;
        data[31..33].copy_from_slice(&height.to_le_bytes());

        // Velocity north (2 bytes s16le at offset 33) - 500 cm/s = 5 m/s
        let v_north: i16 = 500;
        data[33..35].copy_from_slice(&v_north.to_le_bytes());

        // Velocity east (2 bytes s16le at offset 35) - 300 cm/s = 3 m/s
        let v_east: i16 = 300;
        data[35..37].copy_from_slice(&v_east.to_le_bytes());

        // Velocity up (2 bytes s16le at offset 37) - 100 cm/s = 1 m/s
        let v_up: i16 = 100;
        data[37..39].copy_from_slice(&v_up.to_le_bytes());

        // Pitch (2 bytes s16le at offset 39)
        data[39] = 0;
        data[40] = 0;

        // Roll (2 bytes s16le at offset 41)
        data[41] = 0;
        data[42] = 0;

        // Yaw/heading (2 bytes s16le at offset 43) - 90 degrees = 9000 centidegrees
        let yaw: i16 = 9000; // 90 degrees
        data[43..45].copy_from_slice(&yaw.to_le_bytes());

        // Home lon (4 bytes s32le at offset 45)
        data[45..49].copy_from_slice(&lon_raw.to_le_bytes());

        // Home lat (4 bytes s32le at offset 49)
        data[49..53].copy_from_slice(&lat_raw.to_le_bytes());

        // Product type at offset 53
        data[53] = 0x01; // Some product type

        let result = decode_dji_droneid(&data, 0x10);
        assert!(result.is_some());

        let decoded = result.unwrap();
        assert_eq!(decoded.serial_number, Some("1ZNBH2K00C0001".to_string()));
        assert!(decoded.latitude.is_some());
        assert!(decoded.longitude.is_some());
        assert!(decoded.altitude_pressure.is_some());
        assert!(decoded.height_agl.is_some());

        // Check approximate values
        let lat = decoded.latitude.unwrap();
        let lon = decoded.longitude.unwrap();
        assert!(lat > 40.0 && lat < 41.0, "Latitude {} out of expected range", lat);
        assert!(lon > -75.0 && lon < -73.0, "Longitude {} out of expected range", lon);

        let alt = decoded.altitude_pressure.unwrap();
        assert_eq!(alt, 50.0);

        let height = decoded.height_agl.unwrap();
        assert_eq!(height, 45.0);
    }

    #[test]
    fn test_decode_dji_v2_encrypted_payload() {
        // DJI DroneID V2 encrypts telemetry but keeps serial cleartext
        let mut data = vec![0u8; 75];
        data[0] = 0x02; // Version 2 = encrypted telemetry
        data[1] = 0x01; data[2] = 0x00; // seq
        data[3] = 0x07; data[4] = 0x00; // state_info
        let serial = b"4NECS1234567890 ";
        data[5..21].copy_from_slice(serial);
        // Bytes 21-52 would be encrypted gibberish — fill with random-looking data
        for i in 21..53 { data[i] = (i * 7 + 13) as u8; }
        data[53] = 0x03; // product_type (still cleartext)

        let result = decode_dji_droneid(&data, 0x10);
        assert!(result.is_some(), "V2 should still decode serial+product_type");
        let decoded = result.unwrap();
        assert_eq!(decoded.serial_number, Some("4NECS1234567890".to_string()));
        assert_eq!(decoded.product_type, Some(0x03));
        // Telemetry fields must be None for V2 — they're encrypted
        assert!(decoded.latitude.is_none(), "V2 latitude should be None (encrypted)");
        assert!(decoded.longitude.is_none(), "V2 longitude should be None (encrypted)");
        assert!(decoded.altitude_pressure.is_none(), "V2 altitude should be None (encrypted)");
        assert!(decoded.heading.is_none(), "V2 heading should be None (encrypted)");
        assert!(decoded.speed_north.is_none(), "V2 speed should be None (encrypted)");
    }

    #[test]
    fn test_decode_dji_ocusync_format() {
        // OcuSync format: oui_type=0x13, data[0]=subcommand, data[1..]=payload
        // This tests the 60:60:1F OUI format where vendor type 0x13 wraps the real subcommand
        let mut payload = vec![0u8; 75];

        // Version (V1 for this test — V2 OcuSync would have encrypted telemetry)
        payload[0] = 0x01;

        // Sequence number
        payload[1] = 0x42;
        payload[2] = 0x00;

        // State info
        payload[3] = 0x07;
        payload[4] = 0x00;

        // Serial number at offset 5
        let serial = b"3NEASL1234567890";
        payload[5..21].copy_from_slice(serial);

        // Longitude at offset 21
        let lon_raw: i32 = ((-74.006_f64) * 174533.0) as i32;
        payload[21..25].copy_from_slice(&lon_raw.to_le_bytes());

        // Latitude at offset 25
        let lat_raw: i32 = ((40.713_f64) * 174533.0) as i32;
        payload[25..29].copy_from_slice(&lat_raw.to_le_bytes());

        // Altitude at offset 29
        let alt: i16 = 100;
        payload[29..31].copy_from_slice(&alt.to_le_bytes());

        // Build OcuSync data: [subcommand=0x10, payload...]
        let mut ocusync_data = vec![0x10u8]; // subcommand
        ocusync_data.extend_from_slice(&payload);

        // Decode with oui_type=0x13 (OcuSync vendor type)
        let result = decode_dji_droneid(&ocusync_data, 0x13);
        assert!(result.is_some(), "OcuSync format should decode successfully");

        let decoded = result.unwrap();
        assert_eq!(decoded.serial_number, Some("3NEASL1234567890".to_string()));
        assert!(decoded.latitude.is_some(), "Should have latitude");
        assert!(decoded.longitude.is_some(), "Should have longitude");

        let lat = decoded.latitude.unwrap();
        let lon = decoded.longitude.unwrap();
        assert!(lat > 40.0 && lat < 41.0, "Latitude {} out of expected range", lat);
        assert!(lon > -75.0 && lon < -73.0, "Longitude {} out of expected range", lon);
        assert_eq!(decoded.altitude_pressure, Some(100.0));
    }

    #[test]
    fn test_decode_flight_purpose() {
        let mut data = vec![0u8; 30];

        // Serial at offset 5
        let serial = b"3NEASL1234567890";
        data[5..21].copy_from_slice(serial);

        let result = decode_dji_droneid(&data, 0x11);
        assert!(result.is_some());

        let decoded = result.unwrap();
        assert_eq!(decoded.serial_number, Some("3NEASL1234567890".to_string()));
    }
}
