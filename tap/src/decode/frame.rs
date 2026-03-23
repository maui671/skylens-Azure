use anyhow::Result;
use tracing::{debug, info, trace};

/// Radiotap header info extracted from the frame
#[derive(Debug, Clone, Default)]
pub struct RadiotapInfo {
    pub rssi: i32,
    pub channel: u16,
    pub frequency: u16,
    /// Frame has bad FCS (corrupted) — radiotap flags bit 6
    pub bad_fcs: bool,
}

/// Parsed 802.11 management frame
#[derive(Debug, Clone)]
pub struct ParsedFrame {
    pub radiotap: RadiotapInfo,
    pub frame_type: FrameType,
    pub src_mac: [u8; 6],
    pub dst_mac: [u8; 6],
    pub bssid: [u8; 6],
    pub ssid: Option<String>,
    pub beacon_interval: Option<u16>,
    pub vendor_ies: Vec<VendorIE>,
    /// 802.11 Capability Info from fixed parameters (Beacon/ProbeResponse)
    pub capability_info: Option<u16>,
    /// 802.11 sequence number from header (12 bits, 0-4095)
    pub sequence_number: u16,
    /// DS Parameter Set channel (IE tag 3)
    pub ds_channel: Option<u8>,
    /// Country code from Country IE (tag 7), e.g. "US", "CN"
    pub country_code: Option<String>,
    /// Supported rates (IE tag 1) in 0.5 Mbps units
    pub supported_rates: Vec<u8>,
    /// Extended supported rates (IE tag 50)
    pub extended_rates: Vec<u8>,
    /// HT Capabilities info field (IE tag 45), first 2 bytes
    pub ht_capabilities: Option<u16>,
    /// RSN (WPA2/WPA3) cipher suite count — indicates security type
    pub rsn_info: Option<RsnInfo>,
}

#[derive(Debug, Clone, PartialEq)]
pub enum FrameType {
    Beacon,
    ProbeResponse,
    ProbeRequest,
    Action,
    Other(u8, u8),
}

/// Vendor-specific Information Element (IE)
#[derive(Debug, Clone)]
pub struct VendorIE {
    pub oui: [u8; 3],
    pub oui_type: u8,
    pub data: Vec<u8>,
}

/// RSN (Robust Security Network) information from IE tag 48
#[derive(Debug, Clone)]
pub struct RsnInfo {
    /// RSN version (typically 1)
    pub version: u16,
    /// Group cipher OUI + type
    pub group_cipher: [u8; 4],
    /// Number of pairwise cipher suites
    pub pairwise_count: u16,
}

// Well-known OUIs for RemoteID/DroneID vendor IEs
const ASTM_OUI: [u8; 3] = [0xFA, 0x0B, 0xBC]; // ASD-STAN / ASTM F3411 RemoteID
const DJI_OUI: [u8; 3] = [0x26, 0x37, 0x12]; // DJI DroneID (in vendor IE)
const DJI_OUI_ALT: [u8; 3] = [0x26, 0x6F, 0x48]; // DJI DroneID alternate OUI
const DJI_OUI_OCUSYNC: [u8; 3] = [0x60, 0x60, 0x1F]; // DJI OcuSync telemetry frames

// Parrot drone OUIs
const PARROT_OUI: [u8; 3] = [0x90, 0x03, 0xB7]; // Parrot SA
const PARROT_OUI_ALT: [u8; 3] = [0xA0, 0x14, 0x3D]; // Parrot Drones SAS

// Autel drone OUIs (NOTE: 60:60:1F is DJI, not Autel - they share MAC space)
const AUTEL_OUI: [u8; 3] = [0x70, 0x88, 0x6B]; // Autel newer models (EVO II, etc)
const AUTEL_OUI_ALT: [u8; 3] = [0xEC, 0x5B, 0xCD]; // Autel Robotics
const AUTEL_OUI_ALT2: [u8; 3] = [0x18, 0xD7, 0x93]; // Autel Intelligent

// WiFi NAN (Neighbor Awareness Networking) for RemoteID
const NAN_SDF_ACTION_CATEGORY: u8 = 0x04; // Public action frame
const NAN_SDF_ACTION_CODE: u8 = 0x09; // Vendor specific
const WFA_OUI: [u8; 3] = [0x50, 0x6F, 0x9A]; // Wi-Fi Alliance OUI
const WFA_NAN_TYPE: u8 = 0x13; // NAN (Wi-Fi Aware)
const NAN_ATTR_SERVICE_DESCRIPTOR: u8 = 0x03; // Service Descriptor Attribute

/// Parse a raw captured packet (radiotap + 802.11)
pub fn parse_packet(data: &[u8]) -> Result<ParsedFrame> {
    if data.len() < 4 {
        anyhow::bail!("Packet too short for radiotap header");
    }

    // Parse radiotap header
    let rt_len = u16::from_le_bytes([data[2], data[3]]) as usize;
    if data.len() < rt_len || rt_len < 8 {
        anyhow::bail!("Invalid radiotap header length");
    }

    let radiotap = parse_radiotap(&data[..rt_len]);

    // 802.11 frame starts after radiotap
    let frame = &data[rt_len..];
    if frame.len() < 24 {
        anyhow::bail!("Frame too short for 802.11 header");
    }

    // Frame control field
    let fc = u16::from_le_bytes([frame[0], frame[1]]);
    let frame_type_bits = (fc >> 2) & 0x03;
    let frame_subtype_bits = (fc >> 4) & 0x0F;

    let frame_type = match (frame_type_bits, frame_subtype_bits) {
        (0, 8) => FrameType::Beacon,
        (0, 5) => FrameType::ProbeResponse,
        (0, 4) => FrameType::ProbeRequest,
        (0, 13) => FrameType::Action,     // Action
        (0, 14) => FrameType::Action,     // Action No Ack (used by NAN/WiFi Aware)
        (t, s) => FrameType::Other(t as u8, s as u8),
    };

    // MAC addresses from the 802.11 header
    let mut dst_mac = [0u8; 6];
    let mut src_mac = [0u8; 6];
    let mut bssid = [0u8; 6];

    dst_mac.copy_from_slice(&frame[4..10]);
    src_mac.copy_from_slice(&frame[10..16]);
    bssid.copy_from_slice(&frame[16..22]);

    // Sequence control field: bits 4-15 = sequence number, bits 0-3 = fragment
    let sequence_number = (u16::from_le_bytes([frame[22], frame[23]]) >> 4) & 0x0FFF;

    // Parse beacon interval and capability info from fixed params (Beacon/ProbeResponse only)
    // Fixed params: timestamp(8) + beacon_interval(2) + capability(2)
    let beacon_interval = match frame_type {
        FrameType::Beacon | FrameType::ProbeResponse if frame.len() >= 24 + 10 => {
            Some(u16::from_le_bytes([frame[24 + 8], frame[24 + 9]]))
        }
        _ => None,
    };
    let capability_info = match frame_type {
        FrameType::Beacon | FrameType::ProbeResponse if frame.len() >= 24 + 12 => {
            Some(u16::from_le_bytes([frame[24 + 10], frame[24 + 11]]))
        }
        _ => None,
    };

    // Parse tagged parameters (IEs)
    let mut ssid = None;
    let mut vendor_ies = Vec::new();
    let mut ds_channel = None;
    let mut country_code = None;
    let mut supported_rates = Vec::new();
    let mut extended_rates = Vec::new();
    let mut ht_capabilities = None;
    let mut rsn_info = None;

    // Fixed parameters length differs by frame type
    let ie_offset = match frame_type {
        FrameType::Beacon | FrameType::ProbeResponse => 24 + 12, // 12 bytes fixed params
        FrameType::ProbeRequest => 24,                            // No fixed params
        FrameType::Action => {
            // Action frames have category + action code, then variable body
            if frame.len() > 30 {
                let category = frame[24];
                let action_code = frame[25];

                if category == NAN_SDF_ACTION_CATEGORY && action_code == NAN_SDF_ACTION_CODE {
                    let oui = [frame[26], frame[27], frame[28]];
                    let oui_type = frame[29];

                    debug!(
                        oui = format!("{:02X}:{:02X}:{:02X}", oui[0], oui[1], oui[2]),
                        oui_type = format!("0x{:02X}", oui_type),
                        frame_len = frame.len(),
                        "Action frame: Public Vendor Specific"
                    );

                    if oui == WFA_OUI && oui_type == WFA_NAN_TYPE {
                        // WiFi NAN SDF: parse NAN attributes for RemoteID
                        info!(
                            frame_len = frame.len(),
                            nan_body_len = frame.len() - 30,
                            src = format!("{:02X}:{:02X}:{:02X}:{:02X}:{:02X}:{:02X}",
                                frame[10], frame[11], frame[12], frame[13], frame[14], frame[15]),
                            "NAN SDF frame received"
                        );
                        parse_nan_sdf(&frame[30..], &mut vendor_ies);
                    } else if oui == ASTM_OUI && oui_type == 0x0D {
                        // Direct ASTM RemoteID action frame (non-NAN)
                        vendor_ies.push(VendorIE {
                            oui,
                            oui_type,
                            data: frame[30..].to_vec(),
                        });
                    } else {
                        // Other vendor-specific public action frames (e.g. DJI OcuSync)
                        // Treat the payload as a vendor IE so detection checks can match
                        vendor_ies.push(VendorIE {
                            oui,
                            oui_type,
                            data: frame[30..].to_vec(),
                        });
                    }
                    frame.len() // Don't parse as regular IEs
                } else if category == 127 && frame.len() >= 28 {
                    // Category 127: Vendor Specific Action frame
                    // Structure: cat(1) + OUI(3) + vendor body (no action_code field)
                    let oui = [frame[25], frame[26], frame[27]];
                    let oui_type = if frame.len() > 28 { frame[28] } else { 0 };
                    let data_start = if frame.len() > 28 { 29 } else { 28 };
                    vendor_ies.push(VendorIE {
                        oui,
                        oui_type,
                        data: frame[data_start..].to_vec(),
                    });
                    frame.len() // Don't try to parse as regular IEs
                } else {
                    24 + 2 // category + action code, then try IEs
                }
            } else {
                frame.len()
            }
        }
        _ => frame.len(), // Don't parse IEs for other types
    };

    if ie_offset < frame.len() {
        parse_ies(
            &frame[ie_offset..], &mut ssid, &mut vendor_ies,
            &mut ds_channel, &mut country_code, &mut supported_rates,
            &mut extended_rates, &mut ht_capabilities, &mut rsn_info,
        );
    }

    trace!(
        frame_type = ?frame_type,
        src = format_mac(&src_mac),
        bssid_str = format_mac(&bssid),
        ssid = ssid.as_deref().unwrap_or(""),
        rssi = radiotap.rssi,
        channel = radiotap.channel,
        vendor_ies = vendor_ies.len(),
        "Parsed frame"
    );

    Ok(ParsedFrame {
        radiotap,
        frame_type,
        src_mac,
        dst_mac,
        bssid,
        ssid,
        beacon_interval,
        vendor_ies,
        capability_info,
        sequence_number,
        ds_channel,
        country_code,
        supported_rates,
        extended_rates,
        ht_capabilities,
        rsn_info,
    })
}

/// Parse radiotap header to extract RSSI and channel.
/// Handles extended present bitmasks (bit 31 set = more present words follow).
fn parse_radiotap(data: &[u8]) -> RadiotapInfo {
    let mut info = RadiotapInfo::default();

    if data.len() < 8 {
        return info;
    }

    // Count present bitmask words (bit 31 = another word follows)
    let mut present_words: Vec<u32> = Vec::new();
    let mut pw_offset = 4;

    loop {
        if pw_offset + 4 > data.len() {
            return info;
        }
        let word = u32::from_le_bytes([
            data[pw_offset],
            data[pw_offset + 1],
            data[pw_offset + 2],
            data[pw_offset + 3],
        ]);
        present_words.push(word);
        pw_offset += 4;

        // Bit 31: another present word follows
        if word & (1 << 31) == 0 {
            break;
        }
    }

    let present = present_words[0];
    let mut offset = pw_offset; // Start after all present words

    // Walk through fields in order based on present bits
    // Bit 0: TSFT (8 bytes, aligned to 8)
    if present & (1 << 0) != 0 {
        offset = align(offset, 8);
        offset += 8;
    }
    // Bit 1: Flags (1 byte) — bit 6 = bad FCS
    if present & (1 << 1) != 0 {
        if offset < data.len() {
            let flags = data[offset];
            info.bad_fcs = (flags & (1 << 6)) != 0;
        }
        offset += 1;
    }
    // Bit 2: Rate (1 byte)
    if present & (1 << 2) != 0 {
        offset += 1;
    }
    // Bit 3: Channel (2 bytes freq + 2 bytes flags, aligned to 2)
    if present & (1 << 3) != 0 {
        offset = align(offset, 2);
        if offset + 4 <= data.len() {
            info.frequency = u16::from_le_bytes([data[offset], data[offset + 1]]);
            info.channel = freq_to_channel(info.frequency);
        }
        offset += 4;
    }
    // Bit 4: FHSS (2 bytes)
    if present & (1 << 4) != 0 {
        offset += 2;
    }
    // Bit 5: Antenna signal dBm (1 byte, signed)
    if present & (1 << 5) != 0 {
        if offset < data.len() {
            info.rssi = data[offset] as i8 as i32;
        }
    }

    info
}

/// Parse WiFi NAN Service Discovery Frame (SDF) body for RemoteID.
///
/// NAN SDF structure after [cat + action + OUI(50:6F:9A) + type(0x13)]:
///   NAN attributes in TLV format: attr_id(1) + length(2LE) + body(length)
///
/// RemoteID lives inside Service Descriptor Attribute (attr_id=0x03):
///   Service ID (6B) + Instance ID (1B) + Requestor Instance ID (1B) +
///   Service Control (1B) + [optional fields per control bits] +
///   Service Info Length (1B) + Service Info (variable)
///
/// The Service Info contains: ASTM OUI (FA:0B:BC) + type (0x0D) + OpenDroneID payload
fn parse_nan_sdf(nan_body: &[u8], vendor_ies: &mut Vec<VendorIE>) {
    let mut pos: usize = 0;

    // Walk NAN attributes (TLV: id=1B, len=2B LE, body=len bytes)
    while pos + 3 <= nan_body.len() {
        let attr_id = nan_body[pos];
        let attr_len = u16::from_le_bytes([nan_body[pos + 1], nan_body[pos + 2]]) as usize;
        pos += 3;

        if pos + attr_len > nan_body.len() {
            break;
        }

        debug!(attr_id = format!("0x{:02X}", attr_id), attr_len, "NAN attribute");

        if attr_id == NAN_ATTR_SERVICE_DESCRIPTOR {
            debug!(attr_len, "Found NAN Service Descriptor");
            parse_nan_service_descriptor(&nan_body[pos..pos + attr_len], vendor_ies);
        }

        pos += attr_len;
    }
}

/// Parse a NAN Service Descriptor Attribute to extract RemoteID Service Info.
///
/// Service Descriptor Attribute body:
///   [0-5]   Service ID (6 bytes, SHA-256 hash of service name)
///   [6]     Instance ID
///   [7]     Requestor Instance ID
///   [8]     Service Control (bitmask controls which optional fields follow)
///   [9..]   Optional fields (Binding Bitmap, Matching Filter, Service Response Filter,
///           Service Info) — presence determined by Service Control bits
///
/// Service Control bits:
///   Bit 0-2: Service Type (0=Publish, 1=Subscribe, 2=Follow-up)
///   Bit 3:   Matching Filter Present
///   Bit 4:   Service Response Filter Present
///   Bit 5:   Service Info Present
///   Bit 6:   Discovery Range Limited
///   Bit 7:   Binding Bitmap Present
/// NAN Service ID for OpenDroneID RemoteID: first 6 bytes of SHA-256("org.opendroneid.remoteid")
const REMOTEID_SERVICE_ID: [u8; 6] = [0x88, 0x69, 0x19, 0x9D, 0x92, 0x09];

fn parse_nan_service_descriptor(data: &[u8], vendor_ies: &mut Vec<VendorIE>) {
    // Need at least: Service ID(6) + Instance(1) + Requestor(1) + Control(1) = 9 bytes
    if data.len() < 9 {
        return;
    }

    // Log if service ID doesn't match standard OpenDroneID RemoteID
    // but still parse — ASTM OUI check in Service Info is the real filter
    if data[0..6] != REMOTEID_SERVICE_ID {
        debug!(
            service_id = ?&data[0..6],
            "NAN service descriptor with non-standard service ID, parsing anyway"
        );
    }

    let service_control = data[8];
    let mut offset: usize = 9;

    // Bit 7: Binding Bitmap Present → Bitmap Control(1) + Bitmap(variable)
    // Per NAN spec: Bitmap Control determines bitmap length.
    // Bits 0-3 of control = Bitmap Length in bytes (0 means 1 byte bitmap).
    if service_control & (1 << 7) != 0 {
        if offset >= data.len() { return; }
        let bitmap_control = data[offset];
        let bitmap_len = ((bitmap_control & 0x0F) as usize).max(1);
        let total = 1 + bitmap_len; // control byte + bitmap bytes
        if offset + total > data.len() { return; }
        offset += total;
    }

    // Bit 3: Matching Filter Present → length(1) + filter(length)
    if service_control & (1 << 3) != 0 {
        if offset >= data.len() { return; }
        let mf_len = data[offset] as usize;
        if offset + 1 + mf_len > data.len() { return; }
        offset += 1 + mf_len;
    }

    // Bit 4: Service Response Filter Present → length(1) + filter(length)
    if service_control & (1 << 4) != 0 {
        if offset >= data.len() { return; }
        let srf_len = data[offset] as usize;
        if offset + 1 + srf_len > data.len() { return; }
        offset += 1 + srf_len;
    }

    // Bit 5: Service Info Present → length(1) + info(length)
    if service_control & (1 << 5) != 0 {
        if offset >= data.len() { return; }
        let si_len = data[offset] as usize;
        offset += 1;

        if offset + si_len > data.len() { return; }

        let service_info = &data[offset..offset + si_len];

        // Service Info for RemoteID per ASTM F3411-22a Section 7.2.1 (WiFi NAN):
        //   OUI(3) + OUI_Type(1) + Message_Counter(1) + ODID_Message(s)
        // The Message Counter byte is NAN-transport-specific and must be skipped.
        if service_info.len() >= 6 {
            let oui = [service_info[0], service_info[1], service_info[2]];
            let oui_type = service_info[3];

            if oui == ASTM_OUI && oui_type == 0x0D {
                // Skip OUI(3) + type(1) + message_counter(1) = 5 bytes
                info!(
                    payload_len = service_info.len() - 5,
                    "NAN RemoteID extracted from Service Info (ASTM OUI)"
                );
                vendor_ies.push(VendorIE {
                    oui,
                    oui_type,
                    data: service_info[5..].to_vec(),
                });
            }
        }

        // Also try without OUI prefix — some implementations put raw
        // OpenDroneID message pack directly in Service Info
        if service_info.len() >= 2 && vendor_ies.is_empty() {
            // OpenDroneID message starts with msg_type(4bits)|proto_version(4bits)
            // Valid message types are 0x0-0x5 (BasicID through OperatorID) and 0xF (pack)
            let first_nibble = (service_info[0] >> 4) & 0x0F;
            if first_nibble <= 0x05 || first_nibble == 0x0F {
                vendor_ies.push(VendorIE {
                    oui: ASTM_OUI,
                    oui_type: 0x0D,
                    data: service_info.to_vec(),
                });
            }
        }
    }
}

/// Parse Information Elements from frame body
fn parse_ies(
    data: &[u8],
    ssid: &mut Option<String>,
    vendor_ies: &mut Vec<VendorIE>,
    ds_channel: &mut Option<u8>,
    country_code: &mut Option<String>,
    supported_rates: &mut Vec<u8>,
    extended_rates: &mut Vec<u8>,
    ht_capabilities: &mut Option<u16>,
    rsn_info: &mut Option<RsnInfo>,
) {
    let mut pos = 0;

    while pos + 2 <= data.len() {
        let tag = data[pos];
        let len = data[pos + 1] as usize;
        pos += 2;

        if pos + len > data.len() {
            break;
        }

        let ie_data = &data[pos..pos + len];

        match tag {
            // SSID (tag 0)
            0 => {
                if !ie_data.is_empty() {
                    // Trim at first null byte (some drones null-pad SSIDs)
                    let end = ie_data.iter().position(|&b| b == 0).unwrap_or(ie_data.len());
                    if end > 0 {
                        if let Ok(s) = std::str::from_utf8(&ie_data[..end]) {
                            if !s.is_empty() {
                                *ssid = Some(s.to_string());
                            }
                        }
                    }
                }
            }
            // Supported Rates (tag 1)
            1 => {
                *supported_rates = ie_data.to_vec();
            }
            // DS Parameter Set (tag 3) — current channel
            3 => {
                if ie_data.len() >= 1 {
                    *ds_channel = Some(ie_data[0]);
                }
            }
            // Country (tag 7) — first 2 bytes are country code ASCII
            7 => {
                if ie_data.len() >= 2 {
                    if let Ok(cc) = std::str::from_utf8(&ie_data[..2]) {
                        *country_code = Some(cc.to_string());
                    }
                }
            }
            // HT Capabilities (tag 45) — first 2 bytes are HT capability info
            45 => {
                if ie_data.len() >= 2 {
                    *ht_capabilities = Some(u16::from_le_bytes([ie_data[0], ie_data[1]]));
                }
            }
            // RSN Information (tag 48) — WPA2/WPA3 security
            48 => {
                if ie_data.len() >= 8 {
                    let version = u16::from_le_bytes([ie_data[0], ie_data[1]]);
                    let group_cipher = [ie_data[2], ie_data[3], ie_data[4], ie_data[5]];
                    let pairwise_count = u16::from_le_bytes([ie_data[6], ie_data[7]]);
                    *rsn_info = Some(RsnInfo {
                        version,
                        group_cipher,
                        pairwise_count,
                    });
                }
            }
            // Extended Supported Rates (tag 50)
            50 => {
                *extended_rates = ie_data.to_vec();
            }
            // Vendor Specific (tag 221 / 0xDD)
            221 => {
                if ie_data.len() >= 4 {
                    let oui = [ie_data[0], ie_data[1], ie_data[2]];
                    let oui_type = ie_data[3];
                    vendor_ies.push(VendorIE {
                        oui,
                        oui_type,
                        data: ie_data[4..].to_vec(),
                    });
                }
            }
            _ => {}
        }

        pos += len;
    }
}

/// Align offset to boundary
fn align(offset: usize, alignment: usize) -> usize {
    (offset + alignment - 1) & !(alignment - 1)
}

/// Convert WiFi frequency to channel number
fn freq_to_channel(freq: u16) -> u16 {
    match freq {
        2412 => 1,
        2417 => 2,
        2422 => 3,
        2427 => 4,
        2432 => 5,
        2437 => 6,
        2442 => 7,
        2447 => 8,
        2452 => 9,
        2457 => 10,
        2462 => 11,
        2467 => 12,
        2472 => 13,
        2484 => 14,
        // 5 GHz
        f if (5170..=5330).contains(&f) => (f - 5000) / 5,
        f if (5490..=5730).contains(&f) => (f - 5000) / 5,
        f if (5735..=5835).contains(&f) => (f - 5000) / 5,
        // 6 GHz
        f if (5955..=7115).contains(&f) => (f - 5950) / 5,
        _ => 0,
    }
}

/// Format MAC address as string.
/// Uses a stack buffer to avoid heap allocation — MAC is always exactly 17 bytes.
pub fn format_mac(mac: &[u8; 6]) -> String {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    let mut buf = [0u8; 17];
    for i in 0..6 {
        let offset = i * 3;
        buf[offset] = HEX[(mac[i] >> 4) as usize];
        buf[offset + 1] = HEX[(mac[i] & 0x0F) as usize];
        if i < 5 {
            buf[offset + 2] = b':';
        }
    }
    // SAFETY: buf contains only ASCII hex digits and colons
    unsafe { String::from_utf8_unchecked(buf.to_vec()) }
}

/// Check if a vendor IE is an OpenDroneID/ASTM RemoteID message
pub fn is_remoteid_ie(vie: &VendorIE) -> bool {
    vie.oui == ASTM_OUI && vie.oui_type == 0x0D
}

/// Check if a vendor IE is a DJI DroneID message (all known OUIs, any type)
/// Accept any oui_type — DJI may use values outside 0x10-0x14 in category 127
/// Action frames or newer firmware. The DJI decoder handles unknown types gracefully.
pub fn is_dji_droneid_ie(vie: &VendorIE) -> bool {
    vie.oui == DJI_OUI || vie.oui == DJI_OUI_ALT || vie.oui == DJI_OUI_OCUSYNC
}

/// Check if a vendor IE is from a Parrot drone
pub fn is_parrot_ie(vie: &VendorIE) -> bool {
    vie.oui == PARROT_OUI || vie.oui == PARROT_OUI_ALT
}

/// Check if a vendor IE is from an Autel drone
pub fn is_autel_ie(vie: &VendorIE) -> bool {
    vie.oui == AUTEL_OUI || vie.oui == AUTEL_OUI_ALT || vie.oui == AUTEL_OUI_ALT2
}

/// Check if MAC address belongs to a known drone manufacturer.
/// Rejects locally-administered (randomized) MACs — these are used by phones/laptops
/// for privacy and can collide with vendor IE OUIs (e.g. 26:37:12 = DJI vendor IE OUI).
pub fn get_drone_manufacturer_from_oui(mac: &[u8; 6]) -> Option<&'static str> {
    // Locally-administered bit (bit 1 of first byte) means randomized MAC — skip OUI lookup
    if mac[0] & 0x02 != 0 {
        return None;
    }
    let oui = [mac[0], mac[1], mac[2]];
    match oui {
        // DJI OUIs (globally-assigned only — vendor IE OUIs 26:37:12/26:6F:48 excluded
        // because they have the LAA bit set and are filtered above)
        [0x60, 0x60, 0x1F] => Some("DJI"),  // DJI OcuSync
        [0x34, 0xD2, 0x62] => Some("DJI"),
        [0x48, 0x1C, 0xB9] => Some("DJI"),
        // 48:43:5A is Huawei, NOT DJI — removed to prevent false positives
        [0x04, 0xA8, 0x5A] => Some("DJI"),
        [0x0C, 0x9A, 0xE6] => Some("DJI"),
        [0x4C, 0x43, 0xF6] => Some("DJI"),
        [0x58, 0xB8, 0x58] => Some("DJI"),
        // 62:60:1F has LAA bit set — dead code, filtered by LAA check above
        [0x88, 0x29, 0x85] => Some("DJI"),
        [0x8C, 0x1E, 0xD9] => Some("DJI"),  // Phantom 4 Pro V2 (field-verified)
        [0x8C, 0x58, 0x23] => Some("DJI"),
        [0x9C, 0x5A, 0x8A] => Some("DJI"),
        [0xE4, 0x7A, 0x2C] => Some("DJI"),
        // C8:F0:9E, 20:D5:AB, 40:1C:83 REMOVED — unverified, cause false positives
        // C8:F0:9E = Espressif (ESP32), not DJI. Triggered FP on "SentryPlus" IoT devices.

        // Parrot OUIs
        x if x == PARROT_OUI || x == PARROT_OUI_ALT => Some("Parrot"),
        // 98:E7:43 is Dell, NOT Parrot — removed to prevent false positives
        [0x00, 0x12, 0x1C] => Some("Parrot"), // Older Parrot
        [0x00, 0x26, 0x7E] => Some("Parrot"), // Parrot AR.Drone
        [0x90, 0x3A, 0xE6] => Some("Parrot"),

        // Autel OUIs
        x if x == AUTEL_OUI || x == AUTEL_OUI_ALT || x == AUTEL_OUI_ALT2 => Some("Autel"),

        // Skydio OUIs
        [0x38, 0x1D, 0x14] => Some("Skydio"),

        // Yuneec OUIs
        [0xE0, 0xB6, 0xF5] => Some("Yuneec"),

        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Test OUI detection
    #[test]
    fn test_dji_oui_detection() {
        // DJI OcuSync OUI (globally administered)
        let mac = [0x60, 0x60, 0x1F, 0x12, 0x34, 0x56];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("DJI"));

        // Another DJI OUI (globally administered)
        let mac = [0x34, 0xD2, 0x62, 0xAB, 0xCD, 0xEF];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("DJI"));

        // DJI OUI (globally administered)
        let mac = [0x88, 0x29, 0x85, 0x00, 0x00, 0x00];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("DJI"));
    }

    #[test]
    fn test_randomized_mac_rejected() {
        // DJI vendor IE OUI 26:37:12 used as MAC — locally administered bit set
        // This is a randomized MAC from a phone, NOT a real DJI device
        let mac = [0x26, 0x37, 0x12, 0x2D, 0xB5, 0x7E];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), None);

        // DJI vendor IE OUI 26:6F:48 — also LAA
        let mac = [0x26, 0x6F, 0x48, 0x11, 0x22, 0x33];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), None);

        // Any randomized MAC should be rejected
        let mac = [0xFE, 0x12, 0x34, 0x56, 0x78, 0x9A];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), None);
    }

    #[test]
    fn test_parrot_oui_detection() {
        let mac = [0x90, 0x03, 0xB7, 0x12, 0x34, 0x56];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("Parrot"));

        let mac = [0xA0, 0x14, 0x3D, 0x00, 0x00, 0x00];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("Parrot"));
    }

    #[test]
    fn test_autel_oui_detection() {
        // Autel newer models OUI
        let mac = [0x70, 0x88, 0x6B, 0x12, 0x34, 0x56];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("Autel"));

        // Autel Robotics
        let mac = [0xEC, 0x5B, 0xCD, 0x00, 0x00, 0x00];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("Autel"));
    }

    #[test]
    fn test_skydio_oui_detection() {
        let mac = [0x38, 0x1D, 0x14, 0x12, 0x34, 0x56];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("Skydio"));
    }

    #[test]
    fn test_yuneec_oui_detection() {
        let mac = [0xE0, 0xB6, 0xF5, 0x12, 0x34, 0x56];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), Some("Yuneec"));
    }

    #[test]
    fn test_unknown_oui() {
        let mac = [0xFF, 0xFF, 0xFF, 0x12, 0x34, 0x56];
        assert_eq!(get_drone_manufacturer_from_oui(&mac), None);
    }

    // Test vendor IE detection
    #[test]
    fn test_is_remoteid_ie() {
        let vie = VendorIE {
            oui: [0xFA, 0x0B, 0xBC], // ASTM OUI
            oui_type: 0x0D,
            data: vec![],
        };
        assert!(is_remoteid_ie(&vie));

        let vie = VendorIE {
            oui: [0xFA, 0x0B, 0xBC],
            oui_type: 0x00, // Wrong type
            data: vec![],
        };
        assert!(!is_remoteid_ie(&vie));
    }

    #[test]
    fn test_is_dji_droneid_ie() {
        // Primary DJI OUI with type 0x10
        let vie = VendorIE {
            oui: [0x26, 0x37, 0x12],
            oui_type: 0x10,
            data: vec![],
        };
        assert!(is_dji_droneid_ie(&vie));

        // Alt DJI OUI with type 0x11
        let vie = VendorIE {
            oui: [0x26, 0x6F, 0x48],
            oui_type: 0x11,
            data: vec![],
        };
        assert!(is_dji_droneid_ie(&vie));

        // OcuSync OUI with type 0x14
        let vie = VendorIE {
            oui: [0x60, 0x60, 0x1F],
            oui_type: 0x14,
            data: vec![],
        };
        assert!(is_dji_droneid_ie(&vie));

        // DJI OUI with any type now accepted (widened for category 127 Action frames)
        let vie = VendorIE {
            oui: [0x26, 0x37, 0x12],
            oui_type: 0x20,
            data: vec![],
        };
        assert!(is_dji_droneid_ie(&vie));

        // Non-DJI OUI should NOT match
        let vie = VendorIE {
            oui: [0x00, 0x50, 0xF2],
            oui_type: 0x10,
            data: vec![],
        };
        assert!(!is_dji_droneid_ie(&vie));
    }

    #[test]
    fn test_is_parrot_ie() {
        let vie = VendorIE {
            oui: [0x90, 0x03, 0xB7],
            oui_type: 0x00,
            data: vec![],
        };
        assert!(is_parrot_ie(&vie));

        let vie = VendorIE {
            oui: [0xA0, 0x14, 0x3D],
            oui_type: 0x00,
            data: vec![],
        };
        assert!(is_parrot_ie(&vie));
    }

    #[test]
    fn test_is_autel_ie() {
        let vie = VendorIE {
            oui: [0x70, 0x88, 0x6B],
            oui_type: 0x00,
            data: vec![],
        };
        assert!(is_autel_ie(&vie));

        let vie = VendorIE {
            oui: [0xEC, 0x5B, 0xCD],
            oui_type: 0x00,
            data: vec![],
        };
        assert!(is_autel_ie(&vie));
    }

    #[test]
    fn test_format_mac() {
        let mac = [0x60, 0x60, 0x1F, 0x12, 0x34, 0x56];
        assert_eq!(format_mac(&mac), "60:60:1F:12:34:56");

        let mac = [0x00, 0x00, 0x00, 0x00, 0x00, 0x00];
        assert_eq!(format_mac(&mac), "00:00:00:00:00:00");
    }

    #[test]
    fn test_freq_to_channel() {
        // 2.4 GHz channels
        assert_eq!(freq_to_channel(2412), 1);
        assert_eq!(freq_to_channel(2437), 6);
        assert_eq!(freq_to_channel(2462), 11);

        // 5 GHz channels
        assert_eq!(freq_to_channel(5180), 36);
        assert_eq!(freq_to_channel(5200), 40);
        assert_eq!(freq_to_channel(5745), 149);
        assert_eq!(freq_to_channel(5825), 165);

        // Unknown frequency
        assert_eq!(freq_to_channel(1234), 0);
    }

    // ============================================================
    // End-to-end frame parsing tests with realistic packet bytes
    // ============================================================

    /// Build a minimal radiotap header with RSSI and channel info
    fn build_radiotap(rssi: i8, freq: u16) -> Vec<u8> {
        // Radiotap: version(1) + pad(1) + length(2LE) + present(4LE)
        // Present bits: bit 3 (channel) + bit 5 (antenna signal)
        let present: u32 = (1 << 3) | (1 << 5);
        let mut rt = vec![
            0x00,                       // version
            0x00,                       // pad
            0x00, 0x00,                 // length (filled below)
            present.to_le_bytes()[0],
            present.to_le_bytes()[1],
            present.to_le_bytes()[2],
            present.to_le_bytes()[3],
            // Channel: freq(2LE) + flags(2LE) at offset 8 (naturally aligned to 2)
            freq.to_le_bytes()[0],
            freq.to_le_bytes()[1],
            0x00, 0x00,                 // channel flags
            // Antenna signal: 1 byte signed dBm
            rssi as u8,
        ];
        let len = rt.len() as u16;
        rt[2] = len.to_le_bytes()[0];
        rt[3] = len.to_le_bytes()[1];
        rt
    }

    /// Build an 802.11 management frame header
    fn build_80211_header(frame_type: u8, frame_subtype: u8, src: &[u8; 6], bssid: &[u8; 6]) -> Vec<u8> {
        let fc: u16 = ((frame_subtype as u16) << 4) | ((frame_type as u16) << 2);
        let mut hdr = vec![0u8; 24];
        hdr[0] = fc.to_le_bytes()[0];
        hdr[1] = fc.to_le_bytes()[1];
        // Duration
        hdr[2] = 0; hdr[3] = 0;
        // Addr1 (dst) = broadcast
        hdr[4..10].copy_from_slice(&[0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF]);
        // Addr2 (src)
        hdr[10..16].copy_from_slice(src);
        // Addr3 (bssid)
        hdr[16..22].copy_from_slice(bssid);
        // Seq ctrl
        hdr[22] = 0; hdr[23] = 0;
        hdr
    }

    /// Build beacon fixed parameters: timestamp(8) + interval(2) + capability(2)
    fn build_beacon_fixed(interval: u16) -> Vec<u8> {
        let mut fixed = vec![0u8; 12];
        fixed[8] = interval.to_le_bytes()[0];
        fixed[9] = interval.to_le_bytes()[1];
        fixed
    }

    /// Build a vendor-specific IE (tag 0xDD)
    fn build_vendor_ie(oui: &[u8; 3], oui_type: u8, data: &[u8]) -> Vec<u8> {
        let len = 4 + data.len(); // OUI(3) + type(1) + data
        let mut ie = vec![0xDD, len as u8];
        ie.extend_from_slice(oui);
        ie.push(oui_type);
        ie.extend_from_slice(data);
        ie
    }

    /// Build an SSID IE (tag 0)
    fn build_ssid_ie(ssid: &str) -> Vec<u8> {
        let mut ie = vec![0x00, ssid.len() as u8];
        ie.extend_from_slice(ssid.as_bytes());
        ie
    }

    /// Build a DJI flight reg payload (subcommand 0x10 body without the subcommand byte)
    fn build_dji_flight_reg(serial: &str, lat_deg: f64, lon_deg: f64, alt_m: i16) -> Vec<u8> {
        let mut payload = vec![0u8; 75];
        // Version (V1 = cleartext telemetry; V2+ encrypts coordinates)
        payload[0] = 0x01;
        // Seq number
        payload[1] = 0x01; payload[2] = 0x00;
        // State info: motor on + in air
        payload[3] = 0x07; payload[4] = 0x00;
        // Serial (16 bytes at offset 5)
        let serial_bytes = serial.as_bytes();
        let copy_len = serial_bytes.len().min(16);
        payload[5..5 + copy_len].copy_from_slice(&serial_bytes[..copy_len]);
        // Longitude (offset 21) — raw = degrees * 174533.0
        let lon_raw = (lon_deg * 174533.0) as i32;
        payload[21..25].copy_from_slice(&lon_raw.to_le_bytes());
        // Latitude (offset 25) — raw = degrees * 174533.0
        let lat_raw = (lat_deg * 174533.0) as i32;
        payload[25..29].copy_from_slice(&lat_raw.to_le_bytes());
        // Altitude (offset 29)
        payload[29..31].copy_from_slice(&alt_m.to_le_bytes());
        // Height AGL (offset 31)
        payload[31..33].copy_from_slice(&(alt_m - 5).to_le_bytes());
        // Product type (offset 53)
        payload[53] = 0x3C; // some DJI product type
        payload
    }

    // ---- Test 1: DJI beacon with OcuSync vendor IE (60:60:1F, type 0x13) ----
    #[test]
    fn test_parse_dji_ocusync_beacon_end_to_end() {
        let src_mac: [u8; 6] = [0x60, 0x60, 0x1F, 0xAA, 0xBB, 0xCC];

        // Build DJI OcuSync vendor IE: OUI(60:60:1F) + VendorType(0x13) + SubCmd(0x10) + Payload
        let flight_reg = build_dji_flight_reg("1ZNBJ1K00C0042", 40.713, -74.006, 50);
        let mut ocusync_data = vec![0x10]; // subcommand = flight reg
        ocusync_data.extend_from_slice(&flight_reg);

        // Build complete packet: radiotap + 802.11 beacon + fixed params + SSID IE + vendor IE
        let mut packet = build_radiotap(-65, 2437); // ch6
        packet.extend_from_slice(&build_80211_header(0, 8, &src_mac, &src_mac)); // type=0 subtype=8 = beacon
        packet.extend_from_slice(&build_beacon_fixed(100));
        packet.extend_from_slice(&build_ssid_ie("DJI-MINI3PRO-AABB"));
        packet.extend_from_slice(&build_vendor_ie(&[0x60, 0x60, 0x1F], 0x13, &ocusync_data));

        // Parse the complete packet
        let parsed = parse_packet(&packet).expect("Should parse DJI OcuSync beacon");

        // Verify frame type and MACs
        assert_eq!(parsed.frame_type, FrameType::Beacon);
        assert_eq!(parsed.src_mac, src_mac);
        assert_eq!(parsed.ssid, Some("DJI-MINI3PRO-AABB".to_string()));
        assert_eq!(parsed.beacon_interval, Some(100));
        assert_eq!(parsed.radiotap.rssi, -65);
        assert_eq!(parsed.radiotap.channel, 6);

        // Verify vendor IE was extracted
        assert!(!parsed.vendor_ies.is_empty(), "Should have vendor IEs");
        let dji_vie = parsed.vendor_ies.iter().find(|v| is_dji_droneid_ie(v));
        assert!(dji_vie.is_some(), "Should detect DJI DroneID vendor IE");

        let vie = dji_vie.unwrap();
        assert_eq!(vie.oui, [0x60, 0x60, 0x1F]);
        assert_eq!(vie.oui_type, 0x13);

        // Verify DJI decode produces correct data through the OcuSync path
        let dji = super::super::dji::decode_dji_droneid(&vie.data, vie.oui_type)
            .expect("Should decode DJI DroneID from OcuSync format");
        assert_eq!(dji.serial_number, Some("1ZNBJ1K00C0042".to_string()));
        assert!(dji.latitude.is_some(), "Should have latitude");
        assert!(dji.longitude.is_some(), "Should have longitude");
        let lat = dji.latitude.unwrap();
        let lon = dji.longitude.unwrap();
        assert!(lat > 40.0 && lat < 41.0, "Latitude {} should be ~40.71", lat);
        assert!(lon > -75.0 && lon < -73.0, "Longitude {} should be ~-74.01", lon);
        assert_eq!(dji.altitude_pressure, Some(50.0));
        assert_eq!(dji.product_type, Some(0x3C));
    }

    // ---- Test 2: DJI beacon with old-format vendor IE (26:37:12, type=subcmd) ----
    #[test]
    fn test_parse_dji_legacy_beacon_end_to_end() {
        let src_mac: [u8; 6] = [0x26, 0x37, 0x12, 0xDE, 0xAD, 0x01];
        let flight_reg = build_dji_flight_reg("3NEASL0000TEST", 40.712, -74.005, 100);

        // Legacy format: OUI(26:37:12) + SubCmd(0x10) + Payload (no vendor type wrapper)
        let mut packet = build_radiotap(-70, 5180); // ch36
        packet.extend_from_slice(&build_80211_header(0, 8, &src_mac, &src_mac));
        packet.extend_from_slice(&build_beacon_fixed(100));
        packet.extend_from_slice(&build_ssid_ie("DJI-MAVICPRO-DEAD"));
        packet.extend_from_slice(&build_vendor_ie(&[0x26, 0x37, 0x12], 0x10, &flight_reg));

        let parsed = parse_packet(&packet).expect("Should parse DJI legacy beacon");
        assert_eq!(parsed.frame_type, FrameType::Beacon);
        assert_eq!(parsed.ssid, Some("DJI-MAVICPRO-DEAD".to_string()));

        let dji_vie = parsed.vendor_ies.iter().find(|v| is_dji_droneid_ie(v));
        assert!(dji_vie.is_some(), "Should detect legacy DJI DroneID vendor IE");

        let vie = dji_vie.unwrap();
        assert_eq!(vie.oui_type, 0x10);

        // Decode with legacy format — oui_type IS the subcommand
        let dji = super::super::dji::decode_dji_droneid(&vie.data, vie.oui_type)
            .expect("Should decode DJI DroneID from legacy format");
        assert_eq!(dji.serial_number, Some("3NEASL0000TEST".to_string()));
        assert!(dji.latitude.is_some(), "Should have latitude");
        let lat = dji.latitude.unwrap();
        assert!(lat > 40.0 && lat < 41.0, "Latitude {} should be ~40.71", lat);
    }

    // ---- Test 3: NAN Action frame with RemoteID ----
    #[test]
    fn test_parse_nan_remoteid_action_frame_end_to_end() {
        let src_mac: [u8; 6] = [0x60, 0x60, 0x1F, 0x11, 0x22, 0x33];

        // Build OpenDroneID BasicID message (type 0x0, 25 bytes)
        let mut basic_id = vec![0u8; 25];
        basic_id[0] = 0x00; // msg_type=0 (BasicID), proto_version=0
        basic_id[1] = 0x41; // id_type=4 (serial), ua_type=1 (helicopter)
        let serial = b"1581F6N8A241BML3";
        basic_id[2..18].copy_from_slice(serial);

        // Build OpenDroneID Location message (type 0x1, 25 bytes)
        let mut location = vec![0u8; 25];
        location[0] = 0x10; // msg_type=1 (Location), proto_version=0
        // Byte 1: SpeedMult=0(bit0), EWDir=0(bit1), HeightType=0(bit2), Status=3(bits4-7)=airborne
        location[1] = 0x30; // status=3 (airborne) in bits 4-7
        location[2] = 45;   // direction = 45 degrees (full byte)
        location[3] = 40;   // speed = 40 * 0.25 = 10 m/s (full byte, mult=0)
        // Latitude: 40.713 * 1e7 = 407130000
        let lat: i32 = 407130000;
        location[5..9].copy_from_slice(&lat.to_le_bytes());
        // Longitude: -74.006 * 1e7 = -740060000
        let lon: i32 = -740060000;
        location[9..13].copy_from_slice(&lon.to_le_bytes());
        // Altitude pressure: (50 + 1000) / 0.5 = 2100
        let alt: u16 = 2100;
        location[13..15].copy_from_slice(&alt.to_le_bytes());

        // Per ASTM F3411-22a: header(1) + msg_size(1) + count(1) + messages
        let mut msg_pack = vec![0xF0, 25, 0x02]; // type=0xF, single_msg_size=25, count=2
        msg_pack.extend_from_slice(&basic_id);
        msg_pack.extend_from_slice(&location);

        // Build NAN Service Info per ASTM F3411-22a:
        //   OUI(3) + OUI_Type(1) + Message_Counter(1) + ODID payload
        let mut service_info = vec![];
        service_info.extend_from_slice(&ASTM_OUI); // FA:0B:BC
        service_info.push(0x0D);                    // ASTM RemoteID type
        service_info.push(0x5F);                    // Message Counter (NAN-specific)
        service_info.extend_from_slice(&msg_pack);

        // Build NAN Service Descriptor Attribute (0x03)
        // Service ID (6 bytes) + Instance(1) + Requestor(1) + Control(1) + Service Info
        let remoteid_service_id: [u8; 6] = [0x88, 0x69, 0x19, 0x9D, 0x92, 0x09];
        let service_control: u8 = (1 << 5); // bit 5 = Service Info Present
        let mut service_desc = vec![];
        service_desc.extend_from_slice(&remoteid_service_id);
        service_desc.push(0x01); // instance ID
        service_desc.push(0x00); // requestor instance ID
        service_desc.push(service_control);
        service_desc.push(service_info.len() as u8); // service info length
        service_desc.extend_from_slice(&service_info);

        // Build NAN TLV: attr_id(1) + length(2LE) + body
        let sd_len = service_desc.len() as u16;
        let mut nan_body = vec![NAN_ATTR_SERVICE_DESCRIPTOR];
        nan_body.extend_from_slice(&sd_len.to_le_bytes());
        nan_body.extend_from_slice(&service_desc);

        // Build 802.11 Action frame:
        // Header(24) + Category(1) + Action(1) + OUI(3) + Type(1) + NAN body
        let mut packet = build_radiotap(-55, 2437); // ch6
        packet.extend_from_slice(&build_80211_header(0, 13, &src_mac, &src_mac)); // type=0 subtype=13 = action
        // Action frame body: category + action + OUI + type
        packet.push(NAN_SDF_ACTION_CATEGORY); // 0x04 = Public Action
        packet.push(NAN_SDF_ACTION_CODE);     // 0x09 = Vendor Specific
        packet.extend_from_slice(&WFA_OUI);   // 50:6F:9A
        packet.push(WFA_NAN_TYPE);            // 0x13 = NAN
        packet.extend_from_slice(&nan_body);

        // Parse the complete NAN action frame
        let parsed = parse_packet(&packet).expect("Should parse NAN RemoteID action frame");

        assert_eq!(parsed.frame_type, FrameType::Action);
        assert_eq!(parsed.src_mac, src_mac);
        assert_eq!(parsed.radiotap.channel, 6);

        // Verify ASTM RemoteID vendor IE was extracted from NAN SDF
        let rid_vie = parsed.vendor_ies.iter().find(|v| is_remoteid_ie(v));
        assert!(rid_vie.is_some(), "Should extract RemoteID vendor IE from NAN action frame");

        let vie = rid_vie.unwrap();
        assert_eq!(vie.oui, ASTM_OUI);
        assert_eq!(vie.oui_type, 0x0D);

        // Verify RemoteID decode
        let rid = super::super::remoteid::decode_remoteid(&vie.data)
            .expect("Should decode RemoteID from NAN Service Info");
        assert_eq!(rid.uas_id, Some("1581F6N8A241BML3".to_string()));
        assert_eq!(rid.ua_type, 1); // helicopter
        assert_eq!(rid.id_type, 4); // serial
        assert!(rid.latitude.is_some(), "Should have latitude from Location msg");
        assert!(rid.longitude.is_some(), "Should have longitude from Location msg");
        let lat = rid.latitude.unwrap();
        let lon = rid.longitude.unwrap();
        assert!((lat - 40.713).abs() < 0.001, "Latitude {} should be ~40.713", lat);
        assert!((lon - (-74.006)).abs() < 0.001, "Longitude {} should be ~-74.006", lon);
    }

    // ---- Test 4: RemoteID in beacon vendor IE (ASTM OUI directly) ----
    #[test]
    fn test_parse_remoteid_beacon_vendor_ie() {
        let src_mac: [u8; 6] = [0x38, 0x1D, 0x14, 0xAA, 0xBB, 0xCC]; // Skydio OUI

        // Build a single BasicID message
        let mut basic_id = vec![0u8; 25];
        basic_id[0] = 0x00; // BasicID
        basic_id[1] = 0x41; // id_type=4 (serial), ua_type=1
        basic_id[2..18].copy_from_slice(b"SKYDIOX2E0000001");

        let mut packet = build_radiotap(-60, 5745); // ch149
        packet.extend_from_slice(&build_80211_header(0, 8, &src_mac, &src_mac));
        packet.extend_from_slice(&build_beacon_fixed(100));
        packet.extend_from_slice(&build_ssid_ie("Skydio-X2E"));
        packet.extend_from_slice(&build_vendor_ie(&ASTM_OUI, 0x0D, &basic_id));

        let parsed = parse_packet(&packet).expect("Should parse RemoteID beacon");
        assert_eq!(parsed.frame_type, FrameType::Beacon);

        let rid_vie = parsed.vendor_ies.iter().find(|v| is_remoteid_ie(v));
        assert!(rid_vie.is_some(), "Should detect ASTM RemoteID vendor IE in beacon");

        let rid = super::super::remoteid::decode_remoteid(&rid_vie.unwrap().data)
            .expect("Should decode RemoteID from beacon vendor IE");
        assert_eq!(rid.uas_id, Some("SKYDIOX2E0000001".to_string()));
    }

    // ---- Test 5: NAN with non-standard Service ID still parses ----
    #[test]
    fn test_nan_non_standard_service_id_still_parses() {
        let src_mac: [u8; 6] = [0x60, 0x60, 0x1F, 0x44, 0x55, 0x66];

        // Build a single BasicID
        let mut basic_id = vec![0u8; 25];
        basic_id[0] = 0x00;
        basic_id[1] = 0x41;
        basic_id[2..14].copy_from_slice(b"TEST_SERIAL!");

        // Service Info with ASTM OUI + message counter
        let mut service_info = vec![];
        service_info.extend_from_slice(&ASTM_OUI);
        service_info.push(0x0D);
        service_info.push(0x01); // Message Counter
        service_info.extend_from_slice(&basic_id);

        // Use a WRONG service ID — we still parse because we don't hard-filter
        let wrong_service_id: [u8; 6] = [0x00, 0x11, 0x22, 0x33, 0x44, 0x55];
        let service_control: u8 = (1 << 5);
        let mut service_desc = vec![];
        service_desc.extend_from_slice(&wrong_service_id);
        service_desc.push(0x01);
        service_desc.push(0x00);
        service_desc.push(service_control);
        service_desc.push(service_info.len() as u8);
        service_desc.extend_from_slice(&service_info);

        let sd_len = service_desc.len() as u16;
        let mut nan_body = vec![NAN_ATTR_SERVICE_DESCRIPTOR];
        nan_body.extend_from_slice(&sd_len.to_le_bytes());
        nan_body.extend_from_slice(&service_desc);

        let mut packet = build_radiotap(-50, 2437);
        packet.extend_from_slice(&build_80211_header(0, 13, &src_mac, &src_mac));
        packet.push(NAN_SDF_ACTION_CATEGORY);
        packet.push(NAN_SDF_ACTION_CODE);
        packet.extend_from_slice(&WFA_OUI);
        packet.push(WFA_NAN_TYPE);
        packet.extend_from_slice(&nan_body);

        let parsed = parse_packet(&packet).expect("Should parse NAN with non-standard service ID");
        let rid_vie = parsed.vendor_ies.iter().find(|v| is_remoteid_ie(v));
        assert!(rid_vie.is_some(), "Should still extract RemoteID even with non-standard service ID");
    }

    // ---- Test 6: DJI beacon with multiple vendor IEs ----
    #[test]
    fn test_parse_beacon_with_multiple_vendor_ies() {
        let src_mac: [u8; 6] = [0x60, 0x60, 0x1F, 0x77, 0x88, 0x99];
        let flight_reg = build_dji_flight_reg("1ZNBJ1K00CMULTI", 40.713, -74.006, 75);
        let mut ocusync_data = vec![0x10];
        ocusync_data.extend_from_slice(&flight_reg);

        let mut packet = build_radiotap(-72, 2437);
        packet.extend_from_slice(&build_80211_header(0, 8, &src_mac, &src_mac));
        packet.extend_from_slice(&build_beacon_fixed(100));
        packet.extend_from_slice(&build_ssid_ie("DJI-MINI2-7788"));
        // Add a random vendor IE first (non-DJI)
        packet.extend_from_slice(&build_vendor_ie(&[0x00, 0x50, 0xF2], 0x02, &[0x01, 0x01]));
        // Then the DJI OcuSync vendor IE
        packet.extend_from_slice(&build_vendor_ie(&[0x60, 0x60, 0x1F], 0x13, &ocusync_data));
        // And another random vendor IE
        packet.extend_from_slice(&build_vendor_ie(&[0x00, 0x50, 0xF2], 0x01, &[0x01, 0x00]));

        let parsed = parse_packet(&packet).expect("Should parse beacon with multiple vendor IEs");
        assert_eq!(parsed.vendor_ies.len(), 3, "Should extract all 3 vendor IEs");

        let dji_vie = parsed.vendor_ies.iter().find(|v| is_dji_droneid_ie(v));
        assert!(dji_vie.is_some(), "Should find DJI vendor IE among multiple IEs");

        let dji = super::super::dji::decode_dji_droneid(&dji_vie.unwrap().data, dji_vie.unwrap().oui_type)
            .expect("Should decode DJI from beacon with multiple vendor IEs");
        assert_eq!(dji.serial_number, Some("1ZNBJ1K00CMULTI".to_string()));
    }

    // ---- Test 7: DJI OcuSync with flight purpose (subcommand 0x11) ----
    #[test]
    fn test_parse_dji_ocusync_flight_purpose() {
        let src_mac: [u8; 6] = [0x60, 0x60, 0x1F, 0xDD, 0xEE, 0xFF];

        // Flight purpose payload
        let mut purpose_payload = vec![0u8; 30];
        purpose_payload[5..21].copy_from_slice(b"3NEASL999PURPOSE");

        // OcuSync format: [0x11(subcmd), purpose_payload...]
        let mut ocusync_data = vec![0x11]; // subcommand = flight purpose
        ocusync_data.extend_from_slice(&purpose_payload);

        let mut packet = build_radiotap(-68, 2437);
        packet.extend_from_slice(&build_80211_header(0, 8, &src_mac, &src_mac));
        packet.extend_from_slice(&build_beacon_fixed(100));
        packet.extend_from_slice(&build_ssid_ie("DJI-AVATA-DDEE"));
        packet.extend_from_slice(&build_vendor_ie(&[0x60, 0x60, 0x1F], 0x13, &ocusync_data));

        let parsed = parse_packet(&packet).expect("Should parse DJI flight purpose beacon");
        let dji_vie = parsed.vendor_ies.iter().find(|v| is_dji_droneid_ie(v));
        assert!(dji_vie.is_some());

        let vie = dji_vie.unwrap();
        let dji = super::super::dji::decode_dji_droneid(&vie.data, vie.oui_type)
            .expect("Should decode OcuSync flight purpose");
        assert_eq!(dji.serial_number, Some("3NEASL999PURPOSE".to_string()));
    }
}
