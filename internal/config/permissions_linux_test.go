//go:build linux

package config

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

type posixACLTestEntry struct {
	tag        uint16
	permission uint16
	id         uint32
}

func TestPOSIXACLBroadlyReadable(t *testing.T) {
	currentUID := uint32(os.Geteuid())
	ownerUID := currentUID + 1
	unrelatedUID := currentUID + 2
	tests := []struct {
		name    string
		entries []posixACLTestEntry
		want    bool
	}{
		{
			name: "owner only",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
		},
		{
			name: "current process user",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLUser, 4, currentUID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
		},
		{
			name: "owner named user",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLUser, 4, ownerUID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
		},
		{
			name: "unrelated user",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLUser, 4, unrelatedUID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
			want: true,
		},
		{
			name: "unrelated user masked",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLUser, 4, unrelatedUID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLMask, 2, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
		},
		{
			name: "namespace-unmapped user",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLUser, 4, posixACLNoID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
			want: true,
		},
		{
			name: "namespace-unmapped group",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLGroup, 4, posixACLNoID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
			want: true,
		},
		{
			name: "owning group",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLGroupObj, 4, posixACLNoID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
			want: true,
		},
		{
			name: "named group",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLGroup, 4, unrelatedUID},
				{posixACLMask, 4, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
			want: true,
		},
		{
			name: "other users",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLOther, 4, posixACLNoID},
			},
			want: true,
		},
		{
			name: "unrelated user write only",
			entries: []posixACLTestEntry{
				{posixACLUserObj, 6, posixACLNoID},
				{posixACLUser, 2, unrelatedUID},
				{posixACLGroupObj, 0, posixACLNoID},
				{posixACLMask, 6, posixACLNoID},
				{posixACLOther, 0, posixACLNoID},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broad, err := posixACLBroadlyReadable(posixACLTestData(test.entries...), currentUID, ownerUID)
			if err != nil || broad != test.want {
				t.Fatalf("posixACLBroadlyReadable() = %t, %v; want %t", broad, err, test.want)
			}
		})
	}
}

func TestPOSIXACLBroadlyReadableRejectsMalformedACL(t *testing.T) {
	validEntries := []posixACLTestEntry{
		{posixACLUserObj, 6, posixACLNoID},
		{posixACLUser, 4, 1001},
		{posixACLGroupObj, 0, posixACLNoID},
		{posixACLMask, 4, posixACLNoID},
		{posixACLOther, 0, posixACLNoID},
	}
	invalidVersion := posixACLTestData(validEntries...)
	binary.LittleEndian.PutUint32(invalidVersion[:4], 3)
	tests := []struct {
		name     string
		contents []byte
	}{
		{name: "invalid version", contents: invalidVersion},
		{name: "truncated", contents: posixACLTestData(validEntries...)[:10]},
		{name: "missing mask", contents: posixACLTestData(
			posixACLTestEntry{posixACLUserObj, 6, posixACLNoID},
			posixACLTestEntry{posixACLUser, 4, 1001},
			posixACLTestEntry{posixACLGroupObj, 0, posixACLNoID},
			posixACLTestEntry{posixACLOther, 0, posixACLNoID},
		)},
		{name: "unknown tag", contents: posixACLTestData(
			posixACLTestEntry{posixACLUserObj, 6, posixACLNoID},
			posixACLTestEntry{0x40, 0, posixACLNoID},
			posixACLTestEntry{posixACLGroupObj, 0, posixACLNoID},
			posixACLTestEntry{posixACLOther, 0, posixACLNoID},
		)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := posixACLBroadlyReadable(test.contents, uint32(os.Geteuid()), uint32(os.Geteuid())); err == nil {
				t.Fatal("posixACLBroadlyReadable() accepted malformed ACL")
			}
		})
	}
}

func TestConfigFileBroadlyReadPropagatesACLInspectionFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "config-*")
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	broad, err := configFileBroadlyRead(file, info)
	if broad || !errors.Is(err, unix.EBADF) {
		t.Fatalf("configFileBroadlyRead() = %t, %v; want EBADF", broad, err)
	}
}

func posixACLTestData(entries ...posixACLTestEntry) []byte {
	contents := make([]byte, posixACLHeaderBytes+len(entries)*posixACLEntryBytes)
	binary.LittleEndian.PutUint32(contents[:posixACLHeaderBytes], posixACLXattrVersion)
	for index, entry := range entries {
		offset := posixACLHeaderBytes + index*posixACLEntryBytes
		binary.LittleEndian.PutUint16(contents[offset:offset+2], entry.tag)
		binary.LittleEndian.PutUint16(contents[offset+2:offset+4], entry.permission)
		binary.LittleEndian.PutUint32(contents[offset+4:offset+8], entry.id)
	}
	return contents
}
