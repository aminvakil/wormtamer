//go:build !linux

package config

import "os"

func configFileBroadlyRead(_ *os.File, info os.FileInfo) (bool, error) {
	return info.Mode().Perm()&0o044 != 0, nil
}
