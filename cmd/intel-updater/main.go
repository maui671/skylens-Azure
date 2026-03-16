package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/K13094/skylens/internal/intel"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "Show what would change without writing")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	gitCommit := flag.Bool("git", true, "Auto-commit changes to git")
	jsonPath := flag.String("json-path", "", "Override drone_models.json path")
	goPath := flag.String("go-path", "", "Override fingerprint.go path")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Resolve paths relative to working directory
	wd, err := os.Getwd()
	if err != nil {
		slog.Error("Failed to get working directory", "error", err)
		os.Exit(1)
	}

	jp := *jsonPath
	if jp == "" {
		jp = filepath.Join(wd, "internal", "intel", "drone_models.json")
	}

	tapJP := filepath.Join(wd, "tap", "intel", "drone_models.json")
	if _, err := os.Stat(tapJP); os.IsNotExist(err) {
		tapJP = "" // TAP copy doesn't exist, skip sync
	}

	gp := *goPath
	if gp == "" {
		gp = filepath.Join(wd, "internal", "intel", "fingerprint.go")
	}

	slog.Info("Starting intel update",
		"json_path", jp,
		"tap_json_path", tapJP,
		"go_path", gp,
		"dry_run", *dryRun)

	result, err := intel.RunIntelUpdate(jp, tapJP, gp, *dryRun)
	if err != nil {
		slog.Error("Intel update failed", "error", err)
		os.Exit(1)
	}

	if len(result.NewOUIs) == 0 {
		slog.Info("No changes needed", "version", result.PreviousVersion)
		return
	}

	slog.Info("Update complete",
		"previous_version", result.PreviousVersion,
		"new_version", result.NewVersion,
		"new_ouis", len(result.NewOUIs),
		"source", result.Source)

	for oui, label := range result.NewOUIs {
		fmt.Printf("  + %s → %s\n", oui, label)
	}

	// Git commit if enabled and not dry-run
	if *gitCommit && !*dryRun && len(result.NewOUIs) > 0 {
		files := []string{
			"internal/intel/drone_models.json",
			"internal/intel/fingerprint.go",
		}
		if tapJP != "" {
			files = append(files, "tap/intel/drone_models.json")
		}

		addCmd := exec.Command("git", append([]string{"add"}, files...)...)
		addCmd.Dir = wd
		if out, err := addCmd.CombinedOutput(); err != nil {
			slog.Warn("git add failed", "error", err, "output", strings.TrimSpace(string(out)))
		} else {
			msg := fmt.Sprintf("intel: auto-update drone_models.json v%s (+%d OUIs)", result.NewVersion, len(result.NewOUIs))
			commitCmd := exec.Command("git", "commit", "-m", msg)
			commitCmd.Dir = wd
			if out, err := commitCmd.CombinedOutput(); err != nil {
				slog.Warn("git commit failed", "error", err, "output", strings.TrimSpace(string(out)))
			} else {
				slog.Info("Git commit created", "message", msg)
			}
		}
	}
}
