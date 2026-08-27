//go:build linux

package repository

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func secureCreateSpool(directory, name string) (*os.File, error) {
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(directoryFD)
	fileFD, err := unix.Openat(directoryFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fileFD), name)
	if file == nil {
		unix.Close(fileFD)
		return nil, errors.New("create command-output file handle")
	}
	return file, nil
}

func secureRemoveSpool(directory, name string) error {
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(directoryFD)
	return unix.Unlinkat(directoryFD, name, 0)
}
