package desktop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// settings is persisted as JSON in the Faro data directory.
type settings struct {
	APIKey string `json:"apiKey,omitempty"`
}

func settingsPath(dataDir string) string { return filepath.Join(dataDir, "settings.json") }

func loadSettings(dataDir string) (settings, error) {
	var s settings
	b, err := os.ReadFile(settingsPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return settings{}, err
	}
	s.APIKey = strings.TrimSpace(s.APIKey)
	return s, nil
}

// saveSettings writes the file with 0600 permissions. The key is stored in plain text:
// it is protected only by file permissions, not encrypted.
func saveSettings(dataDir string, s settings) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, "settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), settingsPath(dataDir))
}
