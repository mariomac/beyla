package webhook

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/mod/semver"

	"github.com/grafana/beyla/v3/pkg/beyla"
)

type InstrumentationManager struct {
	logger *slog.Logger
	cfg    *beyla.Config
}

func NewInstrumentationManager(cfg *beyla.Config) *InstrumentationManager {
	return &InstrumentationManager{
		logger: slog.With("component", "webhook.InstrumentationManager"),
		cfg:    cfg,
	}
}

// cleanupOldInstrumentationVersions removes instrumentation directories
// older than the specified minimum version
func (i *InstrumentationManager) cleanupOldInstrumentationVersions(instrumentDir string, minVersion string) error {
	if !semver.IsValid(minVersion) {
		return fmt.Errorf("invalid minimum version: %s", minVersion)
	}

	entries, err := os.ReadDir(instrumentDir)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", instrumentDir, err)
	}

	i.logger.Debug("found SDK versions", "entries", entries)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		version := entry.Name()

		// Skip if the directory not a valid semver in the instrumentation volume
		if !semver.IsValid(version) {
			i.logger.Debug("ignoring directory in the instrumentation path", "dir", entry.Name())
			continue
		}

		if semver.Compare(version, minVersion) < 0 {
			dirPath := filepath.Join(instrumentDir, entry.Name())
			if err := os.RemoveAll(dirPath); err != nil {
				return fmt.Errorf("failed to remove directory %s: %w", dirPath, err)
			}
			i.logger.Info("removed old instrumentation", "version", entry.Name())
		}
	}

	return nil
}
