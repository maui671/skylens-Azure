package intel

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UpdateResult contains the outcome of an intel update run.
type UpdateResult struct {
	PreviousVersion string            `json:"previous_version"`
	NewVersion      string            `json:"new_version"`
	NewOUIs         map[string]string `json:"new_ouis"`
	Source          string            `json:"source"`
	Timestamp       time.Time         `json:"timestamp"`
}

// ieeeManufacturer maps an IEEE organization name prefix to our label.
type ieeeManufacturer struct {
	Prefix string // exact prefix match against IEEE org name
	Label  string // value to write in oui_map
}

// ieeeManufacturers is the complete manufacturer match table.
// Uses strings.HasPrefix — not Contains — to prevent false positives.
var ieeeManufacturers = []ieeeManufacturer{
	{"SZ DJI Technology", "DJI (drone)"},
	{"SZ DJI Baiwang", "DJI Baiwang (drone)"},
	{"Parrot SA", "Parrot (drone)"},
	{"Parrot Drones", "Parrot (drone)"},
	{"Autel Robotics", "Autel Robotics (drone)"},
	{"Autel Intelligent", "Autel Intelligent (drone)"},
	{"Skydio", "Skydio (drone)"},
	{"Yuneec", "Yuneec (drone)"},
	{"Holy Stone", "Holy Stone (drone)"},
	{"Zipline", "Zipline (drone)"},
	{"XAG Co", "XAG (drone)"},
	{"Guangzhou XAG", "XAG (drone)"},
	{"Joby Aviation", "Joby Aviation (drone)"},
	{"Joby Aero", "Joby Aviation (drone)"},
	{"Teal Drones", "Teal Drones (drone)"},
	{"AeroVironment", "AeroVironment (drone)"},
	{"Prox Dynamics", "Black Hornet (drone)"},
	{"Zero Zero Robotics", "Zero Zero Robotics (drone)"},
	{"Hangzhou Zero Zero", "ZeroTech (drone)"},
	{"Hangzhou Zero Zhi", "ZeroTech (drone)"},
	{"Flyability", "Flyability (drone)"},
	{"Wingtra", "Wingtra (drone)"},
	{"senseFly", "AgEagle (drone)"},
	{"AgEagle", "AgEagle (drone)"},
	{"Inspired Flight", "Inspired Flight (drone)"},
	{"Draganfly", "Draganfly (drone)"},
	{"Brinc Drones", "Brinc (drone)"},
	{"EHang", "EHang (drone)"},
	{"Shenzhen Walkera", "Walkera (drone)"},
	{"Freefly Systems", "Freefly (drone)"},
	{"FreeFly Systems", "Freefly (drone)"},
	{"Wisk Aero", "Wisk Aero (drone)"},
	{"Matternet", "Matternet (drone)"},
	{"Percepto", "Percepto (drone)"},
	{"Airobotics", "Airobotics (drone)"},
	{"Quantum-Systems", "Quantum-Systems (drone)"},
	{"Wingcopter", "Wingcopter (drone)"},
	{"Microdrones", "MicroDrones (drone)"},
	{"Delair", "Delair (drone)"},
	{"Prodrone", "Prodrone (drone)"},
	{"ACSL", "ACSL (drone)"},
	{"ModalAI", "ModalAI (drone)"},
	{"Volatus", "Volatus (drone)"},
	{"BetaFPV", "BetaFPV (drone)"},
	{"Hubsan", "Hubsan (drone)"},
	{"FIMI", "FIMI (drone)"},
}

// knownDJIOUIs must all be present in fetched IEEE data or it's considered corrupt.
var knownDJIOUIs = []string{"60:60:1F", "34:D2:62", "E4:7A:2C", "48:1C:B9", "04:A8:5A"}

const (
	ieeeCSVURL       = "https://standards-oui.ieee.org/oui/oui.csv"
	wiresharkManufURL = "https://www.wireshark.org/download/automated/data/manuf"
	fetchTimeout     = 60 * time.Second
	minIEEERows      = 30000
	userAgent        = "skylens-intel-updater/1.0"
)

// droneModelsJSON represents the top-level structure of drone_models.json.
type droneModelsJSON struct {
	Version        string                       `json:"version"`
	Description    string                       `json:"description"`
	SerialPrefixes map[string]json.RawMessage    `json:"serial_prefixes"`
	DJIModelCodes  map[string]json.RawMessage    `json:"dji_model_codes"`
	OUIMap         map[string]string             `json:"oui_map"`
	DJISSIDModels  map[string]json.RawMessage    `json:"dji_ssid_models"`
	SSIDPatterns   json.RawMessage               `json:"ssid_patterns"`
}

// RunIntelUpdate fetches the IEEE OUI registry and updates drone_models.json
// and fingerprint.go with any new drone manufacturer OUIs.
func RunIntelUpdate(jsonPath, tapJsonPath, goPath string, dryRun bool) (*UpdateResult, error) {
	// Read current JSON
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("read drone_models.json: %w", err)
	}

	var models droneModelsJSON
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("parse drone_models.json: %w", err)
	}

	if models.OUIMap == nil {
		models.OUIMap = make(map[string]string)
	}

	// Build blocklist from _removed_ entries
	blocklist := buildBlocklist(models.OUIMap)

	result := &UpdateResult{
		PreviousVersion: models.Version,
		NewOUIs:         make(map[string]string),
		Timestamp:       time.Now().UTC(),
	}

	// Try IEEE CSV first, fall back to Wireshark manuf
	droneOUIs, source, err := fetchDroneOUIs()
	if err != nil {
		return nil, fmt.Errorf("fetch OUI data: %w", err)
	}
	result.Source = source

	// Find new OUIs not already in our map
	for oui, label := range droneOUIs {
		// Already exists?
		if _, exists := models.OUIMap[oui]; exists {
			continue
		}

		// Blocked?
		if blocklist[oui] {
			slog.Info("Skipping blocklisted OUI", "oui", oui, "label", label)
			continue
		}

		// Locally administered MAC?
		if isLocallyAdministeredOUI(oui) {
			slog.Warn("Skipping locally-administered OUI from IEEE", "oui", oui, "label", label)
			continue
		}

		result.NewOUIs[oui] = label
	}

	if len(result.NewOUIs) == 0 {
		result.NewVersion = models.Version
		slog.Info("Intel database up to date, no new OUIs found", "source", source, "version", models.Version)
		return result, nil
	}

	// Bump version
	result.NewVersion = bumpPatchVersion(models.Version)

	if dryRun {
		slog.Info("Dry run: would add new OUIs",
			"count", len(result.NewOUIs),
			"new_version", result.NewVersion,
			"source", source)
		for oui, label := range result.NewOUIs {
			slog.Info("  new OUI", "oui", oui, "label", label)
		}
		return result, nil
	}

	// Add new OUIs to model
	for oui, label := range result.NewOUIs {
		models.OUIMap[oui] = label
	}
	models.Version = result.NewVersion

	// Backup before write
	backupPath := jsonPath + ".bak"
	if err := copyFile(jsonPath, backupPath); err != nil {
		slog.Warn("Failed to create backup", "error", err)
	}

	// Write updated JSON atomically
	if err := writeJSONAtomic(jsonPath, &models); err != nil {
		return nil, fmt.Errorf("write drone_models.json: %w", err)
	}

	// Validate the written file
	if err := validateJSONFile(jsonPath); err != nil {
		// Restore backup
		if restoreErr := copyFile(backupPath, jsonPath); restoreErr != nil {
			slog.Error("Failed to restore backup after validation failure", "error", restoreErr)
		}
		return nil, fmt.Errorf("JSON validation failed after write: %w", err)
	}

	slog.Info("Updated drone_models.json",
		"version", result.NewVersion,
		"new_ouis", len(result.NewOUIs),
		"source", source)

	// Sync to TAP copy
	if tapJsonPath != "" {
		if err := copyFile(jsonPath, tapJsonPath); err != nil {
			slog.Warn("Failed to sync to TAP drone_models.json", "error", err)
		} else {
			slog.Info("Synced drone_models.json to TAP", "path", tapJsonPath)
		}
	}

	// Patch fingerprint.go
	if goPath != "" {
		if err := patchFingerprintGo(goPath, result.NewOUIs); err != nil {
			slog.Warn("Failed to patch fingerprint.go (JSON was still updated)", "error", err)
		} else {
			slog.Info("Patched fingerprint.go with new OUIs", "count", len(result.NewOUIs))
		}
	}

	return result, nil
}

// fetchDroneOUIs tries IEEE CSV, falls back to Wireshark manuf.
// Returns map of OUI -> label, source name, and error.
func fetchDroneOUIs() (map[string]string, string, error) {
	ouis, err := fetchIEEECSV()
	if err == nil {
		return ouis, "ieee", nil
	}
	slog.Warn("IEEE CSV fetch failed, trying Wireshark fallback", "error", err)

	ouis, err2 := fetchWiresharkManuf()
	if err2 == nil {
		return ouis, "wireshark", nil
	}

	return nil, "", fmt.Errorf("all sources failed: ieee=%v, wireshark=%v", err, err2)
}

// fetchIEEECSV downloads and parses the IEEE MA-L OUI registry CSV.
func fetchIEEECSV() (map[string]string, error) {
	req, err := http.NewRequest("GET", ieeeCSVURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/csv")

	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from IEEE", resp.StatusCode)
	}

	reader := csv.NewReader(resp.Body)
	reader.LazyQuotes = true

	// Read header
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read CSV header: %w", err)
	}
	_ = header // Registry,Assignment,Organization Name,Organization Address

	droneOUIs := make(map[string]string)
	totalRows := 0
	seenOUIs := make(map[string]bool) // for known OUI cross-check

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed rows
		}

		if len(record) < 3 {
			continue
		}

		// Only MA-L entries
		if record[0] != "MA-L" {
			continue
		}
		totalRows++

		assignment := strings.TrimSpace(record[1])
		orgName := strings.TrimSpace(record[2])

		// Validate assignment format: exactly 6 hex chars
		if len(assignment) != 6 || !isHexString(assignment) {
			continue
		}

		// Format as XX:XX:XX
		oui := formatOUI(assignment)
		seenOUIs[oui] = true

		// Match against our manufacturer table
		label := matchManufacturer(orgName)
		if label != "" {
			droneOUIs[oui] = label
		}
	}

	// Minimum threshold check
	if totalRows < minIEEERows {
		return nil, fmt.Errorf("IEEE data too small: %d rows (need %d+), likely truncated", totalRows, minIEEERows)
	}

	// Known OUI cross-check
	for _, knownOUI := range knownDJIOUIs {
		if !seenOUIs[knownOUI] {
			return nil, fmt.Errorf("known DJI OUI %s missing from IEEE data — download corrupt/truncated", knownOUI)
		}
	}

	slog.Info("Parsed IEEE CSV", "total_ma_l", totalRows, "drone_ouis_found", len(droneOUIs))
	return droneOUIs, nil
}

// fetchWiresharkManuf downloads and parses the Wireshark manufacturer database.
func fetchWiresharkManuf() (map[string]string, error) {
	req, err := http.NewRequest("GET", wiresharkManufURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Wireshark", resp.StatusCode)
	}

	droneOUIs := make(map[string]string)
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}

		ouiRaw := strings.TrimSpace(parts[0])
		fullName := strings.TrimSpace(parts[2])

		// OUI format in Wireshark: XX:XX:XX
		if len(ouiRaw) != 8 || ouiRaw[2] != ':' || ouiRaw[5] != ':' {
			continue
		}

		oui := strings.ToUpper(ouiRaw)
		label := matchManufacturer(fullName)
		if label != "" {
			droneOUIs[oui] = label
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading Wireshark data: %w", err)
	}

	if len(droneOUIs) == 0 {
		return nil, fmt.Errorf("no drone OUIs found in Wireshark data")
	}

	slog.Info("Parsed Wireshark manuf", "drone_ouis_found", len(droneOUIs))
	return droneOUIs, nil
}

// matchManufacturer checks if an org name matches any known drone manufacturer.
func matchManufacturer(orgName string) string {
	for _, m := range ieeeManufacturers {
		if strings.HasPrefix(orgName, m.Prefix) {
			return m.Label
		}
	}
	return ""
}

// buildBlocklist extracts OUI values from _removed_ entries.
func buildBlocklist(ouiMap map[string]string) map[string]bool {
	blocklist := make(map[string]bool)

	// _removed_ entries whose keys look like _removed_XX:XX:XX block that OUI directly
	ouiPattern := regexp.MustCompile(`^_removed_([0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2})$`)
	for key := range ouiMap {
		if m := ouiPattern.FindStringSubmatch(key); m != nil {
			blocklist[strings.ToUpper(m[1])] = true
		}
	}

	return blocklist
}

// isLocallyAdministeredOUI checks if an OUI has the locally-administered bit set.
// The second hex digit must NOT have bit 1 set (2,3,6,7,A,B,E,F).
func isLocallyAdministeredOUI(oui string) bool {
	if len(oui) < 2 {
		return false
	}
	second := strings.ToUpper(string(oui[1]))
	return second == "2" || second == "3" ||
		second == "6" || second == "7" ||
		second == "A" || second == "B" ||
		second == "E" || second == "F"
}

// isHexString checks if a string contains only hex characters.
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// formatOUI converts "60601F" to "60:60:1F".
func formatOUI(assignment string) string {
	upper := strings.ToUpper(assignment)
	if len(upper) != 6 {
		return upper
	}
	return upper[0:2] + ":" + upper[2:4] + ":" + upper[4:6]
}

// bumpPatchVersion increments the patch version: "2.7.0" -> "2.7.1".
func bumpPatchVersion(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return version + ".1"
	}

	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return version + ".1"
	}

	return parts[0] + "." + parts[1] + "." + strconv.Itoa(patch+1)
}

// writeJSONAtomic writes JSON to a temp file then renames it into place.
func writeJSONAtomic(path string, models *droneModelsJSON) error {
	data, err := json.MarshalIndent(models, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "drone_models_*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp to target: %w", err)
	}

	return nil
}

// validateJSONFile re-reads and unmarshals a JSON file to verify integrity.
func validateJSONFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("re-read: %w", err)
	}

	var check droneModelsJSON
	if err := json.Unmarshal(data, &check); err != nil {
		return fmt.Errorf("re-parse: %w", err)
	}

	// Verify essential keys are present
	if check.Version == "" {
		return fmt.Errorf("missing version field")
	}
	if len(check.OUIMap) == 0 {
		return fmt.Errorf("empty oui_map")
	}
	if check.SerialPrefixes == nil {
		return fmt.Errorf("missing serial_prefixes")
	}

	return nil
}

// copyFile copies src to dst.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// patchFingerprintGo inserts new OUI entries into fingerprint.go's ouiMap.
func patchFingerprintGo(goPath string, newOUIs map[string]string) error {
	data, err := os.ReadFile(goPath)
	if err != nil {
		return fmt.Errorf("read fingerprint.go: %w", err)
	}

	content := string(data)
	marker := "// SSID patterns for drone detection"
	idx := strings.Index(content, marker)
	if idx == -1 {
		return fmt.Errorf("marker %q not found in fingerprint.go — skipping Go patch", marker)
	}

	// Build sorted insertion block
	sortedOUIs := make([]string, 0, len(newOUIs))
	for oui := range newOUIs {
		sortedOUIs = append(sortedOUIs, oui)
	}
	sort.Strings(sortedOUIs)

	dateStr := time.Now().UTC().Format("2006-01-02")
	var block strings.Builder
	block.WriteString("\n\t// ===== AUTO-ADDED BY INTEL-UPDATER (" + dateStr + ") =====\n")
	for _, oui := range sortedOUIs {
		block.WriteString(fmt.Sprintf("\t%q: %q, // IEEE MA-L, auto-added %s\n", oui, newOUIs[oui], dateStr))
	}
	block.WriteString("\n")

	// Backup fingerprint.go
	backupPath := goPath + ".bak"
	if err := copyFile(goPath, backupPath); err != nil {
		slog.Warn("Failed to backup fingerprint.go", "error", err)
	}

	// Find the closing brace of ouiMap (the "}" + newline before the marker)
	// We insert right before the closing brace
	closingIdx := strings.LastIndex(content[:idx], "}")
	if closingIdx == -1 {
		return fmt.Errorf("could not find ouiMap closing brace before marker")
	}

	patched := content[:closingIdx] + block.String() + content[closingIdx:]

	// Write atomically
	dir := filepath.Dir(goPath)
	tmp, err := os.CreateTemp(dir, "fingerprint_*.go.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(patched); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()

	if err := os.Rename(tmpPath, goPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	slog.Info("Patched fingerprint.go", "new_entries", len(newOUIs))
	return nil
}

// GetDroneModelsVersion reads just the version field from drone_models.json.
func GetDroneModelsVersion(jsonPath string) string {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	return v.Version
}
