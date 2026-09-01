// Package fsutil holds small filesystem helpers shared by the packages that
// persist user data.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path via a same-directory temp file and a
// rename, so a reader (or a crash) either sees the old file or the new one,
// never a truncated mix. os.WriteFile truncates before writing, which is
// fine for a one-shot CLI but not for files the web UI rewrites on ordinary
// clicks: a process killed mid-write leaves a partial brokers.yaml that
// either fails to parse or - worse - still parses, silently short of every
// entry after the cut.
//
// Same directory matters: rename is only atomic within one filesystem.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("failed to flush temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	// CreateTemp always makes the file 0600; set the caller's mode before
	// the rename so the published file never has the wrong permissions.
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}
