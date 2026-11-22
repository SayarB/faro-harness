package store

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDataDir returns Faro's platform-standard application data directory:
//   - FARO_DATA_DIR env var if set
//   - parent directory of FARO_DB_PATH if set
//   - macOS:   ~/Library/Application Support/faro
//   - Windows: %LOCALAPPDATA%\faro
//   - Linux:   $XDG_DATA_HOME/faro (default ~/.local/share/faro)
func DefaultDataDir() (string, error) {
	if override := os.Getenv("FARO_DATA_DIR"); override != "" {
		return filepath.Clean(override), nil
	}
	if dbOverride := os.Getenv("FARO_DB_PATH"); dbOverride != "" {
		return filepath.Dir(filepath.Clean(dbOverride)), nil
	}

	switch runtime.GOOS {
	case "darwin":
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "faro"), nil

	case "windows":
		base, err := os.UserCacheDir()
		if err != nil {
			base, err = os.UserConfigDir()
			if err != nil {
				return "", err
			}
		}
		return filepath.Join(base, "faro"), nil

	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "faro"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "faro"), nil
	}
}

// DefaultDBPath returns the platform-standard path for Faro's global SQLite catalog.
func DefaultDBPath() (string, error) {
	if override := os.Getenv("FARO_DB_PATH"); override != "" {
		return filepath.Clean(override), nil
	}
	dir, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "catalog.db"), nil
}

// DefaultBotsDir returns the directory where bot home folders live.
func DefaultBotsDir() (string, error) {
	if override := os.Getenv("FARO_BOTS_DIR"); override != "" {
		return filepath.Clean(override), nil
	}
	dir, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bots"), nil
}
