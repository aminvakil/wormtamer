//go:build !linux

package repository

import (
	"os"
	"path/filepath"
)

func secureCreateSpool(directory, name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func secureRemoveSpool(directory, name string) error {
	return os.Remove(filepath.Join(directory, name))
}
