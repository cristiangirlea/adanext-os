package source

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
)

const (
	modeTypeMask = 0o170000
	modeSymlink  = 0o120000
	modeGitlink  = 0o160000
	modeDir      = 0o040000
)

type indexEntry struct {
	path        string
	mode        uint32
	oid         []byte
	stage       int
	assumeValid bool
	skipWork    bool
	intentToAdd bool
}

type index struct {
	version    uint32
	entries    []indexEntry
	extensions []string // signatures in file order
}

func (x *index) has(ext string) bool {
	for _, e := range x.extensions {
		if e == ext {
			return true
		}
	}
	return false
}

var errIndexChecksum = errors.New("index checksum mismatch")

// parseIndex reads an index of version 2, 3 or 4 whose object IDs are
// hashSize bytes, verifying its trailing checksum unless git skipped it.
func parseIndex(data []byte, hashSize int) (*index, error) {
	if len(data) < 12+hashSize || string(data[:4]) != "DIRC" {
		return nil, errors.New("not an index file")
	}
	body, trailer := data[:len(data)-hashSize], data[len(data)-hashSize:]
	if !bytes.Equal(trailer, make([]byte, hashSize)) {
		var h hash.Hash = sha1.New()
		if hashSize == sha256.Size {
			h = sha256.New()
		}
		h.Write(body)
		if !bytes.Equal(h.Sum(nil), trailer) {
			return nil, errIndexChecksum
		}
	}
	x := &index{version: binary.BigEndian.Uint32(data[4:8])}
	if x.version < 2 || x.version > 4 {
		return nil, fmt.Errorf("unsupported index version %d", x.version)
	}
	count := binary.BigEndian.Uint32(data[8:12])
	pos := 12
	var previous string
	short := errors.New("index truncated")
	for n := uint32(0); n < count; n++ {
		start := pos
		fixed := 40 + hashSize + 2
		if pos+fixed > len(body) {
			return nil, short
		}
		e := indexEntry{mode: binary.BigEndian.Uint32(body[pos+24 : pos+28])}
		e.oid = append([]byte(nil), body[pos+40:pos+40+hashSize]...)
		flags := binary.BigEndian.Uint16(body[pos+40+hashSize:])
		e.assumeValid = flags&0x8000 != 0
		e.stage = int(flags>>12) & 3
		pos += fixed
		if flags&0x4000 != 0 {
			if x.version < 3 || pos+2 > len(body) {
				return nil, errors.New("bad extended flags")
			}
			extended := binary.BigEndian.Uint16(body[pos:])
			e.skipWork = extended&0x4000 != 0
			e.intentToAdd = extended&0x2000 != 0
			pos += 2
		}
		if x.version == 4 {
			strip, used := readOffsetVarint(body[pos:])
			if used == 0 || strip > uint64(len(previous)) {
				return nil, errors.New("bad path compression")
			}
			pos += used
			end := bytes.IndexByte(body[pos:], 0)
			if end < 0 {
				return nil, short
			}
			e.path = previous[:len(previous)-int(strip)] + string(body[pos:pos+end])
			pos += end + 1
		} else {
			end := bytes.IndexByte(body[pos:], 0)
			if end < 0 {
				return nil, short
			}
			e.path = string(body[pos : pos+end])
			// Entries are padded with 1 to 8 NULs to a multiple of 8.
			pos = start + (pos-start+end+8)&^7
			if pos > len(body) {
				return nil, short
			}
		}
		previous = e.path
		x.entries = append(x.entries, e)
	}
	for pos+8 <= len(body) {
		size := int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
		if size < 0 || pos+8+size > len(body) {
			return nil, errors.New("bad index extension")
		}
		x.extensions = append(x.extensions, string(body[pos:pos+4]))
		pos += 8 + size
	}
	if pos != len(body) {
		return nil, errors.New("bad index extension")
	}
	return x, nil
}

// readOffsetVarint is git's offset varint; used is 0 on malformed input.
func readOffsetVarint(b []byte) (value uint64, used int) {
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	value = uint64(c & 0x7f)
	used = 1
	for c&0x80 != 0 {
		if used >= len(b) || used > 9 {
			return 0, 0
		}
		c = b[used]
		used++
		value = ((value + 1) << 7) | uint64(c&0x7f)
	}
	return value, used
}
