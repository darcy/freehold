package state

import (
	"os"
	"path/filepath"
)

// ensurePrivateDir asserts the CP state root's mode: 0711 — traverse-only,
// never listable. Sandboxed capability doors run as an unprivileged user and
// must TRAVERSE this dir to reach their own package dir (<state>/runner/<name>,
// 0700 owned by that user): at 0700 a running door cannot append its audit log
// (the containment guarantee) or re-read its identity, and a healthy unit is
// never restarted by systemd, so the loss would be silent and steady-state.
// Re-asserted on EVERY open — routine console traffic (overview, settings,
// door flows) re-opens state continuously, so a 0700 assertion here strips
// traversal out from under live doors. Contents stay protected by their own
// modes: state.json 0600, world-secrets 0700/0600, console identity 0600,
// per-door package dirs 0700. (The Rust mirror tightens only the runner's own
// package dir, which the door user owns.)
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return err
	}
	return os.Chmod(dir, 0o711)
}

// write0600Atomic writes data to path via a unique 0600-at-birth temp file +
// fsync + atomic rename — matching core/src/futil.rs::write_0600_atomic.
func write0600Atomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
