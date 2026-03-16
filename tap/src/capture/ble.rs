//! BLE scanner for OpenDroneID (ASTM F3411) advertisements.
//!
//! Second capture source alongside pcap. Scans for BLE
//! advertisements carrying service UUID 0xFFFA (OpenDroneID), strips
//! the BLE transport header, and feeds decoded detections into the
//! same det_tx channel.
//!
//! Uses continuous property-change events (duplicate_data: true) instead
//! of remove/re-add cycling. Every advertisement from a device fires a
//! PropertyChanged event, giving us continuous position tracking.
//!
//! BLE 4 legacy: Single 25-byte ODID message per advertisement.
//! Drones cycle through message types across successive adverts.
//! We accumulate per-MAC in a local merge map before emitting.
//!
//! BLE 5 extended: Full MessagePack (type 0xF) in one advert.
//! Decoded and emitted immediately.

use crate::decode::remoteid::{self, RemoteIdData};
use crate::proto;

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use futures::stream::SelectAll;
use futures::StreamExt;
use tokio::sync::mpsc;
use tracing::{debug, error, info, warn};

/// Stats exposed for heartbeat and stats logger
pub struct BleStats {
    pub advertisements: AtomicU64,
    pub detections: AtomicU64,
    pub scanning: AtomicBool,
    /// Total BLE devices seen (all devices, not just ODID)
    pub devices_seen: AtomicU64,
    /// Detections dropped because det_tx channel was full
    pub channel_drops: AtomicU64,
    /// Total ServiceData property change events received (any UUID)
    pub svc_data_events: AtomicU64,
    /// Total ManufacturerData property change events received (any company ID)
    pub mfr_data_events: AtomicU64,
}

impl Default for BleStats {
    fn default() -> Self {
        Self {
            advertisements: AtomicU64::new(0),
            detections: AtomicU64::new(0),
            scanning: AtomicBool::new(false),
            devices_seen: AtomicU64::new(0),
            channel_drops: AtomicU64::new(0),
            svc_data_events: AtomicU64::new(0),
            mfr_data_events: AtomicU64::new(0),
        }
    }
}

/// OpenDroneID BLE service UUID (ASTM F3411)
/// Full 128-bit Bluetooth Base UUID: 0000FFFA-0000-1000-8000-00805F9B34FB
const ODID_UUID: uuid::Uuid =
    uuid::Uuid::from_u128(0x0000FFFA_0000_1000_8000_00805F9B34FB);

/// OpenDroneID Application Code in BLE service data
const ODID_APP_CODE: u8 = 0x0D;

/// OpenDroneID Bluetooth company ID for ManufacturerData advertisements.
/// Some drones advertise ODID via ManufacturerData instead of (or alongside)
/// ServiceData. The company ID is 0xFFFA (same as the UUID short form).
const ODID_COMPANY_ID: u16 = 0xFFFA;

/// BLE4 accumulation window — emit after this even if incomplete.
/// 3s captures ~10 adverts at 3-4Hz = all message types in a cycle.
const BLE4_ACCUMULATION_SECS: u64 = 3;

/// Maximum restart backoff
const MAX_BACKOFF_SECS: u64 = 30;

/// Cleanup interval for stale BLE4 accumulation entries and dead streams
const CLEANUP_INTERVAL_SECS: u64 = 30;

/// BLE5 dedup window — suppress duplicate BLE5 detections from the same MAC
/// within this duration. MessagePacks are complete per advert; 1Hz position updates.
const BLE5_DEDUP_SECS: u64 = 1;

/// Check if a BLE adapter is available on this system
pub fn is_ble_available(adapter: &str) -> bool {
    std::path::Path::new(&format!("/sys/class/bluetooth/{}", adapter)).exists()
}

/// BLE4 per-device accumulation state
struct Ble4State {
    first_seen: Instant,
    last_update: Instant,
    remote_id: RemoteIdData,
    rssi: i32,
    raw_payload: Vec<u8>,
    emitted: bool,
}

/// Check if BLE 5 Coded PHY (Long Range) is enabled via btmgmt.
/// MT7921U enables all PHYs by default (LECODEDTX/LECODEDRX in Selected phys).
/// We just verify and log — no need to set, which avoids btmgmt hanging when
/// BlueZ already holds the adapter lock.
async fn check_coded_phy(adapter: &str) {
    let idx = adapter.trim_start_matches("hci");
    let child = tokio::process::Command::new("btmgmt")
        .args(["--index", idx, "phy"])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn();

    let mut child = match child {
        Ok(c) => c,
        Err(e) => {
            debug!(adapter, error = %e, "btmgmt not available, cannot check Coded PHY");
            return;
        }
    };

    match tokio::time::timeout(Duration::from_secs(3), child.wait()).await {
        Ok(Ok(status)) if status.success() => {
            // Read stdout after process exits
            if let Some(mut stdout) = child.stdout.take() {
                let mut buf = String::new();
                if tokio::io::AsyncReadExt::read_to_string(&mut stdout, &mut buf).await.is_ok() {
                    let has_coded = buf.contains("LECODEDTX") && buf.contains("LECODEDRX");
                    if has_coded {
                        info!(adapter, "BLE 5 Coded PHY (Long Range) confirmed active");
                    } else {
                        debug!(adapter, "btmgmt output missing Coded PHY flags (may need root)");
                    }
                }
            }
        }
        Ok(Ok(_)) => {
            // btmgmt needs root — Permission Denied is normal for non-root services.
            // MT7921U enables all PHYs by default, so Coded PHY is active regardless.
            debug!(adapter, "btmgmt phy query needs root — assuming Coded PHY enabled (MT7921U default)");
        }
        Ok(Err(e)) => {
            debug!(adapter, error = %e, "btmgmt phy query failed");
        }
        Err(_) => {
            // btmgmt hangs when BlueZ holds adapter — kill and assume PHYs are default-enabled
            debug!(adapter, "btmgmt phy query timed out (adapter busy), assuming Coded PHY enabled");
            let _ = child.kill().await;
        }
    }
}

/// Switch BLE scanning from Active to Passive via raw HCI commands.
///
/// BlueZ forces Active scanning during discovery (sends SCAN_REQ after each
/// advertisement). For ODID detection we only need the advertisement data,
/// not scan responses. Passive scanning eliminates TX overhead, giving the
/// radio more time to listen for weak signals at long range.
///
/// Sequence: stop scan → set passive params → restart scan.
/// Uses LE Set Extended Scan Parameters (0x0041) + Enable (0x0042).
async fn force_passive_scan(adapter: &str) {
    let idx = adapter.trim_start_matches("hci");

    // Step 1: Stop scanning
    let stop = tokio::process::Command::new("hcitool")
        .args(["-i", adapter, "cmd", "0x08", "0x0042",
               "0x00", "0x00", "0x00", "0x00", "0x00", "0x00"])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status().await;

    if !matches!(stop, Ok(s) if s.success()) {
        warn!(adapter, "Failed to stop BLE scan for passive switch");
        return;
    }

    // Step 2: Set passive scan parameters
    // OwnAddrType=Random(0x01), FilterPolicy=AcceptAll(0x00), PHYs=0x05(1M+Coded)
    // LE 1M: Type=Passive(0x00), Interval=0x0012(11.25ms), Window=0x0012(11.25ms)
    // LE Coded: Type=Passive(0x00), Interval=0x0036(33.75ms), Window=0x0036(33.75ms)
    let params = tokio::process::Command::new("hcitool")
        .args(["-i", adapter, "cmd", "0x08", "0x0041",
               "0x01", "0x00", "0x05",
               "0x00", "0x12", "0x00", "0x12", "0x00",  // 1M passive
               "0x00", "0x36", "0x00", "0x36", "0x00"])  // Coded passive
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status().await;

    if !matches!(params, Ok(s) if s.success()) {
        warn!(adapter, "Failed to set passive scan parameters");
        return;
    }

    // Step 3: Re-enable scanning (filter_duplicates=disabled)
    let enable = tokio::process::Command::new("hcitool")
        .args(["-i", adapter, "cmd", "0x08", "0x0042",
               "0x01", "0x00", "0x00", "0x00", "0x00", "0x00"])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status().await;

    if matches!(enable, Ok(s) if s.success()) {
        info!(adapter, "BLE switched to PASSIVE scanning (no SCAN_REQ TX overhead)");
    } else {
        warn!(adapter, "Failed to re-enable BLE scan after passive switch");
    }
}

/// Main BLE scan loop — runs as a tokio async task.
/// Wraps run_scanner with restart-on-error and exponential backoff.
pub async fn ble_scan_loop(
    adapter_name: String,
    stop: Arc<AtomicBool>,
    det_tx: mpsc::Sender<proto::Detection>,
    tap_id: String,
    stats: Arc<BleStats>,
    mac_denylist: Arc<HashSet<String>>,
) {
    let mut backoff_secs = 1u64;

    loop {
        if !stop.load(Ordering::SeqCst) {
            break;
        }

        let start = Instant::now();
        match run_scanner(&adapter_name, &stop, &det_tx, &tap_id, &stats, &mac_denylist).await {
            Ok(()) => {
                // Clean exit (stop flag set)
                break;
            }
            Err(e) => {
                stats.scanning.store(false, Ordering::Relaxed);
                error!(error = %e, backoff_secs, "BLE scanner error, restarting");

                // Reset backoff if scanner ran for a while (transient issue)
                if start.elapsed() > Duration::from_secs(60) {
                    backoff_secs = 1;
                }

                // Sleep with stop-flag checks
                let deadline = Instant::now() + Duration::from_secs(backoff_secs);
                while Instant::now() < deadline {
                    if !stop.load(Ordering::SeqCst) {
                        return;
                    }
                    tokio::time::sleep(Duration::from_millis(500)).await;
                }

                backoff_secs = (backoff_secs * 2).min(MAX_BACKOFF_SECS);
            }
        }
    }

    stats.scanning.store(false, Ordering::Relaxed);
}

/// Inner scanner — connects to BlueZ, starts discovery, processes events.
/// Uses continuous property-change events for every advertisement.
async fn run_scanner(
    adapter_name: &str,
    stop: &Arc<AtomicBool>,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<BleStats>,
    mac_denylist: &HashSet<String>,
) -> anyhow::Result<()> {
    let session = bluer::Session::new().await?;
    let adapter = session.adapter(adapter_name)?;
    adapter.set_powered(true).await?;
    info!(adapter = adapter_name, "BLE adapter powered on");

    // Verify BLE 5 Coded PHY (Long Range) is active (MT7921U enables by default)
    check_coded_phy(adapter_name).await;

    // LE-only, duplicate_data for continuous advertisements, rssi=-127 to
    // disable BlueZ's default RSSI delta-threshold (which suppresses weak signals)
    adapter
        .set_discovery_filter(bluer::DiscoveryFilter {
            transport: bluer::DiscoveryTransport::Le,
            duplicate_data: true,
            rssi: Some(-127),
            ..Default::default()
        })
        .await?;

    stats.scanning.store(true, Ordering::Relaxed);

    // Register an Advertisement Monitor for ODID service UUID 0xFFFA.
    // This triggers BlueZ to use PASSIVE scanning (no SCAN_REQ TX overhead),
    // which gives us more listening time and better sensitivity for weak signals.
    // Pattern matches 16-bit service UUID 0xFFFA in "Incomplete List of 16-bit Service Class UUIDs"
    // AD type 0x02 or "Complete List" AD type 0x03, at offset 0, value 0xFAFF (little-endian).
    let _monitor_handle = match adapter.monitor().await {
        Ok(monitor_mgr) => {
            // Match ODID service data UUID 0xFFFA (AD type 0x16 = Service Data - 16-bit UUID)
            // The first 2 bytes of Service Data are the UUID in little-endian: 0xFA, 0xFF
            let odid_pattern = bluer::monitor::Pattern::new(
                0x16, // Service Data - 16-bit UUID
                0,    // Start at beginning of AD data
                &[0xFA, 0xFF], // ODID UUID 0xFFFA in little-endian
            );
            // Also match ManufacturerData with company ID 0xFFFA
            let mfr_pattern = bluer::monitor::Pattern::new(
                0xFF, // Manufacturer Specific Data
                0,    // Start at beginning of AD data
                &[0xFA, 0xFF], // Company ID 0xFFFA in little-endian
            );
            let monitor = bluer::monitor::Monitor {
                monitor_type: bluer::monitor::Type::OrPatterns,
                rssi_low_threshold: Some(-127),
                rssi_high_threshold: Some(-127),
                rssi_low_timeout: Some(Duration::from_secs(5)),
                rssi_high_timeout: Some(Duration::from_secs(1)),
                rssi_sampling_period: Some(bluer::monitor::RssiSamplingPeriod::All),
                patterns: Some(vec![odid_pattern, mfr_pattern]),
                ..Default::default()
            };
            match monitor_mgr.register(monitor).await {
                Ok(handle) => {
                    info!("ODID Advertisement Monitor registered — passive scanning enabled");
                    Some((monitor_mgr, handle))
                }
                Err(e) => {
                    warn!(error = %e, "Failed to register ODID monitor, falling back to active scanning");
                    None
                }
            }
        }
        Err(e) => {
            warn!(error = %e, "Advertisement Monitor API not available, using active scanning only");
            None
        }
    };

    info!("BLE discovery started with continuous tracking (duplicate_data=true, coded_phy=enabled)");

    let device_events = adapter.discover_devices().await?;
    futures::pin_mut!(device_events);

    // Override BlueZ's Active scanning with Passive via raw HCI.
    // Must happen AFTER discover_devices() starts the scan.
    // Small sleep to let BlueZ finish its scan parameter setup.
    tokio::time::sleep(Duration::from_millis(500)).await;
    force_passive_scan(adapter_name).await;

    // Per-device property change streams, merged into one
    let mut change_streams: SelectAll<
        std::pin::Pin<Box<dyn futures::Stream<Item = (bluer::Address, bluer::DeviceEvent)> + Send>>,
    > = SelectAll::new();

    // BLE4 message accumulation state per MAC
    let mut ble4_accum: HashMap<String, Ble4State> = HashMap::new();
    // BLE5 dedup: track last emission time per MAC to suppress 4-5 Hz advertisement flood
    let mut ble5_last_emit: HashMap<String, Instant> = HashMap::new();
    let mut tick = tokio::time::interval(Duration::from_secs(1));
    let mut last_cleanup = Instant::now();

    loop {
        tokio::select! {
            // Adapter-level events: new/removed devices
            event = device_events.next() => {
                match event {
                    Some(bluer::AdapterEvent::DeviceAdded(addr)) => {
                        stats.devices_seen.fetch_add(1, Ordering::Relaxed);

                        // Process initial service data
                        if let Ok(device) = adapter.device(addr) {
                            process_device_data(
                                &device, addr, det_tx, tap_id, stats, &mut ble4_accum,
                                &mut ble5_last_emit, mac_denylist,
                            ).await;

                            // Subscribe to ongoing property changes from this device
                            match device.events().await {
                                Ok(events) => {
                                    let stream = events.map(move |evt| (addr, evt));
                                    change_streams.push(Box::pin(stream));
                                }
                                Err(e) => {
                                    debug!(error = %e, addr = %addr, "Failed to subscribe to device events");
                                }
                            }
                        }
                    }
                    Some(bluer::AdapterEvent::DeviceRemoved(_)) => {
                        // Stream for this device will end naturally in SelectAll
                    }
                    Some(_) => {}
                    None => {
                        warn!("BLE discovery stream ended unexpectedly");
                        return Err(anyhow::anyhow!("Discovery stream ended"));
                    }
                }
            }

            // Per-device property change events (continuous advertisements)
            Some((addr, bluer::DeviceEvent::PropertyChanged(prop))) = change_streams.next() => {
                match prop {
                    bluer::DeviceProperty::ServiceData(sd) => {
                        stats.svc_data_events.fetch_add(1, Ordering::Relaxed);
                        // Check for ODID in updated service data
                        process_service_data(
                            addr, &sd, det_tx, tap_id, stats, &mut ble4_accum,
                            &mut ble5_last_emit, mac_denylist,
                        ).await;
                    }
                    bluer::DeviceProperty::ManufacturerData(md) => {
                        stats.mfr_data_events.fetch_add(1, Ordering::Relaxed);
                        // Check for ODID in ManufacturerData (company ID 0xFFFA)
                        process_manufacturer_data(
                            addr, &md, det_tx, tap_id, stats, &mut ble4_accum,
                            &mut ble5_last_emit, mac_denylist,
                        ).await;
                    }
                    bluer::DeviceProperty::Rssi(rssi) => {
                        // Update RSSI for BLE4 accumulation state
                        let mac = addr.to_string().to_uppercase();
                        if let Some(state) = ble4_accum.get_mut(&mac) {
                            state.rssi = rssi as i32;
                        }
                    }
                    _ => {}
                }
            }

            _ = tick.tick() => {
                if !stop.load(Ordering::SeqCst) {
                    info!("BLE scanner stopping");
                    return Ok(());
                }

                // Flush mature BLE4 accumulations (window expired)
                flush_ble4(&mut ble4_accum, det_tx, tap_id, stats).await;

                // Periodic cleanup of stale entries
                if last_cleanup.elapsed() > Duration::from_secs(CLEANUP_INTERVAL_SECS) {
                    let cutoff = Duration::from_secs(BLE4_ACCUMULATION_SECS * 3);
                    ble4_accum.retain(|_, state| state.last_update.elapsed() < cutoff);
                    let dedup_cutoff = Duration::from_secs(BLE5_DEDUP_SECS * 3);
                    ble5_last_emit.retain(|_, ts| ts.elapsed() < dedup_cutoff);
                    last_cleanup = Instant::now();
                }
            }
        }
    }
}

/// Process service data from a BLE device (initial discovery or property change).
/// Extracts ODID payload, decodes RemoteID, builds and sends detections.
fn process_odid_data(
    addr: bluer::Address,
    odid_data: &[u8],
    rssi: i32,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<BleStats>,
    ble4_accum: &mut HashMap<String, Ble4State>,
    ble5_last_emit: &mut HashMap<String, Instant>,
    mac_denylist: &HashSet<String>,
) {
    stats.advertisements.fetch_add(1, Ordering::Relaxed);

    // Minimum: AppCode(1) + Counter(1) + at least 1 byte ODID
    if odid_data.len() < 3 {
        return;
    }

    // Validate application code
    if odid_data[0] != ODID_APP_CODE {
        debug!(
            app_code = format!("0x{:02X}", odid_data[0]),
            "Unexpected ODID application code"
        );
        return;
    }

    // Strip BLE transport header: AppCode(1) + Counter(1)
    let odid_payload = &odid_data[2..];
    if odid_payload.is_empty() {
        return;
    }

    // Determine BLE version from ODID payload
    let is_message_pack = (odid_payload[0] >> 4) == 0xF;
    let mac = addr.to_string().to_uppercase();

    // MAC denylist check — same as pcap path
    if mac_denylist.contains(&mac) {
        return;
    }

    if is_message_pack {
        // BLE 5 extended: full MessagePack, decode and emit immediately
        // Rate-limit: suppress duplicate emissions within BLE5_DEDUP_SECS
        if let Some(last) = ble5_last_emit.get(&mac) {
            if last.elapsed() < Duration::from_secs(BLE5_DEDUP_SECS) {
                return; // Dedup — suppress 4-5 Hz advertisement flood
            }
        }

        if let Some(remote_id) = remoteid::decode_remoteid(odid_payload) {
            info!(
                mac = %mac,
                serial = ?remote_id.uas_id,
                rssi,
                "BLE5 OpenDroneID MessagePack"
            );
            let det = build_detection(
                &mac,
                rssi,
                tap_id,
                &remote_id,
                odid_payload,
                proto::DetectionSource::SourceBluetooth5,
            );
            match det_tx.try_send(det) {
                Ok(()) => {
                    stats.detections.fetch_add(1, Ordering::Relaxed);
                    ble5_last_emit.insert(mac, Instant::now());
                }
                Err(_) => {
                    stats.channel_drops.fetch_add(1, Ordering::Relaxed);
                    warn!("BLE: det_tx channel full, dropping BLE5 detection");
                }
            }
        }
    } else {
        // BLE 4 legacy: single message, accumulate across advertisements
        let state = ble4_accum.entry(mac.clone()).or_insert_with(|| Ble4State {
            first_seen: Instant::now(),
            last_update: Instant::now(),
            remote_id: RemoteIdData::default(),
            rssi,
            raw_payload: Vec::new(),
            emitted: false,
        });

        state.last_update = Instant::now();
        state.rssi = rssi;
        state.raw_payload = odid_payload.to_vec();

        // Decode single message and merge into accumulated state
        if let Some(new_data) = remoteid::decode_remoteid(odid_payload) {
            merge_remote_id(&mut state.remote_id, &new_data);
        }

        // For BLE4, allow re-emission on location updates (continuous tracking)
        if state.remote_id.uas_id.is_some() && state.remote_id.latitude.is_some() {
            // Re-emit every accumulation window for position updates
            if !state.emitted || state.first_seen.elapsed() > Duration::from_secs(BLE4_ACCUMULATION_SECS) {
                info!(
                    mac = %mac,
                    serial = ?state.remote_id.uas_id,
                    rssi,
                    "BLE4 OpenDroneID"
                );
                let det = build_detection(
                    &mac,
                    state.rssi,
                    tap_id,
                    &state.remote_id,
                    &state.raw_payload,
                    proto::DetectionSource::SourceBluetooth4,
                );
                match det_tx.try_send(det) {
                    Ok(()) => {
                        stats.detections.fetch_add(1, Ordering::Relaxed);
                    }
                    Err(_) => {
                        stats.channel_drops.fetch_add(1, Ordering::Relaxed);
                        warn!("BLE: det_tx channel full, dropping BLE4 detection");
                    }
                }
                state.emitted = true;
                // Reset window for next update cycle
                state.first_seen = Instant::now();
            }
        }
    }
}

/// Process a device on initial discovery — read properties and check for ODID.
/// Checks both ServiceData (UUID 0xFFFA) and ManufacturerData (company ID 0xFFFA).
async fn process_device_data(
    device: &bluer::Device,
    addr: bluer::Address,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<BleStats>,
    ble4_accum: &mut HashMap<String, Ble4State>,
    ble5_last_emit: &mut HashMap<String, Instant>,
    mac_denylist: &HashSet<String>,
) {
    let rssi = device.rssi().await.unwrap_or(None).unwrap_or(-100) as i32;

    // Try ServiceData first (primary ODID transport)
    if let Ok(Some(sd)) = device.service_data().await {
        if let Some(odid_data) = sd.get(&ODID_UUID) {
            process_odid_data(addr, odid_data, rssi, det_tx, tap_id, stats, ble4_accum, ble5_last_emit, mac_denylist);
            return;
        }
    }

    // Fallback: ManufacturerData with company ID 0xFFFA
    if let Ok(Some(md)) = device.manufacturer_data().await {
        if let Some(odid_data) = md.get(&ODID_COMPANY_ID) {
            process_odid_data(addr, odid_data, rssi, det_tx, tap_id, stats, ble4_accum, ble5_last_emit, mac_denylist);
        }
    }
}

/// Process updated service data from a property change event
async fn process_service_data(
    addr: bluer::Address,
    sd: &HashMap<uuid::Uuid, Vec<u8>>,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<BleStats>,
    ble4_accum: &mut HashMap<String, Ble4State>,
    ble5_last_emit: &mut HashMap<String, Instant>,
    mac_denylist: &HashSet<String>,
) {
    let odid_data = match sd.get(&ODID_UUID) {
        Some(data) => data,
        None => return,
    };

    // We don't have RSSI from service data change — use cached value or default
    let mac = addr.to_string().to_uppercase();
    let rssi = ble4_accum.get(&mac).map(|s| s.rssi).unwrap_or(-100);
    process_odid_data(addr, odid_data, rssi, det_tx, tap_id, stats, ble4_accum, ble5_last_emit, mac_denylist);
}

/// Process ManufacturerData from a property change event.
/// Some drones use ManufacturerData (company ID 0xFFFA) instead of ServiceData.
async fn process_manufacturer_data(
    addr: bluer::Address,
    md: &HashMap<u16, Vec<u8>>,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<BleStats>,
    ble4_accum: &mut HashMap<String, Ble4State>,
    ble5_last_emit: &mut HashMap<String, Instant>,
    mac_denylist: &HashSet<String>,
) {
    let odid_data = match md.get(&ODID_COMPANY_ID) {
        Some(data) => data,
        None => return,
    };

    let mac = addr.to_string().to_uppercase();
    let rssi = ble4_accum.get(&mac).map(|s| s.rssi).unwrap_or(-100);
    process_odid_data(addr, odid_data, rssi, det_tx, tap_id, stats, ble4_accum, ble5_last_emit, mac_denylist);
}

/// Flush BLE4 accumulation entries whose window has expired
async fn flush_ble4(
    ble4_accum: &mut HashMap<String, Ble4State>,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<BleStats>,
) {
    let window = Duration::from_secs(BLE4_ACCUMULATION_SECS);
    let now = Instant::now();

    let expired: Vec<String> = ble4_accum
        .iter()
        .filter(|(_, state)| !state.emitted && now.duration_since(state.first_seen) >= window)
        .map(|(mac, _)| mac.clone())
        .collect();

    for mac in expired {
        if let Some(state) = ble4_accum.get_mut(&mac) {
            // Only emit if we decoded something useful
            if state.remote_id.uas_id.is_some() || state.remote_id.latitude.is_some() {
                info!(
                    mac = %mac,
                    serial = ?state.remote_id.uas_id,
                    rssi = state.rssi,
                    "BLE4 OpenDroneID (window expired)"
                );
                let det = build_detection(
                    &mac,
                    state.rssi,
                    tap_id,
                    &state.remote_id,
                    &state.raw_payload,
                    proto::DetectionSource::SourceBluetooth4,
                );
                match det_tx.try_send(det) {
                    Ok(()) => {
                        stats.detections.fetch_add(1, Ordering::Relaxed);
                    }
                    Err(_) => {
                        stats.channel_drops.fetch_add(1, Ordering::Relaxed);
                        warn!("BLE: det_tx channel full, dropping BLE4 detection (expired)");
                    }
                }
            }
            state.emitted = true;
        }
    }
}

/// Merge new RemoteIdData fields into existing accumulated state.
/// For Location messages, always update (drone is moving).
fn merge_remote_id(existing: &mut RemoteIdData, new: &RemoteIdData) {
    // BasicID — take first seen
    if existing.uas_id.is_none() && new.uas_id.is_some() {
        existing.uas_id = new.uas_id.clone();
        existing.id_type = new.id_type;
        existing.ua_type = new.ua_type;
    }
    // Location — ALWAYS update (drone position changes)
    if new.latitude.is_some() {
        existing.latitude = new.latitude;
        existing.longitude = new.longitude;
        existing.altitude_geodetic = new.altitude_geodetic;
        existing.altitude_pressure = new.altitude_pressure;
        existing.height_agl = new.height_agl;
        existing.height_reference = new.height_reference;
        existing.speed = new.speed;
        existing.vertical_speed = new.vertical_speed;
        existing.heading = new.heading;
        existing.operational_status = new.operational_status;
    }
    // System (operator location) — ALWAYS update (operator may move)
    if new.operator_latitude.is_some() {
        existing.operator_latitude = new.operator_latitude;
        existing.operator_longitude = new.operator_longitude;
        existing.operator_altitude = new.operator_altitude;
        existing.operator_location_type = new.operator_location_type;
    }
    // OperatorID — take first seen
    if existing.operator_id.is_none() && new.operator_id.is_some() {
        existing.operator_id = new.operator_id.clone();
    }
    // SelfID — take first seen
    if existing.self_id_description.is_none() && new.self_id_description.is_some() {
        existing.self_id_description = new.self_id_description.clone();
        existing.registration = new.registration.clone();
    }
}

/// Build a proto::Detection from decoded RemoteID data
fn build_detection(
    mac: &str,
    rssi: i32,
    tap_id: &str,
    rid: &RemoteIdData,
    raw_payload: &[u8],
    source: proto::DetectionSource,
) -> proto::Detection {
    let serial = rid.uas_id.clone().unwrap_or_default();
    let identifier = if !serial.is_empty() {
        serial.clone()
    } else {
        mac.to_string()
    };

    proto::Detection {
        tap_id: tap_id.to_string(),
        timestamp_ns: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos() as i64,
        mac_address: mac.to_string(),
        identifier,
        serial_number: serial,
        registration: rid.registration.clone().unwrap_or_default(),
        session_id: uuid::Uuid::new_v5(&uuid::Uuid::NAMESPACE_OID, mac.as_bytes()).to_string(),
        utm_id: String::new(),
        latitude: rid.latitude.unwrap_or(0.0),
        longitude: rid.longitude.unwrap_or(0.0),
        altitude_geodetic: rid.altitude_geodetic.unwrap_or(0.0),
        altitude_pressure: rid.altitude_pressure.unwrap_or(0.0),
        height_agl: rid.height_agl.unwrap_or(0.0),
        height_reference: rid.height_reference as i32,
        speed: rid.speed.unwrap_or(0.0),
        vertical_speed: rid.vertical_speed.unwrap_or(0.0),
        heading: rid.heading.unwrap_or(0.0),
        track_direction: 0.0,
        operator_latitude: rid.operator_latitude.unwrap_or(0.0),
        operator_longitude: rid.operator_longitude.unwrap_or(0.0),
        operator_altitude: rid.operator_altitude.unwrap_or(0.0),
        operator_location_type: rid.operator_location_type as i32,
        operator_id: rid.operator_id.clone().unwrap_or_default(),
        rssi,
        channel: 0,
        frequency_mhz: 0,
        ssid: String::new(),
        beacon_interval_tu: 0,
        source: source as i32,
        uav_type: ua_type_to_string(rid.ua_type).to_string(),
        uav_category: ua_type_to_category(rid.ua_type) as i32,
        designation: String::new(),
        manufacturer: String::new(),
        operational_status: rid.operational_status as i32,
        confidence: 0.95,
        is_controller: false,
        country_code: String::new(),
        raw_frame: Vec::new(),
        remoteid_payload: raw_payload.to_vec(),
        ht_capabilities: 0,
    }
}

/// Map ASTM F3411 ua_type to human-readable string
fn ua_type_to_string(ua_type: u8) -> &'static str {
    match ua_type {
        0 => "None",
        1 => "Aeroplane",
        2 => "Helicopter/Multirotor",
        3 => "Gyroplane",
        4 => "Hybrid Lift (VTOL)",
        5 => "Ornithopter",
        6 => "Glider",
        7 => "Kite",
        8 => "Free Balloon",
        9 => "Captive Balloon",
        10 => "Airship",
        11 => "Free Fall/Parachute",
        12 => "Rocket",
        13 => "Ground Obstacle",
        14 => "Other",
        _ => "Unknown",
    }
}

/// Map ASTM F3411 ua_type to proto UAVCategory enum
fn ua_type_to_category(ua_type: u8) -> proto::UavCategory {
    match ua_type {
        0 => proto::UavCategory::UavCatUndeclared,
        1 => proto::UavCategory::UavCatAeroplane,
        2 => proto::UavCategory::UavCatHelicopter,
        3 => proto::UavCategory::UavCatGyroplane,
        4 => proto::UavCategory::UavCatHybridLift,
        5 => proto::UavCategory::UavCatOrnithopter,
        6 => proto::UavCategory::UavCatGlider,
        7 => proto::UavCategory::UavCatKite,
        8 => proto::UavCategory::UavCatFreeBalloon,
        9 => proto::UavCategory::UavCatCaptiveBalloon,
        10 => proto::UavCategory::UavCatAirship,
        11 => proto::UavCategory::UavCatFreeFall,
        12 => proto::UavCategory::UavCatRocket,
        13 => proto::UavCategory::UavCatGroundObstacle,
        14 => proto::UavCategory::UavCatOther,
        _ => proto::UavCategory::UavCatUnknown,
    }
}
