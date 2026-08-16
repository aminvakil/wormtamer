//go:build linux

package config

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	posixACLXattrName    = "system.posix_acl_access"
	posixACLXattrVersion = 2
	posixACLHeaderBytes  = 4
	posixACLEntryBytes   = 8
	maxPOSIXACLBytes     = 64 << 10

	posixACLUserObj  = 0x01
	posixACLUser     = 0x02
	posixACLGroupObj = 0x04
	posixACLGroup    = 0x08
	posixACLMask     = 0x10
	posixACLOther    = 0x20
	posixACLRead     = 0x04
	posixACLPermMask = 0x07
	posixACLNoID     = ^uint32(0)
)

func configFileBroadlyRead(file *os.File, info os.FileInfo) (bool, error) {
	permissions := info.Mode().Perm()
	if permissions&0o004 != 0 {
		return true, nil
	}
	acl, found, err := readPOSIXAccessACL(file)
	if err != nil {
		return false, fmt.Errorf("read POSIX access ACL: %w", err)
	}
	if !found {
		return permissions&0o044 != 0, nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("read Linux configuration owner")
	}
	broad, err := posixACLBroadlyReadable(acl, uint32(os.Geteuid()), stat.Uid)
	if err != nil {
		return false, fmt.Errorf("parse POSIX access ACL: %w", err)
	}
	return broad, nil
}

func readPOSIXAccessACL(file *os.File) ([]byte, bool, error) {
	size, err := unix.Fgetxattr(int(file.Fd()), posixACLXattrName, nil)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if size < posixACLHeaderBytes || size > maxPOSIXACLBytes {
		return nil, true, errors.New("invalid POSIX ACL size")
	}
	contents := make([]byte, size)
	read, err := unix.Fgetxattr(int(file.Fd()), posixACLXattrName, contents)
	if err != nil {
		return nil, true, err
	}
	if read < posixACLHeaderBytes || read > len(contents) {
		return nil, true, errors.New("invalid POSIX ACL read size")
	}
	return contents[:read], true, nil
}

type posixACLNamedEntry struct {
	id         uint32
	permission uint16
}

func posixACLBroadlyReadable(contents []byte, currentUID, ownerUID uint32) (bool, error) {
	if len(contents) < posixACLHeaderBytes || (len(contents)-posixACLHeaderBytes)%posixACLEntryBytes != 0 {
		return false, errors.New("invalid POSIX ACL encoding")
	}
	if binary.LittleEndian.Uint32(contents[:posixACLHeaderBytes]) != posixACLXattrVersion {
		return false, errors.New("unsupported POSIX ACL version")
	}

	var userObjSeen, groupObjSeen, maskSeen, otherSeen bool
	var groupObjPerm, maskPerm, otherPerm uint16
	namedUsers := make([]posixACLNamedEntry, 0)
	namedGroups := make([]posixACLNamedEntry, 0)
	seenUserIDs := make(map[uint32]struct{})
	seenGroupIDs := make(map[uint32]struct{})
	for offset := posixACLHeaderBytes; offset < len(contents); offset += posixACLEntryBytes {
		tag := binary.LittleEndian.Uint16(contents[offset : offset+2])
		permission := binary.LittleEndian.Uint16(contents[offset+2 : offset+4])
		id := binary.LittleEndian.Uint32(contents[offset+4 : offset+8])
		if permission&^uint16(posixACLPermMask) != 0 {
			return false, errors.New("invalid POSIX ACL permission")
		}
		switch tag {
		case posixACLUserObj:
			if userObjSeen || id != posixACLNoID {
				return false, errors.New("invalid POSIX ACL owner entry")
			}
			userObjSeen = true
		case posixACLUser:
			if id != posixACLNoID {
				if _, exists := seenUserIDs[id]; exists {
					return false, errors.New("duplicate POSIX ACL user entry")
				}
				seenUserIDs[id] = struct{}{}
			}
			namedUsers = append(namedUsers, posixACLNamedEntry{id: id, permission: permission})
		case posixACLGroupObj:
			if groupObjSeen || id != posixACLNoID {
				return false, errors.New("invalid POSIX ACL group owner entry")
			}
			groupObjSeen = true
			groupObjPerm = permission
		case posixACLGroup:
			if id != posixACLNoID {
				if _, exists := seenGroupIDs[id]; exists {
					return false, errors.New("duplicate POSIX ACL group entry")
				}
				seenGroupIDs[id] = struct{}{}
			}
			namedGroups = append(namedGroups, posixACLNamedEntry{id: id, permission: permission})
		case posixACLMask:
			if maskSeen || id != posixACLNoID {
				return false, errors.New("invalid POSIX ACL mask entry")
			}
			maskSeen = true
			maskPerm = permission
		case posixACLOther:
			if otherSeen || id != posixACLNoID {
				return false, errors.New("invalid POSIX ACL other entry")
			}
			otherSeen = true
			otherPerm = permission
		default:
			return false, fmt.Errorf("unknown POSIX ACL tag %d", tag)
		}
	}
	if !userObjSeen || !groupObjSeen || !otherSeen {
		return false, errors.New("incomplete POSIX ACL")
	}
	if (len(namedUsers) > 0 || len(namedGroups) > 0) && !maskSeen {
		return false, errors.New("POSIX ACL is missing a mask")
	}

	effectiveMask := uint16(posixACLPermMask)
	if maskSeen {
		effectiveMask = maskPerm
	}
	if groupObjPerm&effectiveMask&posixACLRead != 0 || otherPerm&posixACLRead != 0 {
		return true, nil
	}
	for _, entry := range namedUsers {
		if entry.id != currentUID && entry.id != ownerUID && entry.permission&effectiveMask&posixACLRead != 0 {
			return true, nil
		}
	}
	for _, entry := range namedGroups {
		if entry.permission&effectiveMask&posixACLRead != 0 {
			return true, nil
		}
	}
	return false, nil
}
