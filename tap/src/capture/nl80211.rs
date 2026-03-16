use anyhow::{Context, Result};
use neli::{
    consts::{
        nl::NlmF,
        socket::NlFamily,
    },
    genl::{AttrTypeBuilder, GenlmsghdrBuilder, NlattrBuilder, NoUserHeader},
    nl::NlPayload,
    router::synchronous::NlRouter,
    types::GenlBuffer,
    utils::Groups,
};
use tracing::{debug, info};

// nl80211 command constants (from linux/nl80211.h)
#[neli::neli_enum(serialized_type = "u8")]
pub enum Nl80211Cmd {
    GetWiphy = 1,
    SetWiphy = 2,
    GetInterface = 5,
}
impl neli::consts::genl::Cmd for Nl80211Cmd {}

// nl80211 attribute constants (from linux/nl80211.h)
#[neli::neli_enum(serialized_type = "u16")]
pub enum Nl80211Attr {
    Wiphy = 1,
    Ifindex = 3,
    Ifname = 4,
    WiphyFreq = 38,
    WiphyChannelType = 39,
    ChannelWidth = 159,
    CenterFreq1 = 160,
}
impl neli::consts::genl::NlAttrType for Nl80211Attr {}

/// nl80211 channel width values (NL80211_CHAN_WIDTH_*)
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[repr(u32)]
pub enum ChanWidth {
    Width20NoHt = 0,
    Width20 = 1,
    Width40 = 2,
    Width80 = 3,
}

impl ChanWidth {
    /// MHz value for iw command
    pub fn mhz(&self) -> i32 {
        match self {
            ChanWidth::Width20NoHt | ChanWidth::Width20 => 20,
            ChanWidth::Width40 => 40,
            ChanWidth::Width80 => 80,
        }
    }

    /// Next narrower width for fallback
    pub fn narrower(&self) -> Option<ChanWidth> {
        match self {
            ChanWidth::Width80 => Some(ChanWidth::Width40),
            ChanWidth::Width40 => Some(ChanWidth::Width20NoHt),
            _ => None,
        }
    }
}

/// A single step in the hop sequence
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HopStep {
    /// Representative channel number (lowest in the block)
    pub channel: i32,
    /// Width to use for this hop
    pub width: ChanWidth,
    /// Primary frequency MHz
    pub freq: i32,
    /// Center frequency for 40/80 MHz
    pub center_freq1: i32,
    /// All channels covered by this hop (for logging)
    pub covers: Vec<i32>,
}

/// 2.4 GHz HT40 block definitions for width hopping.
/// Two-hop plan covering all DJI DroneID burst frequencies (2399.5-2459.5 MHz):
///   Block 1 (Ch1 HT40+): 2402-2442 MHz — DroneID at 2414.5 + 2429.5 MHz
///   Block 2 (Ch6 HT40+): 2427-2467 MHz — NAN RemoteID + DroneID at 2429.5 + 2444.5 + 2459.5 MHz
/// Together: all 5 DroneID hop frequencies covered plus NAN discovery on ch6.
const BLOCKS_24GHZ_40MHZ: &[(i32, &[i32])] = &[
    (2422, &[1, 2, 3, 4, 5]),        // Ch 1 HT40+: primary=2412, center=2422
    (2447, &[6, 7, 8, 9, 10, 11]),   // Ch 6 HT40+: primary=2437, center=2447
];

/// 80 MHz block definitions for 5 GHz
/// Each block: (center_freq1, channels covered)
const BLOCKS_80MHZ: &[(i32, &[i32])] = &[
    (5210, &[36, 40, 44, 48]),
    (5290, &[52, 56, 60, 64]),
    (5530, &[100, 104, 108, 112]),
    (5610, &[116, 120, 124, 128]),
    (5690, &[132, 136, 140, 144]),
    (5775, &[149, 153, 157, 161]),
];

/// 40 MHz sub-block definitions for 5 GHz
/// Each block: (center_freq1, channels covered)
const BLOCKS_40MHZ: &[(i32, &[i32])] = &[
    (5190, &[36, 40]),
    (5230, &[44, 48]),
    (5270, &[52, 56]),
    (5310, &[60, 64]),
    (5510, &[100, 104]),
    (5550, &[108, 112]),
    (5590, &[116, 120]),
    (5630, &[124, 128]),
    (5670, &[132, 136]),
    (5710, &[140, 144]),
    (5755, &[149, 153]),
    (5795, &[157, 161]),
];

/// Build an optimized hop sequence from a list of channels.
/// Groups 5 GHz channels into 80 MHz blocks where possible, falls back to 40/20 MHz.
/// 2.4 GHz always uses 20 MHz NoHT.
/// Priority: ch6 gets 5x dwell, ch149 block gets 2x dwell.
pub fn build_hop_sequence(channels: &[i32], width_hopping: bool) -> Vec<HopStep> {
    let mut steps: Vec<HopStep> = Vec::new();

    // Separate 2.4 GHz and 5 GHz channels
    let mut ch_24: Vec<i32> = Vec::new();
    let mut ch_5: Vec<i32> = Vec::new();
    for &ch in channels {
        if ch >= 1 && ch <= 14 {
            ch_24.push(ch);
        } else if ch >= 36 {
            ch_5.push(ch);
        }
    }

    if !width_hopping {
        // Width hopping disabled — individual 20 MHz for everything
        for &ch in &ch_24 {
            let freq = channel_to_freq(ch);
            steps.push(HopStep {
                channel: ch,
                width: ChanWidth::Width20NoHt,
                freq,
                center_freq1: freq,
                covers: vec![ch],
            });
        }
        for &ch in &ch_5 {
            let freq = channel_to_freq(ch);
            steps.push(HopStep {
                channel: ch,
                width: ChanWidth::Width20NoHt,
                freq,
                center_freq1: freq,
                covers: vec![ch],
            });
        }
        return steps;
    }

    // 2.4 GHz with width hopping: try HT40 blocks for DJI DroneID frequency coverage
    let mut covered_24: std::collections::HashSet<i32> = std::collections::HashSet::new();

    for &(cf1, block_chs) in BLOCKS_24GHZ_40MHZ {
        let block_present: Vec<i32> = block_chs.iter().filter(|c| ch_24.contains(c)).copied().collect();
        if block_present.len() == block_chs.len() {
            // All channels present — use HT40
            let primary_ch = block_chs[0];
            let primary_freq = channel_to_freq(primary_ch);
            steps.push(HopStep {
                channel: primary_ch,
                width: ChanWidth::Width40,
                freq: primary_freq,
                center_freq1: cf1,
                covers: block_present.clone(),
            });
            for ch in &block_present {
                covered_24.insert(*ch);
            }
        }
    }

    // Any uncovered 2.4 GHz channels at 20 MHz
    for &ch in &ch_24 {
        if !covered_24.contains(&ch) {
            let freq = channel_to_freq(ch);
            steps.push(HopStep {
                channel: ch,
                width: ChanWidth::Width20NoHt,
                freq,
                center_freq1: freq,
                covers: vec![ch],
            });
        }
    }

    // 5 GHz with width hopping: try to group into 80 MHz blocks
    let mut covered: std::collections::HashSet<i32> = std::collections::HashSet::new();

    // Pass 1: 80 MHz blocks — only if ALL channels in the block are in our list
    for &(cf1, block_chs) in BLOCKS_80MHZ {
        let block_present: Vec<i32> = block_chs.iter().filter(|c| ch_5.contains(c)).copied().collect();
        if block_present.len() == block_chs.len() {
            // All channels present — use 80 MHz
            let primary_ch = block_chs[0];
            let primary_freq = channel_to_freq(primary_ch);
            steps.push(HopStep {
                channel: primary_ch,
                width: ChanWidth::Width80,
                freq: primary_freq,
                center_freq1: cf1,
                covers: block_present.clone(),
            });
            for ch in &block_present {
                covered.insert(*ch);
            }
        }
    }

    // Pass 2: 40 MHz blocks for remaining uncovered channels
    for &(cf1, block_chs) in BLOCKS_40MHZ {
        let block_present: Vec<i32> = block_chs.iter()
            .filter(|c| ch_5.contains(c) && !covered.contains(c))
            .copied()
            .collect();
        if block_present.len() == block_chs.len() {
            let primary_ch = block_chs[0];
            let primary_freq = channel_to_freq(primary_ch);
            steps.push(HopStep {
                channel: primary_ch,
                width: ChanWidth::Width40,
                freq: primary_freq,
                center_freq1: cf1,
                covers: block_present.clone(),
            });
            for ch in &block_present {
                covered.insert(*ch);
            }
        }
    }

    // Pass 3: remaining 5 GHz channels at 20 MHz
    for &ch in &ch_5 {
        if !covered.contains(&ch) {
            let freq = channel_to_freq(ch);
            steps.push(HopStep {
                channel: ch,
                width: ChanWidth::Width20NoHt,
                freq,
                center_freq1: freq,
                covers: vec![ch],
            });
        }
    }

    steps
}

/// Expand a HopStep into narrower fallback steps when width fails.
/// 80 MHz → two 40 MHz steps. 40 MHz → two 20 MHz steps.
pub fn fallback_steps(step: &HopStep) -> Vec<HopStep> {
    let narrower = match step.width.narrower() {
        Some(w) => w,
        None => return vec![],
    };

    if narrower == ChanWidth::Width40 {
        // 80 MHz failed → split into 40 MHz pairs
        let mut result = Vec::new();
        for &(cf1, block_chs) in BLOCKS_40MHZ {
            let sub: Vec<i32> = block_chs.iter()
                .filter(|c| step.covers.contains(c))
                .copied()
                .collect();
            if sub.len() == block_chs.len() {
                let freq = channel_to_freq(sub[0]);
                result.push(HopStep {
                    channel: sub[0],
                    width: ChanWidth::Width40,
                    freq,
                    center_freq1: cf1,
                    covers: sub,
                });
            }
        }
        // Any channels not in a 40 MHz pair get 20 MHz
        let in_40: std::collections::HashSet<i32> = result.iter().flat_map(|s| s.covers.iter()).copied().collect();
        for &ch in &step.covers {
            if !in_40.contains(&ch) {
                let freq = channel_to_freq(ch);
                result.push(HopStep {
                    channel: ch,
                    width: ChanWidth::Width20NoHt,
                    freq,
                    center_freq1: freq,
                    covers: vec![ch],
                });
            }
        }
        result
    } else {
        // 40 MHz failed → individual 20 MHz channels
        step.covers.iter().map(|&ch| {
            let freq = channel_to_freq(ch);
            HopStep {
                channel: ch,
                width: ChanWidth::Width20NoHt,
                freq,
                center_freq1: freq,
                covers: vec![ch],
            }
        }).collect()
    }
}

/// Netlink nl80211 channel switcher — replaces `iw dev <iface> set channel`.
/// Opens a genetlink socket once at construction and reuses it for all channel switches.
pub struct Nl80211Channel {
    ifindex: u32,
    family_id: u16,
    router: NlRouter,
}

impl Nl80211Channel {
    /// Create a new nl80211 channel switcher for the given interface.
    /// Resolves the nl80211 genetlink family and interface index at startup.
    pub fn new(interface: &str) -> Result<Self> {
        let ifindex = Self::get_ifindex(interface)
            .with_context(|| format!("Failed to get ifindex for {}", interface))?;

        let (router, _) = NlRouter::connect(NlFamily::Generic, Some(0), Groups::empty())
            .context("Failed to connect genetlink socket")?;

        let family_id = router
            .resolve_genl_family("nl80211")
            .context("Failed to resolve nl80211 genetlink family")?;

        info!(
            interface,
            ifindex, family_id, "nl80211 netlink channel switcher initialized"
        );

        Ok(Self {
            ifindex,
            family_id,
            router,
        })
    }

    /// Set the WiFi interface to a specific channel (20MHz NoHT) via netlink.
    /// This matches exactly what `iw dev <iface> set channel <ch>` sends:
    /// IFINDEX + WIPHY_FREQ + CHANNEL_WIDTH(Width20NoHt=0) + CENTER_FREQ1(=primary_freq)
    ///
    /// Previous code used Width20 (HT20, value 1) which Realtek drivers ACK
    /// but silently ignore. Width20NoHt (value 0) is what iw actually uses.
    pub fn set_channel(&self, channel: i32) -> Result<()> {
        let freq = channel_to_freq(channel);
        if freq == 0 {
            anyhow::bail!("Invalid channel number: {}", channel);
        }
        self.set_freq(freq, ChanWidth::Width20NoHt, freq)
    }

    /// Set the WiFi interface to a specific frequency/width/center via netlink.
    pub fn set_freq(&self, freq: i32, width: ChanWidth, center_freq1: i32) -> Result<()> {
        let attr_vec = vec![
            NlattrBuilder::default()
                .nla_type(
                    AttrTypeBuilder::default()
                        .nla_type(Nl80211Attr::Ifindex)
                        .build()
                        .map_err(|e| anyhow::anyhow!("AttrType build: {}", e))?,
                )
                .nla_payload(self.ifindex)
                .build()
                .map_err(|e| anyhow::anyhow!("Nlattr build: {}", e))?,
            NlattrBuilder::default()
                .nla_type(
                    AttrTypeBuilder::default()
                        .nla_type(Nl80211Attr::WiphyFreq)
                        .build()
                        .map_err(|e| anyhow::anyhow!("AttrType build: {}", e))?,
                )
                .nla_payload(freq as u32)
                .build()
                .map_err(|e| anyhow::anyhow!("Nlattr build: {}", e))?,
            NlattrBuilder::default()
                .nla_type(
                    AttrTypeBuilder::default()
                        .nla_type(Nl80211Attr::ChannelWidth)
                        .build()
                        .map_err(|e| anyhow::anyhow!("AttrType build: {}", e))?,
                )
                .nla_payload(width as u32)
                .build()
                .map_err(|e| anyhow::anyhow!("Nlattr build: {}", e))?,
            NlattrBuilder::default()
                .nla_type(
                    AttrTypeBuilder::default()
                        .nla_type(Nl80211Attr::CenterFreq1)
                        .build()
                        .map_err(|e| anyhow::anyhow!("AttrType build: {}", e))?,
                )
                .nla_payload(center_freq1 as u32)
                .build()
                .map_err(|e| anyhow::anyhow!("Nlattr build: {}", e))?,
        ];

        let attrs = attr_vec.into_iter().collect::<GenlBuffer<_, _>>();

        let genl_payload = GenlmsghdrBuilder::<Nl80211Cmd, Nl80211Attr, NoUserHeader>::default()
            .cmd(Nl80211Cmd::SetWiphy)
            .version(0)
            .attrs(attrs)
            .build()
            .map_err(|e| anyhow::anyhow!("Genlmsghdr build: {}", e))?;

        let recv = self.router.send::<_, _, u16, neli::genl::Genlmsghdr<Nl80211Cmd, Nl80211Attr>>(
            self.family_id,
            NlmF::ACK,
            NlPayload::Payload(genl_payload),
        )?;

        // Consume the response iterator to check for errors
        for msg in recv {
            match msg {
                Ok(_) => {}
                Err(e) => {
                    let err_str = format!("{}", e);
                    if err_str.contains("Busy") || err_str.contains("busy") || err_str.contains("-16") {
                        return Err(anyhow::anyhow!("nl80211 set_freq EBUSY (freq {} width {:?})", freq, width));
                    }
                    return Err(anyhow::anyhow!("nl80211 set_freq error: {}", e));
                }
            }
        }

        debug!(freq, width = ?width, center_freq1, "Hopped to freq (netlink)");
        Ok(())
    }

    /// Read interface index from sysfs
    fn get_ifindex(interface: &str) -> Result<u32> {
        let path = format!("/sys/class/net/{}/ifindex", interface);
        let content = std::fs::read_to_string(&path)
            .with_context(|| format!("Cannot read {}", path))?;
        content
            .trim()
            .parse::<u32>()
            .with_context(|| format!("Invalid ifindex in {}", path))
    }
}

/// Convert WiFi channel number to frequency in MHz
pub fn channel_to_freq(channel: i32) -> i32 {
    match channel {
        // 2.4 GHz band
        1..=13 => 2407 + 5 * channel,
        14 => 2484,
        // 5 GHz band
        36..=177 => 5000 + 5 * channel,
        _ => 0,
    }
}

/// Fallback: set channel using iw subprocess (20MHz NoHT)
pub async fn set_channel_iw(interface: &str, channel: i32) -> Result<()> {
    let output = tokio::process::Command::new("iw")
        .args(["dev", interface, "set", "channel", &channel.to_string()])
        .output()
        .await?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        if stderr.contains("Device or resource busy") {
            anyhow::bail!("iw set channel {} EBUSY (channel not changed)", channel);
        }
        anyhow::bail!("iw set channel {} failed: {}", channel, stderr.trim());
    }

    Ok(())
}

/// Set frequency with width via iw subprocess: `iw dev <iface> set freq <freq> <width> <cf1>`
pub async fn set_freq_iw(interface: &str, freq: i32, width: ChanWidth, center_freq1: i32) -> Result<()> {
    if width == ChanWidth::Width20NoHt || width == ChanWidth::Width20 {
        // For 20 MHz, use simple channel command
        let output = tokio::process::Command::new("iw")
            .args(["dev", interface, "set", "freq", &freq.to_string()])
            .output()
            .await?;
        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            if stderr.contains("Device or resource busy") {
                anyhow::bail!("iw set freq {} EBUSY (channel not changed)", freq);
            }
            anyhow::bail!("iw set freq {} failed: {}", freq, stderr.trim());
        }
    } else {
        // For 40/80 MHz: iw dev <iface> set freq <freq> <width_mhz> <center_freq1>
        let width_str = match width {
            ChanWidth::Width40 => "40",
            ChanWidth::Width80 => "80",
            _ => "20",
        };
        let output = tokio::process::Command::new("iw")
            .args([
                "dev", interface, "set", "freq",
                &freq.to_string(), width_str, &center_freq1.to_string(),
            ])
            .output()
            .await?;
        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            if stderr.contains("Device or resource busy") {
                anyhow::bail!("iw set freq {} {}MHz cf1={} EBUSY (channel not changed)", freq, width_str, center_freq1);
            }
            anyhow::bail!("iw set freq {} {}MHz cf1={} failed: {}", freq, width_str, center_freq1, stderr.trim());
        }
    }

    debug!(freq, width = ?width, center_freq1, "Hopped to freq (iw)");
    Ok(())
}
