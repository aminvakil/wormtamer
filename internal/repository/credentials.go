package repository

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ValidateCredentialBoundary rejects known service-private paths that the review-tool identity can access.
func ValidateCredentialBoundary(toolUID, toolGID uint32, privateFiles, privateDirectories []string) error {
	for _, directory := range privateDirectories {
		accessible, err := identityCanAccess(directory, toolUID, toolGID, 0o7)
		if err != nil {
			return fmt.Errorf("inspect private directory permissions: %w", err)
		}
		if accessible {
			return fmt.Errorf("review-tool identity can access service-private directory %s", directory)
		}
	}
	for _, path := range privateFiles {
		if path == "" {
			continue
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect private file: %w", err)
		}
		accessible, err := identityCanAccess(path, toolUID, toolGID, 0o6)
		if err != nil {
			return fmt.Errorf("inspect private file permissions: %w", err)
		}
		if accessible {
			return fmt.Errorf("review-tool identity can read or write service-private file %s", path)
		}
	}
	return nil
}

func identityCanAccess(path string, uid, gid uint32, requested os.FileMode) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		absolute = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	parts, err := filepath.Rel(current, absolute)
	if err != nil {
		return false, err
	}
	components := splitPathComponents(parts)
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		permission := effectivePermission(info, uid, gid)
		if index < len(components)-1 && permission&0o1 == 0 {
			return false, nil
		}
		if index == len(components)-1 && permission&requested != 0 {
			return true, nil
		}
	}
	return false, nil
}

func splitPathComponents(path string) []string {
	components := make([]string, 0)
	for path != "." && path != "" {
		directory, file := filepath.Split(path)
		if file != "" {
			components = append([]string{file}, components...)
		}
		path = filepath.Clean(directory)
		path = filepath.Clean(path)
		if path == string(filepath.Separator) {
			break
		}
	}
	return components
}

func effectivePermission(info os.FileInfo, uid, gid uint32) os.FileMode {
	mode := info.Mode().Perm()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode & 0o7
	}
	if stat.Uid == uid {
		return (mode >> 6) & 0o7
	}
	if stat.Gid == gid {
		return (mode >> 3) & 0o7
	}
	return mode & 0o7
}
