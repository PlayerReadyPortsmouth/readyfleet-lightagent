package selfpayload

import (
	"encoding/binary"
	"testing"
)

func lenPrefixed(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

func TestReadLenPrefixed_ReadsSectionAndReturnsNextOffset(t *testing.T) {
	section := []byte("agent bytes here")
	data := append([]byte("prefix junk"), lenPrefixed(section)...)
	data = append(data, []byte("trailing bytes")...)

	got, next, err := ReadLenPrefixed(data, len("prefix junk"))
	if err != nil {
		t.Fatalf("ReadLenPrefixed: %v", err)
	}
	if string(got) != string(section) {
		t.Errorf("section = %q, want %q", got, section)
	}
	if want := len("prefix junk") + 4 + len(section); next != want {
		t.Errorf("next offset = %d, want %d", next, want)
	}
}

func TestReadLenPrefixed_ChainsMultipleSections(t *testing.T) {
	first := []byte("first section")
	second := []byte("second section, longer")
	var data []byte
	data = append(data, lenPrefixed(first)...)
	data = append(data, lenPrefixed(second)...)

	gotFirst, pos, err := ReadLenPrefixed(data, 0)
	if err != nil {
		t.Fatalf("first ReadLenPrefixed: %v", err)
	}
	if string(gotFirst) != string(first) {
		t.Fatalf("first = %q, want %q", gotFirst, first)
	}
	gotSecond, _, err := ReadLenPrefixed(data, pos)
	if err != nil {
		t.Fatalf("second ReadLenPrefixed: %v", err)
	}
	if string(gotSecond) != string(second) {
		t.Fatalf("second = %q, want %q", gotSecond, second)
	}
}

func TestReadLenPrefixed_FailsSafeOnTruncatedPrefix(t *testing.T) {
	// Only 2 bytes of the 4-byte length prefix are present.
	data := []byte{0x05, 0x00}
	if _, _, err := ReadLenPrefixed(data, 0); err == nil {
		t.Fatal("expected an error for a truncated length prefix, got nil")
	}
}

func TestReadLenPrefixed_FailsSafeOnOversizedLength(t *testing.T) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 0xFFFFFFF0) // wildly larger than the buffer
	if _, _, err := ReadLenPrefixed(data, 0); err == nil {
		t.Fatal("expected an error for an out-of-range length prefix, got nil")
	}
}

func TestReadLenPrefixed_FailsSafeOnNegativeOffset(t *testing.T) {
	data := lenPrefixed([]byte("whatever"))
	if _, _, err := ReadLenPrefixed(data, -1); err == nil {
		t.Fatal("expected an error for a negative offset, got nil")
	}
}

// fakePE returns a minimal PE32+ header whose security data directory can be
// patched by setSecurity; enough for ContentEnd, which reads nothing else.
func fakePE() []byte {
	h := make([]byte, 0x200)
	h[0], h[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(h[0x3C:], 0x80)
	copy(h[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(h[0x80+24:], 0x20b)
	return h
}

func setSecurity(file []byte, off, size int) {
	entry := 0x80 + 24 + 112 + 4*8
	binary.LittleEndian.PutUint32(file[entry:], uint32(off))
	binary.LittleEndian.PutUint32(file[entry+4:], uint32(size))
}

func TestContentEnd(t *testing.T) {
	content := append(fakePE(), []byte("installer body + trailer")...)
	cert := []byte("certificate table bytes")

	signed := append(append([]byte{}, content...), cert...)
	setSecurity(signed, len(content), len(cert))
	if got := ContentEnd(signed); got != len(content) {
		t.Errorf("signed PE: ContentEnd = %d, want %d (start of the certificate table)", got, len(content))
	}

	if got := ContentEnd(content); got != len(content) {
		t.Errorf("unsigned PE: ContentEnd = %d, want len(data) %d", got, len(content))
	}

	notAtEOF := append(append([]byte{}, signed...), []byte("appended after signing")...)
	if got := ContentEnd(notAtEOF); got != len(notAtEOF) {
		t.Errorf("table not at EOF: ContentEnd = %d, want len(data) %d", got, len(notAtEOF))
	}

	for name, data := range map[string][]byte{
		"empty":        nil,
		"not a PE":     []byte("#!/bin/sh\necho linux bundle trailer data follows........................"),
		"bad e_lfanew": func() []byte { b := fakePE(); binary.LittleEndian.PutUint32(b[0x3C:], 0xFFFFFFF0); return b }(),
		"truncated":    fakePE()[:0x90],
	} {
		if got := ContentEnd(data); got != len(data) {
			t.Errorf("%s: ContentEnd = %d, want len(data) %d", name, got, len(data))
		}
	}
}

func TestPaddingOnly(t *testing.T) {
	data := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	if !PaddingOnly(data, 1, 1) || !PaddingOnly(data, 1, 8) {
		t.Error("0–7 zero bytes must count as signer padding")
	}
	if PaddingOnly(data, 1, 9) {
		t.Error("8 bytes is more than alignment padding")
	}
	if PaddingOnly(data, 0, 3) || PaddingOnly(data, 8, 10) {
		t.Error("non-zero bytes are not padding")
	}
	if PaddingOnly(data, 5, 3) || PaddingOnly(data, 5, 20) {
		t.Error("invalid ranges must be rejected")
	}
}
