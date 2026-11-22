package store

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDBPath returns the platform-standard path for Faro's global SQLite catalog:
//   - FARO_DB_PATH env var if set
//   - macOS:   ~/Library/Application Support/faro/catalog.db
//   - Windows: %LOCALAPPDATA%\faro\catalog.db
//   - Linux:   $XDG_DATA_HOME/faro/catalog.db (default ~/.local/share/faro/catalog.db)
func DefaultDBPath() (string, error) {
	if override := os.Getenv("FARO_DB_PATH"); override != "" {
		return filepath.Clean(override), nil
	}

	switch runtime.GOOS {
	case "darwin":
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "faro", "catalog.db"), nil

	case "windows":
		base, err := os.UserCacheDir()
		if err != nil {
			base, err = os.UserConfigDir()
			if err != nil {
				return "", err
			}
		}
		return filepath.Join(base, "faro", "catalog.db"), nil

	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "faro", "catalog.db"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "faro", "catalog.db"), nil
	}
}
