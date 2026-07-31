// Package atomicfile writes files so that a crash or power cut never leaves a
// half-written one behind.
//
// Both files this bridge cannot afford to lose — the OAuth token and the
// accessory-id registry — go through here. On a Raspberry Pi booting from an
// SD card, an unclean shutdown mid-write is a routine event, and either file
// arriving truncated means a HomeKit setup that has to be rebuilt by hand.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes data to path atomically.
//
// The temp file lives in the destination directory so the rename stays within
// one filesystem, where it is atomic. The file is fsynced before the rename
// and the directory afterwards, so the rename itself is durable.
func Write(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := writeSync(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return SyncDir(dir)
}

// WriteWithBackup is Write, preserving the previous contents at path+".bak"
// first. A reader that finds the primary missing or corrupt can fall back to
// the backup, which is what makes a crash mid-write recoverable rather than
// merely non-corrupting.
func WriteWithBackup(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// Stage the new contents before touching anything that exists.
	tmp := path + ".tmp"
	if err := writeSync(tmp, data, perm); err != nil {
		return err
	}

	// Preserve the outgoing version. A failure here is not fatal: the new
	// contents are already staged and the primary is still intact.
	if existing, err := os.ReadFile(path); err == nil {
		backupTmp := path + ".bak.tmp"
		if err := writeSync(backupTmp, existing, perm); err == nil {
			if err := os.Rename(backupTmp, path+".bak"); err != nil {
				os.Remove(backupTmp)
			}
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return SyncDir(dir)
}

// writeSync creates path with the given contents and fsyncs it.
func writeSync(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// SyncDir fsyncs a directory so that a rename inside it is durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
