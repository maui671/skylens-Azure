use anyhow::{Context, Result};
use serde::Deserialize;
use std::collections::HashMap;
use std::path::Path;
use tracing::info;

/// SSID pattern from drone_models.json
#[derive(Debug, Deserialize, Clone)]
pub struct SsidPattern {
    pub pattern: String,
    pub manufacturer: String,
    #[serde(default, alias = "model_hint")]
    pub model_hint: String,
    #[serde(default)]
    pub is_controller: bool,
}

/// Serial prefix entry
#[derive(Debug, Deserialize, Clone)]
pub struct SerialPrefix {
    pub manufacturer: String,
    pub model: String,
}

/// Complete intel database loaded from drone_models.json
pub struct IntelDatabase {
    /// OUI prefix [0x04, 0xa8, 0x5a] → description string (e.g. "DJI (drone)")
    /// Uses byte array key for zero-alloc lookups (no format!() per frame)
    pub oui_map: HashMap<[u8; 3], String>,
    /// SSID compiled regex patterns
    pub ssid_patterns: Vec<(regex::Regex, SsidPattern)>,
    /// Serial prefix → model info
    pub serial_prefixes: HashMap<String, SerialPrefix>,
    /// DJI model code → model name
    pub dji_model_codes: HashMap<String, String>,
    /// DJI SSID model string → model name
    pub dji_ssid_models: HashMap<String, String>,
}

/// Raw JSON structure matching drone_models.json
#[derive(Debug, Deserialize)]
struct RawIntelDb {
    oui_map: HashMap<String, String>,
    ssid_patterns: Vec<SsidPattern>,
    serial_prefixes: HashMap<String, SerialPrefix>,
    dji_model_codes: HashMap<String, String>,
    dji_ssid_models: HashMap<String, String>,
}

impl IntelDatabase {
    pub fn load(path: &Path) -> Result<Self> {
        let content = std::fs::read_to_string(path)
            .with_context(|| format!("Failed to read intel database: {}", path.display()))?;

        let raw: RawIntelDb =
            serde_json::from_str(&content).with_context(|| "Failed to parse intel JSON")?;

        // Compile SSID regex patterns
        let mut ssid_compiled = Vec::with_capacity(raw.ssid_patterns.len());
        for pat in &raw.ssid_patterns {
            match regex::Regex::new(&pat.pattern) {
                Ok(re) => ssid_compiled.push((re, pat.clone())),
                Err(e) => {
                    tracing::warn!("Skipping invalid SSID pattern '{}': {}", pat.pattern, e);
                }
            }
        }

        let dji_model_codes = raw.dji_model_codes;

        // Parse OUI string keys ("04:a8:5a") into [u8; 3] for zero-alloc lookup
        let mut oui_map: HashMap<[u8; 3], String> = HashMap::with_capacity(raw.oui_map.len());
        for (k, v) in raw.oui_map {
            let k_lower = k.to_lowercase();
            let parts: Vec<&str> = k_lower.split(':').collect();
            if parts.len() == 3 {
                if let (Ok(a), Ok(b), Ok(c)) = (
                    u8::from_str_radix(parts[0], 16),
                    u8::from_str_radix(parts[1], 16),
                    u8::from_str_radix(parts[2], 16),
                ) {
                    oui_map.insert([a, b, c], v);
                }
            }
        }

        info!(
            "Intel database loaded: {} OUIs, {} SSID patterns, {} serial prefixes, {} DJI model codes, {} DJI SSID models",
            oui_map.len(),
            ssid_compiled.len(),
            raw.serial_prefixes.len(),
            dji_model_codes.len(),
            raw.dji_ssid_models.len(),
        );

        Ok(Self {
            oui_map,
            ssid_patterns: ssid_compiled,
            serial_prefixes: raw.serial_prefixes,
            dji_model_codes,
            dji_ssid_models: raw.dji_ssid_models,
        })
    }

    /// Check if a MAC address matches a known drone OUI.
    /// Returns the description string (e.g. "DJI (drone)").
    /// Zero-alloc: uses [u8; 3] key directly from MAC bytes.
    pub fn match_oui(&self, mac: &[u8; 6]) -> Option<&str> {
        self.oui_map.get(&[mac[0], mac[1], mac[2]]).map(|s| s.as_str())
    }

    /// Extract manufacturer name from OUI description string.
    /// "DJI (drone)" → "DJI", "Parrot SA (drone)" → "Parrot SA"
    pub fn manufacturer_from_oui(desc: &str) -> &str {
        desc.find(" (").map_or(desc, |idx| &desc[..idx])
    }

    /// Check if an SSID matches any drone pattern
    pub fn match_ssid(&self, ssid: &str) -> Option<&SsidPattern> {
        for (re, pattern) in &self.ssid_patterns {
            if re.is_match(ssid) {
                return Some(pattern);
            }
        }
        None
    }

    /// Look up a serial number prefix
    pub fn match_serial(&self, serial: &str) -> Option<&SerialPrefix> {
        // Try progressively shorter prefixes (longest match first)
        for len in (3..=serial.len().min(10)).rev() {
            if let Some(substr) = serial.get(..len) {
                if let Some(entry) = self.serial_prefixes.get(substr) {
                    return Some(entry);
                }
            }
        }
        None
    }

    /// Look up a DJI model code
    pub fn match_dji_model_code(&self, code: &str) -> Option<&str> {
        self.dji_model_codes.get(code).map(|s| s.as_str())
    }

    /// Look up a DJI SSID model string.
    /// DJI SSIDs look like "DJI-MAVICPRO-A1B2" — extract the model part and match.
    pub fn match_dji_ssid_model(&self, ssid: &str) -> Option<&str> {
        // First try exact match
        if let Some(model) = self.dji_ssid_models.get(ssid) {
            return Some(model.as_str());
        }

        // Extract model part from DJI SSID format: "DJI-<MODEL>-<SUFFIX>"
        let upper = ssid.to_uppercase();
        let stripped = upper.strip_prefix("DJI-").unwrap_or(&upper);
        // Remove the last segment (e.g., "-A1B2") which is typically a 4-char hex suffix
        if let Some(dash_pos) = stripped.rfind('-') {
            let model_part = &stripped[..dash_pos];
            if let Some(model) = self.dji_ssid_models.get(model_part) {
                return Some(model.as_str());
            }
        }
        // Try the full stripped string (without DJI- prefix)
        if let Some(model) = self.dji_ssid_models.get(stripped) {
            return Some(model.as_str());
        }

        // Fallback: only for DJI-prefixed SSIDs, check if SSID contains a known model key
        // Guard: require DJI prefix to avoid false positives from short keys like "FPV", "NEO"
        if upper.starts_with("DJI") {
            let mut best: Option<(&str, usize)> = None;
            for (key, model) in &self.dji_ssid_models {
                if key.len() >= 4 && upper.contains(key.as_str()) {
                    if best.is_none() || key.len() > best.unwrap().1 {
                        best = Some((model.as_str(), key.len()));
                    }
                }
            }
            return best.map(|(m, _)| m);
        }

        None
    }

    /// BPF filter for pcap capture — management + data frames.
    /// Excludes control frames (ACK/CTS/RTS) which have no useful MAC/OUI data.
    /// Data frames are needed for OUI matching on drone data traffic (OcuSync link)
    /// since Realtek firmware drops NAN Action frames.
    pub fn build_bpf_filter(&self) -> Option<String> {
        Some("not type ctl".to_string())
    }
}
