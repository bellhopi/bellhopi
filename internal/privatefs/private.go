package privatefs

import (
	"os"

	"github.com/enetx/g"
)

// MkdirAll protects newly created directories and validates existing ones.
func MkdirAll(path g.String) error {
	_, statErr := os.Stat(path.Std())
	if err := os.MkdirAll(path.Std(), 0700); err != nil {
		return err
	}
	if os.IsNotExist(statErr) {
		if err := protectDir(path); err != nil {
			return err
		}
	}
	f, err := os.Open(path.Std())
	if err != nil {
		return err
	}
	defer f.Close()
	return Check(f)
}
