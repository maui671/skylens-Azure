# Realtek RTL8812AU / RTL8814AU Beacon Capture Fix

## The Problem

Realtek RTL8812AU and RTL8814AU WiFi adapters (commonly sold under the ALFA Network brand) **do not capture Beacon frames in monitor mode** with the standard `88XXau` and `8814au` Linux drivers.

This is a **hardware-level filter** bug, not a software issue. The driver sets the `RCR_CBSSID_BCN` bit in the chip's Receive Configuration Register (RCR), which tells the hardware to only accept beacons matching the associated BSSID. In monitor mode there is no associated BSSID, so the hardware silently drops **all** beacon frames.

### Impact

Without beacons, you cannot:
- Detect OpenDroneID / ASTM F3411 RemoteID broadcasts (sent in beacon vendor IEs)
- Parse SSIDs from access points and drones
- Capture probe responses from drone WiFi networks
- See any beacon-based vendor-specific information elements

### How to Identify the Problem

```bash
# Check if your adapter is affected
lsusb | grep -i realtek
# 0bda:8812 = RTL8812AU (affected)
# 0bda:8813 = RTL8814AU (affected)

# Put interface in monitor mode
sudo ip link set wlan1 down
sudo iw dev wlan1 set type monitor
sudo ip link set wlan1 up
sudo iw dev wlan1 set channel 6

# Capture and count frame types (channel 6 should have MANY beacons)
sudo timeout 10 tshark -i wlan1 -c 500 -T fields -e wlan.fc.type_subtype 2>/dev/null \
  | sort | uniq -c | sort -rn

# If you see ZERO "0x0008" (Beacon) lines, you are affected
# You'll see mostly control frames (0x001c, 0x001d, 0x001b, 0x0019)
```

### Which Drivers Are Affected

| Driver Module | USB IDs | Source | Affected |
|--------------|---------|--------|----------|
| `88XXau` | 0bda:8812, 0bda:8813 | aircrack-ng/rtl8812au, realtek-rtl88xxau DKMS | Yes |
| `8814au` | 0bda:8813 | rtl8814au DKMS | Yes |
| `mt7921u` / `mt76` | 0e8d:7961 (MediaTek) | Mainline Linux kernel | No (works correctly) |

### Adapters Known to Be Affected

- ALFA AWUS036ACH (RTL8812AU)
- ALFA AWUS036ACH v2 (RTL8812AU)
- ALFA AWUS1900 (RTL8814AU)
- Any USB adapter using Realtek RTL8812AU or RTL8814AU chipsets

### Adapters Known to Work Without Patching

- ALFA AWUS036ACHM (MT7612U, uses mt76 driver)
- Any adapter using MediaTek MT7921U, MT7612U, MT7610U (mainline mt76 driver)

## The Fix

Patch the `rtw_hal_rcr_set_chk_bssid()` function in the driver's `hal/hal_com.c` to clear the beacon BSSID filter when in monitor mode.

### Step 1: Find the DKMS Source

```bash
# For 88XXau driver (RTL8812AU)
ls /usr/src/ | grep -i 88xx
# Example: realtek-rtl88xxau-5.6.4.2~20230501

# For 8814au driver (RTL8814AU)
ls /usr/src/ | grep -i 8814
# Example: rtl8814au-5.8.5.1
```

### Step 2: Apply the Patch

For `88XXau` (RTL8812AU):
```bash
DRIVER_SRC=/usr/src/realtek-rtl88xxau-5.6.4.2~20230501  # adjust version

# Backup
sudo cp $DRIVER_SRC/hal/hal_com.c $DRIVER_SRC/hal/hal_com.c.bak

# Find the function and add the monitor mode check
sudo sed -i '/rcr_new = rcr;/a\
\t/* Monitor mode: accept all beacons and data regardless of BSSID */\
\tif (check_fwstate(\&adapter->mlmepriv, WIFI_MONITOR_STATE)) {\
\t\trcr_new \&= ~(RCR_CBSSID_BCN | RCR_CBSSID_DATA);\
\t\tif (rcr != rcr_new)\
\t\t\trtw_hal_set_hwreg(adapter, HW_VAR_RCR, (u8 *)\&rcr_new);\
\t\treturn;\
\t}' $DRIVER_SRC/hal/hal_com.c

# Fix potential stray character from sed
sudo sed -i 's/^n\t\/\* Monitor/\t\/\* Monitor/' $DRIVER_SRC/hal/hal_com.c
```

For `8814au` (RTL8814AU):
```bash
DRIVER_SRC=/usr/src/rtl8814au-5.8.5.1  # adjust version

# Same patch, different source path
sudo cp $DRIVER_SRC/hal/hal_com.c $DRIVER_SRC/hal/hal_com.c.bak

sudo sed -i '/rcr_new = rcr;/a\
\t/* Monitor mode: accept all beacons and data regardless of BSSID */\
\tif (check_fwstate(\&adapter->mlmepriv, WIFI_MONITOR_STATE)) {\
\t\trcr_new \&= ~(RCR_CBSSID_BCN | RCR_CBSSID_DATA);\
\t\tif (rcr != rcr_new)\
\t\t\trtw_hal_set_hwreg(adapter, HW_VAR_RCR, (u8 *)\&rcr_new);\
\t\treturn;\
\t}' $DRIVER_SRC/hal/hal_com.c

sudo sed -i 's/^n\t\/\* Monitor/\t\/\* Monitor/' $DRIVER_SRC/hal/hal_com.c
```

### Step 3: Verify the Patch

```bash
# Check the patched code looks correct
grep -A 8 'rcr_new = rcr;' $DRIVER_SRC/hal/hal_com.c | head -12

# Expected output:
#     rcr_new = rcr;
#     /* Monitor mode: accept all beacons and data regardless of BSSID */
#     if (check_fwstate(&adapter->mlmepriv, WIFI_MONITOR_STATE)) {
#         rcr_new &= ~(RCR_CBSSID_BCN | RCR_CBSSID_DATA);
#         if (rcr != rcr_new)
#             rtw_hal_set_hwreg(adapter, HW_VAR_RCR, (u8 *)&rcr_new);
#         return;
#     }
```

### Step 4: Rebuild the DKMS Module

```bash
KERNEL=$(uname -r)

# For 88XXau
MODULE=realtek-rtl88xxau
VERSION=5.6.4.2~20230501  # adjust

sudo dkms remove $MODULE/$VERSION -k $KERNEL
sudo dkms build $MODULE/$VERSION -k $KERNEL
sudo dkms install $MODULE/$VERSION -k $KERNEL

# For 8814au
MODULE=rtl8814au
VERSION=5.8.5.1  # adjust

sudo dkms remove $MODULE/$VERSION -k $KERNEL
sudo dkms build $MODULE/$VERSION -k $KERNEL
sudo dkms install $MODULE/$VERSION -k $KERNEL
```

### Step 5: Reload the Module

```bash
# For 88XXau
sudo rmmod 88XXau
sudo modprobe 88XXau

# For 8814au
sudo rmmod 8814au
sudo modprobe 8814au

# Re-setup monitor mode
sudo ip link set wlan1 down
sudo iw dev wlan1 set type monitor
sudo ip link set wlan1 up
sudo iw dev wlan1 set channel 6
```

### Step 6: Verify Beacons Are Captured

```bash
sudo timeout 5 tshark -i wlan1 -c 200 -T fields -e wlan.fc.type_subtype 2>/dev/null \
  | sort | uniq -c | sort -rn

# You should now see "0x0008" (Beacon) frames!
# Before: 0 beacons
# After:  Dozens of beacons per second on populated channels
```

## What the Patch Does

The `rtw_hal_rcr_set_chk_bssid()` function in `hal/hal_com.c` controls the hardware RX filter register (RCR). The original code never checks for monitor mode, so `RCR_CBSSID_BCN` (bit 7) stays set, telling the chip: "only accept beacons matching my associated BSSID."

Our patch adds an early-return check at the top of the function:

```c
/* Monitor mode: accept all beacons and data regardless of BSSID */
if (check_fwstate(&adapter->mlmepriv, WIFI_MONITOR_STATE)) {
    rcr_new &= ~(RCR_CBSSID_BCN | RCR_CBSSID_DATA);
    if (rcr != rcr_new)
        rtw_hal_set_hwreg(adapter, HW_VAR_RCR, (u8 *)&rcr_new);
    return;
}
```

This clears both `RCR_CBSSID_BCN` (beacon filter) and `RCR_CBSSID_DATA` (data BSSID filter) when in monitor mode, allowing the hardware to deliver all frames to the driver.

## Important Notes

- This patch must be **re-applied after kernel upgrades** if DKMS rebuilds the module from the original (unpatched) source. To make it permanent, keep the patched source in the DKMS tree.
- The patch only affects monitor mode behavior. Normal managed-mode operation is unchanged.
- If using both `88XXau` and `8814au` drivers on the same system, patch both.
- After a kernel update, verify beacons are still captured with the tshark test above.

## Alternative: Use MediaTek Adapters

If you want to avoid driver patching entirely, use WiFi adapters with MediaTek chipsets that have proper mainline Linux kernel support:

| Adapter | Chipset | Driver | Monitor Mode |
|---------|---------|--------|-------------|
| ALFA AWUS036ACHM | MT7612U | mt76 (mainline) | Full support |
| Any MT7921U USB | MT7921U | mt7921u (mainline) | Full support |
| Any MT7610U USB | MT7610U | mt76 (mainline) | Full support |

These adapters require no driver patching and properly capture all frame types in monitor mode.
