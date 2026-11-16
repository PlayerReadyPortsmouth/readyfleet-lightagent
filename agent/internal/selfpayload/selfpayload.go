// Package selfpayload implements the one piece of the self-contained-exe
// trailer format that light-installer and managed-installer both need
// identically: reading a uint32-LE length prefix followed by that many
// bytes. Everything else about a trailer — the magic marker used to find
// it (each installer has its own, and each must search with
// bytes.LastIndex, not Index: a compiled installer contains its own magic
// constant as a Go string literal in .rodata, so the first match is the
// binary's own copy of the constant, not the real trailer appended after
// build), how many length-prefixed sections follow it, and what those
// sections mean — is specific to each installer and stays in that
// installer's own package. Only this length-prefix reader is identical
// enough between them to be worth sharing: duplicating it verbatim in two
// places is exactly the kind of thing that silently drifts.
package selfpayload

import (
	"encoding/binary"
	"errors"
)

// ReadLenPrefixed reads a uint32 little-endian length prefix at offset in
// data, then that many following bytes. It returns the section's bytes and
// the offset immediately after them (so callers can chain calls to read
// several sections in sequence). Every bound is checked against len(data);
// a corrupt or truncated trailer produces an error, never a panic or an
// out-of-range read.
func ReadLenPrefixed(data []byte, offset int) ([]byte, int, error) {
	if offset < 0 || offset+4 > len(data) {
		return nil, 0, errors.New("truncated length prefix")
	}
	n := int(binary.LittleEndian.Uint32(data[offset:]))
	offset += 4
	if n < 0 || offset+n > len(data) {
		return nil, 0, errors.New("length prefix out of range")
	}
	return data[offset : offset+n], offset + n, nil
}

// ContentEnd returns the offset where a file's own content ends: the start of
// its Authenticode certificate table when data is a signed PE whose table sits
// at the very end of the file (as signtool/osslsigncode always place it), and
// len(data) otherwise. Signing a self-contained installer happens AFTER its
// trailer is appended, so the signature follows the trailer; a parser that
// insists the trailer ends at EOF must measure against ContentEnd, not
// len(data). Signing also pads the content up to an 8-byte boundary before the
// table, so callers should allow up to 7 zero bytes between a trailer's end and
// ContentEnd (see PaddingOnly).
//
// Every read is bounds-checked; anything that is not a well-formed PE with an
// end-of-file certificate table yields len(data), never an error or a panic.
func ContentEnd(data []byte) int {
	if len(data) < 0x40 || data[0] != 'M' || data[1] != 'Z' {
		return len(data)
	}
	pe := int(binary.LittleEndian.Uint32(data[0x3C:]))
	if pe < 0 || pe+24 > len(data) || string(data[pe:pe+4]) != "PE\x00\x00" {
		return len(data)
	}
	opt := pe + 24
	if opt+2 > len(data) {
		return len(data)
	}
	var dirs int
	switch binary.LittleEndian.Uint16(data[opt:]) {
	case 0x10b: // PE32
		dirs = opt + 96
	case 0x20b: // PE32+
		dirs = opt + 112
	default:
		return len(data)
	}
	const securityDir = 4 // IMAGE_DIRECTORY_ENTRY_SECURITY; its address is a FILE offset
	entry := dirs + securityDir*8
	if entry+8 > len(data) {
		return len(data)
	}
	off := int(binary.LittleEndian.Uint32(data[entry:]))
	size := int(binary.LittleEndian.Uint32(data[entry+4:]))
	if off <= 0 || size <= 0 || off+size != len(data) {
		return len(data)
	}
	return off
}

// PaddingOnly reports whether data[from:to] is at most 7 bytes, all zero: the
// alignment padding a signer inserts before the certificate table.
func PaddingOnly(data []byte, from, to int) bool {
	if from > to || to-from > 7 || to > len(data) {
		return false
	}
	for _, b := range data[from:to] {
		if b != 0 {
			return false
		}
	}
	return true
}
