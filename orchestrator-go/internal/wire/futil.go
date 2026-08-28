package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ensurePrivateDir creates dir with mode 0700 (no-op if it exists), matching
// core/src/futil.rs::ensure_private_dir.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create private dir %s: %w", dir, err)
	}
	// Ensure an existing dir is not world-readable.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod private dir %s: %w", dir, err)
	}
	return nil
}

// write0600Atomic writes data to path via a unique 0600-at-birth temp file +
// fsync + atomic rename, matching core/src/futil.rs::write_0600_atomic.
func write0600Atomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Born 0600.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// EnsurePrivateDir creates dir 0700 (exported for cross-package use).
func EnsurePrivateDir(dir string) error { return ensurePrivateDir(dir) }

// WriteJSON0600 writes doc as pretty JSON to path atomically with 0600.
func WriteJSON0600(path string, doc map[string]string) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return write0600Atomic(path, data)
}
