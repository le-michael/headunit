// Package testutil provides shared audio file fixtures and file-writing helpers for tests.
package testutil

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"go.senan.xyz/taglib"
)

// MinimalWAVBytes returns a valid 2-second, 128 kbps (8000 Hz, 16-bit mono PCM)
// RIFF/WAVE payload.
func MinimalWAVBytes() []byte {
	const (
		channels      uint16 = 1
		sampleRate    uint32 = 8000
		bitsPerSample uint16 = 16
		blockAlign           = channels * bitsPerSample / 8
		byteRate             = sampleRate * uint32(blockAlign)
		dataSize      uint32 = byteRate * 2 // 32,000 bytes = 2 seconds
	)

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataSize)
	return buf
}

// WriteMinimalWAV ensures parent directories exist and writes MinimalWAVBytes
// to path with mode 0o600.
func WriteMinimalWAV(t testing.TB, path string) {
	t.Helper()
	WriteBytes(t, path, MinimalWAVBytes())
}

// WriteMinimalMP3 ensures parent directories exist and writes 77 MPEG-1 Layer III
// CBR frames (44100 Hz, 128 kbps, 417 bytes/frame, ~2.01 seconds) to path with
// mode 0o600.
func WriteMinimalMP3(t testing.TB, path string) {
	t.Helper()

	const (
		frameSize  = 417
		frameCount = 77
	)

	buf := make([]byte, frameSize*frameCount)
	for i := range frameCount {
		off := i * frameSize
		buf[off] = 0xFF
		buf[off+1] = 0xFB
		buf[off+2] = 0x90
		buf[off+3] = 0x00
	}

	WriteBytes(t, path, buf)
}

// WriteMinimalFLAC ensures parent directories exist and writes a minimal valid
// 2-second FLAC file (STREAMINFO block and 16,000 payload bytes) to path with
// mode 0o600.
func WriteMinimalFLAC(t testing.TB, path string) {
	t.Helper()

	const (
		sampleRate    uint64 = 44100
		channels      uint64 = 2
		bitsPerSample uint64 = 16
		totalSamples  uint64 = 88200 // 2s at 44100 Hz
		payloadSize          = 16000
	)

	buf := make([]byte, 4+4+34+payloadSize)
	copy(buf[0:4], "fLaC")
	buf[4] = 0x80 // last metadata block, type 0 (STREAMINFO)
	buf[5] = 0x00
	buf[6] = 0x00
	buf[7] = 34

	binary.BigEndian.PutUint16(buf[8:10], 4096)  // min block size
	binary.BigEndian.PutUint16(buf[10:12], 4096) // max block size
	packed := (sampleRate << 44) | ((channels - 1) << 41) | ((bitsPerSample - 1) << 36) | totalSamples
	binary.BigEndian.PutUint64(buf[18:26], packed)

	WriteBytes(t, path, buf)
}

// WriteTags clears existing metadata and writes tags to path via taglib.WriteTags.
func WriteTags(t testing.TB, path string, tags map[string][]string) {
	t.Helper()
	if err := taglib.WriteTags(path, tags, taglib.Clear); err != nil {
		t.Fatalf("taglib.WriteTags(%q): %v", path, err)
	}
}

// WriteBytes ensures parent directories exist and writes data to path with mode 0o600.
func WriteBytes(t testing.TB, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", path, err)
	}
}
