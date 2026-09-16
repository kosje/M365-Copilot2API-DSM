package web

import (
	"os"
	"path/filepath"
	"time"
)

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// staleTmpAge is how old a leftover temp file must be before it is swept.
//
// The glob below cannot distinguish an abandoned temp file from one a concurrent
// writer created moments ago, and removing the latter makes that writer's
// os.Rename fail. Age is the cheap discriminator: a live write cannot be an hour
// old.
const staleTmpAge = time.Hour

func cleanupStaleTmp(path string) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	cutoff := time.Now().Add(-staleTmpAge)
	for _, pat := range []string{filepath.Join(dir, "."+base+".tmp.*"), filepath.Join(dir, base+".tmp.*")} {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			_ = os.Remove(m)
		}
	}
	if fi, err := os.Stat(path + ".tmp"); err == nil && fi.ModTime().Before(cutoff) {
		_ = os.Remove(path + ".tmp")
	}
}

// writeFileAtomic persists b to path durably: tmp random -> fsync(file) -> close -> rename -> fsync(dir).
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cleanupStaleTmp(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	_ = fsyncDir(dir)
	return nil
}
