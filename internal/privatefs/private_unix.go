//go:build !windows

package privatefs

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/enetx/g"
)

func Check(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("private permissions required: chmod 600 files or 700 directories")
	}
	return nil
}

func protectDir(path g.String) error { return os.Chmod(path.Std(), 0700) }

func Create(path g.String) (*os.File, error) {
	return os.OpenFile(path.Std(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

func Replace(from, to g.String) error {
	if err := os.Rename(from.Std(), to.Std()); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to.Std()))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
