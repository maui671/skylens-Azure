use regex::Regex;
use std::sync::LazyLock;
use tracing::trace;

/// Compiled regex patterns for registration ID extraction from SelfID descriptions.
/// Patterns support common CAA/FAA registration formats:
/// - FAA: "FA3-xxxx" (US drone registration)
/// - Swiss FOCA: "CHE-xxxx"
/// - UK CAA: "OP-xxxx" or "FLY-xxxx"
/// - EU: country code + alphanumeric (e.g., "DEU-xxxxx", "FRA-xxxxx")
/// - Generic: "REG-xxxx" or alphanumeric with hyphens
static REGISTRATION_PATTERNS: LazyLock<Vec<Regex>> = LazyLock::new(|| {
    vec![
        // FAA registration (e.g., FA3R2K4N5P)
        Regex::new(r"\b(FA[0-9][A-Z0-9]{6,8})\b").unwrap(),
        // Country-prefix format (e.g., CHE-1234, DEU-ABC123, FRA-12345)
        Regex::new(r"\b([A-Z]{2,3}-[A-Z0-9]{3,10})\b").unwrap(),
        // UK CAA operator ID (e.g., OP-12345678)
        Regex::new(r"\b(OP-[A-Z0-9]{6,12})\b").unwrap(),
        // UK Flyer ID (e.g., FLY-12345678)
        Regex::new(r"\b(FLY-[A-Z0-9]{6,12})\b").unwrap(),
        // Generic REG- prefix
        Regex::new(r"\b(REG-[A-Z0-9]{3,12})\b").unwrap(),
    ]
});

/// OpenDroneID message types (ASTM F3411)
#[derive(Debug, Clone, Copy, PartialEq)]
#[repr(u8)]
pub enum MessageType {
    BasicId = 0x0,
    Location = 0x1,
    Auth = 0x2,
    SelfId = 0x3,
    System = 0x4,
    OperatorId = 0x5,
    MessagePack = 0xF,
}

/// Decoded RemoteID data aggregated from multiple message types
#[derive(Debug, Clone, Default)]
pub struct RemoteIdData {
    // BasicID
    pub id_type: u8,
    pub ua_type: u8,
    pub uas_id: Option<String>,

    // Location
    pub latitude: Option<f64>,
    pub longitude: Option<f64>,
    pub altitude_geodetic: Option<f32>,
    pub altitude_pressure: Option<f32>,
    pub height_agl: Option<f32>,
    pub height_reference: u8,
    pub speed: Option<f32>,
    pub vertical_speed: Option<f32>,
    pub heading: Option<f32>,
    pub operational_status: u8,

    // System
    pub operator_latitude: Option<f64>,
    pub operator_longitude: Option<f64>,
    pub operator_altitude: Option<f32>,
    pub operator_location_type: u8,

    // OperatorID
    pub operator_id: Option<String>,

    // SelfID
    pub self_id_description: Option<String>,
    /// Registration/CAA ID extracted from SelfID description
    pub registration: Option<String>,

    // Location accuracy (ASTM F3411-22a bytes 19-22)
    pub horizontal_accuracy: Option<u8>,
    pub vertical_accuracy: Option<u8>,
    pub speed_accuracy: Option<u8>,
    pub baro_accuracy: Option<u8>,
    /// Timestamp in 0.1s increments since the hour (ASTM F3411)
    pub timestamp_tenths_since_hour: Option<u16>,

    // System extras (ASTM F3411-22a)
    pub classification_type: u8,
    pub area_count: Option<u16>,
    pub area_radius: Option<u8>,
    pub area_ceiling: Option<f32>,
    pub area_floor: Option<f32>,

    // Auth
    pub auth_type: Option<u8>,
    pub auth_page_count: u8,
    pub auth_data_present: bool,
}

/// Decode OpenDroneID payload from a vendor-specific IE
/// The data starts after the OUI + OUI type bytes (and message counter for NAN)
pub fn decode_remoteid(data: &[u8]) -> Option<RemoteIdData> {
    if data.is_empty() {
        return None;
    }

    // Try decoding from the start of the data
    if let Some(result) = try_decode_odid(data) {
        return Some(result);
    }

    // Fallback: some implementations prepend extra byte(s) before the ODID payload.
    // Known cases:
    // - NAN message counter not stripped by frame parser
    // - DJI RemoteID prefix byte (0xFC) before standard ODID MessagePack
    // Always try skipping 1 byte if the initial decode failed.
    if data.len() > 1 {
        trace!(
            skipped_byte = format!("0x{:02X}", data[0]),
            "Retrying ODID decode after skipping 1 prefix byte"
        );
        if let Some(result) = try_decode_odid(&data[1..]) {
            return Some(result);
        }
    }

    None
}

/// Try to decode ODID payload starting at the given offset
fn try_decode_odid(data: &[u8]) -> Option<RemoteIdData> {
    if data.is_empty() {
        return None;
    }

    let mut result = RemoteIdData::default();
    let mut decoded_any = false;

    // Could be a single message or a message pack
    let msg_type = data[0] >> 4;

    if msg_type == MessageType::MessagePack as u8 {
        // Message pack: contains multiple messages
        // Per ASTM F3411-22a: byte 0 = header (type+version), byte 1 = msg_size, byte 2 = msg_count
        if data.len() < 3 {
            return None;
        }
        let msg_size_raw = data[1] as usize;
        let msg_count = data[2] as usize;
        let msg_size = if msg_size_raw == 0 { 25 } else { msg_size_raw };

        // Sanity: standard ODID messages are 25 bytes; reject obviously wrong sizes
        if msg_size > 50 || msg_count == 0 || msg_count > 20 {
            return None;
        }

        for i in 0..msg_count {
            let offset = 3 + i * msg_size;
            if offset + msg_size > data.len() {
                break;
            }
            if decode_single_message(&data[offset..offset + msg_size], &mut result) {
                decoded_any = true;
            }
        }
    } else {
        // Single message
        decoded_any = decode_single_message(data, &mut result);
    }

    if decoded_any {
        Some(result)
    } else {
        None
    }
}

/// Decode a single 25-byte OpenDroneID message
fn decode_single_message(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.is_empty() {
        return false;
    }

    let msg_type = data[0] >> 4;
    let proto_version = data[0] & 0x0F;

    trace!(msg_type, proto_version, len = data.len(), "Decoding ODID message");

    match msg_type {
        0x0 => decode_basic_id(data, result),
        0x1 => decode_location(data, result),
        0x2 => decode_auth(data, result),
        0x3 => decode_self_id(data, result),
        0x4 => decode_system(data, result),
        0x5 => decode_operator_id(data, result),
        _ => false,
    }
}

/// Decode BasicID message (type 0x0)
fn decode_basic_id(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.len() < 25 {
        return false;
    }

    result.id_type = (data[1] >> 4) & 0x0F;
    result.ua_type = data[1] & 0x0F;

    // UAS ID is bytes 2-21 (20 bytes), null-terminated ASCII
    let id_bytes = &data[2..22];
    let id = extract_ascii_string(id_bytes);
    if !id.is_empty() {
        result.uas_id = Some(id);
    }

    true
}

/// Decode Location message (type 0x1)
fn decode_location(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.len() < 25 {
        return false;
    }

    // Byte 1 bitfield layout (ASTM F3411-22a, opendroneid-core-c):
    //   bit 0:    SpeedMultiplier
    //   bit 1:    EW Direction Segment
    //   bit 2:    HeightType (0=takeoff, 1=ground)
    //   bit 3:    Reserved
    //   bits 4-7: Status (operational status)
    let speed_mult = data[1] & 0x01;
    let ew_direction = (data[1] >> 1) & 0x01;
    result.height_reference = (data[1] >> 2) & 0x01;
    result.operational_status = (data[1] >> 4) & 0x0F;

    // Direction (byte 2): full byte, 0-179 range, add 180 if EW flag set
    let dir_raw = data[2];
    if dir_raw <= 179 {
        let direction = if ew_direction != 0 {
            dir_raw as u16 + 180
        } else {
            dir_raw as u16
        };
        result.heading = Some(direction as f32);
    }

    // Horizontal speed (byte 3): full byte * resolution
    // SpeedMultiplier is in byte 1 bit 0 (NOT byte 3 MSB)
    // mult=0: speed = raw * 0.25 m/s (range 0-63.75)
    // mult=1: speed = (raw * 0.75) + 63.75 m/s (range 63.75-254.25)
    // Unknown sentinel: 255
    let speed_raw = data[3];
    if speed_raw != 255 {
        if speed_mult == 0 {
            result.speed = Some(speed_raw as f32 * 0.25);
        } else {
            result.speed = Some(speed_raw as f32 * 0.75 + 63.75);
        }
    }

    // Vertical speed (byte 4): INT8 (signed) * 0.5 m/s
    // Per opendroneid-core-c: SpeedVertical is int8_t, decoded = raw_signed * 0.5
    // Range: -64.0 to +63.5 m/s, unknown sentinel = 63
    let vs_raw = data[4] as i8;
    if vs_raw != 63 {
        result.vertical_speed = Some(vs_raw as f32 * 0.5);
    }

    // Latitude (bytes 5-8): encoded as 1e-7 degrees
    let lat_raw = i32::from_le_bytes([data[5], data[6], data[7], data[8]]);
    if lat_raw != 0 {
        let lat = lat_raw as f64 / 1e7;
        if (-90.0..=90.0).contains(&lat) {
            result.latitude = Some(lat);
        }
    }

    // Longitude (bytes 9-12): encoded as 1e-7 degrees
    let lon_raw = i32::from_le_bytes([data[9], data[10], data[11], data[12]]);
    if lon_raw != 0 {
        let lon = lon_raw as f64 / 1e7;
        if (-180.0..=180.0).contains(&lon) {
            result.longitude = Some(lon);
        }
    }

    // Altitude pressure (bytes 13-14): encoded as 0.5m, offset -1000m
    // Unknown sentinel: 0 (no data) or 0xFFFF (unknown per ASTM F3411 Table 12)
    let alt_press_raw = u16::from_le_bytes([data[13], data[14]]);
    if alt_press_raw != 0 && alt_press_raw != 0xFFFF {
        result.altitude_pressure = Some(alt_press_raw as f32 * 0.5 - 1000.0);
    }

    // Altitude geodetic (bytes 15-16)
    let alt_geo_raw = u16::from_le_bytes([data[15], data[16]]);
    if alt_geo_raw != 0 && alt_geo_raw != 0xFFFF {
        result.altitude_geodetic = Some(alt_geo_raw as f32 * 0.5 - 1000.0);
    }

    // Height AGL (bytes 17-18)
    let height_raw = u16::from_le_bytes([data[17], data[18]]);
    if height_raw != 0 && height_raw != 0xFFFF {
        result.height_agl = Some(height_raw as f32 * 0.5 - 1000.0);
    }

    // Accuracy fields (bytes 19-22, per ASTM F3411-22a)
    // Byte 19: vertical_accuracy (bits 0-3) + horizontal_accuracy (bits 4-7)
    let acc_byte = data[19];
    let vert_acc = acc_byte & 0x0F;
    let horiz_acc = (acc_byte >> 4) & 0x0F;
    if horiz_acc != 0 {
        result.horizontal_accuracy = Some(horiz_acc);
    }
    if vert_acc != 0 {
        result.vertical_accuracy = Some(vert_acc);
    }

    // Byte 20: speed_accuracy (bits 0-3) + baro_accuracy (bits 4-7)
    let acc_byte2 = data[20];
    let spd_acc = acc_byte2 & 0x0F;
    let baro_acc = (acc_byte2 >> 4) & 0x0F;
    if spd_acc != 0 {
        result.speed_accuracy = Some(spd_acc);
    }
    if baro_acc != 0 {
        result.baro_accuracy = Some(baro_acc);
    }

    // Bytes 21-22: timestamp (0.1s since the hour, uint16 LE)
    let ts_raw = u16::from_le_bytes([data[21], data[22]]);
    if ts_raw != 0xFFFF {
        result.timestamp_tenths_since_hour = Some(ts_raw);
    }

    true
}

/// Decode System message (type 0x4)
fn decode_system(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.len() < 25 {
        return false;
    }

    result.operator_location_type = data[1] & 0x03;
    // Classification type (bits 2-3 of byte 1, per ASTM F3411 / ASD-STAN EN 4709-002)
    result.classification_type = (data[1] >> 2) & 0x07;

    // Operator latitude (bytes 2-5)
    let lat_raw = i32::from_le_bytes([data[2], data[3], data[4], data[5]]);
    if lat_raw != 0 {
        let lat = lat_raw as f64 / 1e7;
        if (-90.0..=90.0).contains(&lat) {
            result.operator_latitude = Some(lat);
        }
    }

    // Operator longitude (bytes 6-9)
    let lon_raw = i32::from_le_bytes([data[6], data[7], data[8], data[9]]);
    if lon_raw != 0 {
        let lon = lon_raw as f64 / 1e7;
        if (-180.0..=180.0).contains(&lon) {
            result.operator_longitude = Some(lon);
        }
    }

    // Area count (bytes 10-11): number of UAS in the area
    let area_count = u16::from_le_bytes([data[10], data[11]]);
    if area_count > 0 {
        result.area_count = Some(area_count);
    }

    // Area radius (byte 12): encoded as radius * 10m
    if data[12] != 0 {
        result.area_radius = Some(data[12]);
    }

    // Area ceiling (bytes 13-14): 0.5m resolution, -1000m offset
    let ceiling_raw = u16::from_le_bytes([data[13], data[14]]);
    if ceiling_raw != 0 {
        result.area_ceiling = Some(ceiling_raw as f32 * 0.5 - 1000.0);
    }

    // Area floor (bytes 15-16): 0.5m resolution, -1000m offset
    let floor_raw = u16::from_le_bytes([data[15], data[16]]);
    if floor_raw != 0 {
        result.area_floor = Some(floor_raw as f32 * 0.5 - 1000.0);
    }

    // Byte 17: ClassEU (bits 4-7) | CategoryEU (bits 0-3) — skipped for now

    // Operator altitude (bytes 18-19): 0.5m resolution, -1000m offset
    let alt_raw = u16::from_le_bytes([data[18], data[19]]);
    if alt_raw != 0 && alt_raw != 0xFFFF {
        result.operator_altitude = Some(alt_raw as f32 * 0.5 - 1000.0);
    }

    true
}

/// Decode OperatorID message (type 0x5)
fn decode_operator_id(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.len() < 25 {
        return false;
    }

    // Operator ID is bytes 2-21
    let id = extract_ascii_string(&data[2..22]);
    if !id.is_empty() {
        result.operator_id = Some(id);
    }

    true
}

/// Decode SelfID message (type 0x3)
/// Also attempts to extract registration/CAA ID from the description field
fn decode_self_id(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.len() < 25 {
        return false;
    }

    let desc = extract_ascii_string(&data[2..25]);
    if !desc.is_empty() {
        // Try to extract registration ID from description
        if let Some(reg) = extract_registration(&desc) {
            trace!(registration = %reg, description = %desc, "Extracted registration from SelfID");
            result.registration = Some(reg);
        }
        result.self_id_description = Some(desc);
    }

    true
}

/// Extract registration/CAA ID from SelfID description text.
/// Searches for common patterns like FAA (FA3-xxxx), Swiss (CHE-xxxx), etc.
pub fn extract_registration(description: &str) -> Option<String> {
    // Uppercase for matching (registrations are typically uppercase)
    let upper = description.to_uppercase();

    for pattern in REGISTRATION_PATTERNS.iter() {
        if let Some(captures) = pattern.captures(&upper) {
            if let Some(m) = captures.get(1) {
                return Some(m.as_str().to_string());
            }
        }
    }

    None
}

/// Decode Auth message (type 0x2)
/// ASTM F3411 Authentication message — page 0 contains auth type + page count
fn decode_auth(data: &[u8], result: &mut RemoteIdData) -> bool {
    if data.len() < 25 {
        return false;
    }

    // Byte 1: page number (lower 4 bits) + auth type (upper 4 bits)
    // Per ASTM F3411-22a: bits 0-3 = DataPage, bits 4-7 = AuthType
    let page_number = data[1] & 0x0F;
    let auth_type = (data[1] >> 4) & 0x0F;

    result.auth_data_present = true;

    if page_number == 0 {
        // Page 0 has additional header: page_count(1) + length(1) + timestamp(4)
        result.auth_type = Some(auth_type);
        if data.len() >= 4 {
            result.auth_page_count = data[2];
        }
    }

    true
}

/// Extract null-terminated ASCII string from bytes
fn extract_ascii_string(data: &[u8]) -> String {
    let end = data.iter().position(|&b| b == 0).unwrap_or(data.len());
    String::from_utf8_lossy(&data[..end]).trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a 25-byte BasicID message with the given serial
    fn build_basic_id(serial: &str) -> [u8; 25] {
        let mut msg = [0u8; 25];
        msg[0] = 0x02; // type=0 (BasicID), version=2
        msg[1] = 0x12; // id_type=1 (Serial), ua_type=2 (Aeroplane)
        let bytes = serial.as_bytes();
        msg[2..2 + bytes.len().min(20)].copy_from_slice(&bytes[..bytes.len().min(20)]);
        msg
    }

    /// Build a 25-byte Location message with the given lat/lon
    fn build_location(lat: f64, lon: f64) -> [u8; 25] {
        let mut msg = [0u8; 25];
        msg[0] = 0x12; // type=1 (Location), version=2
        let lat_raw = (lat * 1e7) as i32;
        let lon_raw = (lon * 1e7) as i32;
        msg[5..9].copy_from_slice(&lat_raw.to_le_bytes());
        msg[9..13].copy_from_slice(&lon_raw.to_le_bytes());
        msg
    }

    /// Build a 25-byte System message with the given operator lat/lon
    fn build_system(lat: f64, lon: f64) -> [u8; 25] {
        let mut msg = [0u8; 25];
        msg[0] = 0x42; // type=4 (System), version=2
        let lat_raw = (lat * 1e7) as i32;
        let lon_raw = (lon * 1e7) as i32;
        msg[2..6].copy_from_slice(&lat_raw.to_le_bytes());
        msg[6..10].copy_from_slice(&lon_raw.to_le_bytes());
        msg
    }

    #[test]
    fn test_decode_basic_id() {
        let msg = build_basic_id("1581F5FJD22B800B903B");
        let result = decode_remoteid(&msg).unwrap();
        assert_eq!(result.uas_id.as_deref(), Some("1581F5FJD22B800B903B"));
        assert_eq!(result.id_type, 1);
    }

    #[test]
    fn test_decode_location_valid() {
        let msg = build_location(18.2369, -65.6600);
        let result = decode_remoteid(&msg).unwrap();
        let lat = result.latitude.unwrap();
        let lon = result.longitude.unwrap();
        assert!((lat - 18.2369).abs() < 0.001);
        assert!((lon - (-65.6600)).abs() < 0.001);
    }

    #[test]
    fn test_decode_location_rejects_bogus_lat() {
        // lat=94.30 is impossible (>90)
        let msg = build_location(94.30, 117.79);
        let result = decode_remoteid(&msg).unwrap();
        assert!(result.latitude.is_none(), "Latitude 94.30 should be rejected");
    }

    #[test]
    fn test_decode_system_rejects_bogus_coords() {
        // 30.21/82.58 is valid individually, but test impossible lat
        let msg = build_system(95.0, 200.0);
        let result = decode_remoteid(&msg).unwrap();
        assert!(result.operator_latitude.is_none(), "Latitude 95 should be rejected");
        assert!(result.operator_longitude.is_none(), "Longitude 200 should be rejected");
    }

    #[test]
    fn test_decode_system_valid() {
        let msg = build_system(18.2369, -65.6600);
        let result = decode_remoteid(&msg).unwrap();
        let lat = result.operator_latitude.unwrap();
        let lon = result.operator_longitude.unwrap();
        assert!((lat - 18.2369).abs() < 0.001);
        assert!((lon - (-65.6600)).abs() < 0.001);
    }

    #[test]
    fn test_decode_dji_prefixed_message_pack() {
        // DJI prepends 0xFC before standard ODID MessagePack
        // FC = DJI prefix, F2 = type 0xF (MessagePack) version 2,
        // 19 = msg_size 25, 03 = msg_count 3
        let basic = build_basic_id("1581F5FJD22B800B903B");
        let location = build_location(18.2369, -65.6600);
        let system = build_system(18.2369, -65.6600);

        let mut data = vec![0xFC]; // DJI prefix
        data.push(0xF2); // MessagePack header: type=0xF, version=2
        data.push(0x19); // msg_size = 25
        data.push(0x03); // msg_count = 3
        data.extend_from_slice(&basic);
        data.extend_from_slice(&location);
        data.extend_from_slice(&system);
        assert_eq!(data.len(), 79); // 1 + 3 + 75

        let result = decode_remoteid(&data).unwrap();
        assert_eq!(result.uas_id.as_deref(), Some("1581F5FJD22B800B903B"));
        let lat = result.latitude.unwrap();
        assert!((lat - 18.2369).abs() < 0.001);
        let op_lat = result.operator_latitude.unwrap();
        assert!((op_lat - 18.2369).abs() < 0.001);
    }

    #[test]
    fn test_message_pack_rejects_absurd_size() {
        // msg_size=242 is way too large, should be rejected
        let mut data = vec![0xF0, 0xF2, 0x19]; // type=0xF, msg_size=242, count=25
        data.extend_from_slice(&[0u8; 76]);
        let result = decode_remoteid(&data);
        assert!(result.is_none());
    }
}
