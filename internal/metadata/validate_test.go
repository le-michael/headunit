package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.senan.xyz/taglib"

	"headunit/internal/testutil"
)

func TestValidateAudio_ValidFormats(t *testing.T) {
	tests := []struct {
		name  string
		ext   string
		write func(t testing.TB, path string)
	}{
		{name: "mp3", ext: ".mp3", write: testutil.WriteMinimalMP3},
		{name: "flac", ext: ".flac", write: testutil.WriteMinimalFLAC},
		{name: "wav", ext: ".wav", write: testutil.WriteMinimalWAV},
		{name: "m4a", ext: ".m4a", write: writeMinimalM4A},
		{name: "ogg", ext: ".ogg", write: writeMinimalOGG},
		{name: "opus", ext: ".opus", write: writeMinimalOPUS},
		{name: "aac single frame", ext: ".aac", write: writeMinimalADTS(1, false, false, false)},
		{name: "aac multi frame", ext: ".aac", write: writeMinimalADTS(3, false, false, false)},
		{name: "aac with crc", ext: ".aac", write: writeMinimalADTS(2, true, false, false)},
		{name: "aac with id3v2", ext: ".aac", write: writeMinimalADTS(2, false, true, false)},
		{name: "aac with id3v2 footer", ext: ".aac", write: writeMinimalADTS(2, false, true, true)},
		{name: "aac with trailing id3v1", ext: ".aac", write: writeMinimalADTSWithTrailer("id3v1")},
		{name: "aac with trailing apev2", ext: ".aac", write: writeMinimalADTSWithTrailer("apev2")},
		{name: "uppercase MP3", ext: ".MP3", write: testutil.WriteMinimalMP3},
		{name: "uppercase FLAC", ext: ".FLAC", write: testutil.WriteMinimalFLAC},
		{name: "uppercase M4A", ext: ".M4A", write: writeMinimalM4A},
		{name: "uppercase OGG", ext: ".OGG", write: writeMinimalOGG},
		{name: "uppercase OPUS", ext: ".OPUS", write: writeMinimalOPUS},
		{name: "uppercase WAV", ext: ".WAV", write: testutil.WriteMinimalWAV},
		{name: "uppercase AAC", ext: ".AAC", write: writeMinimalADTS(2, false, false, false)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "track"+tc.ext)
			tc.write(t, p)

			if err := ValidateAudio(p); err != nil {
				t.Fatalf("ValidateAudio(%q) unexpected error: %v", p, err)
			}
		})
	}

	t.Run("cross-directory symlink to valid mp3", func(t *testing.T) {
		root := t.TempDir()
		realPath := filepath.Join(root, "real", "source.mp3")
		testutil.WriteMinimalMP3(t, realPath)

		linkDir := filepath.Join(root, "links")
		if err := os.MkdirAll(linkDir, 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q): %v", linkDir, err)
		}
		linkPath := filepath.Join(linkDir, "alias.mp3")
		if err := os.Symlink(realPath, linkPath); err != nil {
			t.Fatalf("os.Symlink(%q, %q): %v", realPath, linkPath, err)
		}

		if err := ValidateAudio(linkPath); err != nil {
			t.Fatalf("ValidateAudio(symlink %q) unexpected error: %v", linkPath, err)
		}
	})
}

func TestValidateAudio_InvalidAudio(t *testing.T) {
	t.Run("empty file for every supported extension", func(t *testing.T) {
		for _, ext := range []string{".mp3", ".flac", ".m4a", ".aac", ".ogg", ".opus", ".wav"} {
			t.Run(ext, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "empty"+ext)
				testutil.WriteBytes(t, p, nil)

				err := ValidateAudio(p)
				if !errors.Is(err, ErrInvalidAudio) {
					t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", p, err)
				}
			})
		}
	})

	t.Run("plaintext renamed to every supported extension", func(t *testing.T) {
		payload := []byte("hello world, this is definitely plain text and not audio")
		for _, ext := range []string{".mp3", ".flac", ".m4a", ".aac", ".ogg", ".opus", ".wav"} {
			t.Run(ext, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "fake"+ext)
				testutil.WriteBytes(t, p, payload)

				err := ValidateAudio(p)
				if !errors.Is(err, ErrInvalidAudio) {
					t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", p, err)
				}
			})
		}
	})

	t.Run("ID3v2-tagged garbage mp3", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "tagged-garbage.mp3")
		var buf bytes.Buffer
		buf.Write(makeID3v2Header(16, false))
		buf.Write(make([]byte, 16))
		buf.WriteString("this is not mpeg audio frames at all, just plain garbage")
		testutil.WriteBytes(t, p, buf.Bytes())

		err := ValidateAudio(p)
		if !errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", p, err)
		}
	})

	t.Run("corrupt or truncated aac variants", func(t *testing.T) {
		validFrame := makeADTSFrame(false, 4, 2, 32)
		tests := []struct {
			name string
			data []byte
		}{
			{
				name: "too short (< 7 bytes)",
				data: []byte{0xFF, 0xF1, 0x50, 0x80, 0x04, 0x00},
			},
			{
				name: "bad syncword",
				data: func() []byte {
					b := append([]byte(nil), validFrame...)
					b[0] = 0xFE
					return b
				}(),
			},
			{
				name: "bad layer (non-zero)",
				data: func() []byte {
					b := append([]byte(nil), validFrame...)
					b[1] |= 0x02
					return b
				}(),
			},
			{
				name: "mp3 frame renamed to aac",
				data: func() []byte {
					b := make([]byte, 417)
					b[0] = 0xFF
					b[1] = 0xFB
					b[2] = 0x90
					return b
				}(),
			},
			{
				name: "reserved sample-rate index 13",
				data: makeADTSFrame(false, 13, 2, 32),
			},
			{
				name: "forbidden sample-rate index 15",
				data: makeADTSFrame(false, 15, 2, 32),
			},
			{
				name: "channel config 0",
				data: makeADTSFrame(false, 4, 0, 32),
			},
			{
				name: "frame length smaller than header",
				data: func() []byte {
					b := make([]byte, 32)
					copy(b, makeADTSFrameHeader(false, 4, 2, 5))
					return b
				}(),
			},
			{
				name: "crc frame length smaller than 9-byte header",
				data: func() []byte {
					b := make([]byte, 32)
					copy(b, makeADTSFrameHeader(true, 4, 2, 8))
					return b
				}(),
			},
			{
				name: "truncated frame length beyond EOF",
				data: validFrame[:len(validFrame)-1],
			},
			{
				name: "corrupt second frame",
				data: append(append([]byte(nil), validFrame...), []byte("garbage-second-frame")...),
			},
			{
				name: "truncated second frame",
				data: append(append([]byte(nil), validFrame...), validFrame[:len(validFrame)-1]...),
			},
			{
				name: "truncated trailing id3v1 tag",
				data: append(append([]byte(nil), validFrame...), []byte("TAGshort")...),
			},
			{
				name: "truncated ID3v2 header",
				data: []byte("ID3\x04\x00"),
			},
			{
				name: "invalid ID3v2 non-syncsafe size",
				data: append([]byte{'I', 'D', '3', 3, 0, 0, 0x80, 0, 0, 10}, validFrame...),
			},
			{
				name: "ID3v2 with no audio frames after tag",
				data: append(makeID3v2Header(16, false), make([]byte, 16)...),
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "bad.aac")
				testutil.WriteBytes(t, p, tc.data)

				err := ValidateAudio(p)
				if !errors.Is(err, ErrInvalidAudio) {
					t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", p, err)
				}
			})
		}
	})

	t.Run("mismatched container extensions", func(t *testing.T) {
		tests := []struct {
			name  string
			ext   string
			write func(t testing.TB, path string)
		}{
			{name: "WAV renamed to flac", ext: ".flac", write: testutil.WriteMinimalWAV},
			{name: "FLAC renamed to mp3", ext: ".mp3", write: testutil.WriteMinimalFLAC},
			{name: "Ogg Vorbis renamed to opus", ext: ".opus", write: writeMinimalOGG},
			{name: "MP3 renamed to m4a", ext: ".m4a", write: testutil.WriteMinimalMP3},
			{name: "FLAC renamed to wav", ext: ".wav", write: testutil.WriteMinimalFLAC},
			{name: "M4A renamed to ogg", ext: ".ogg", write: writeMinimalM4A},
			{name: "ADTS AAC renamed to mp3", ext: ".mp3", write: writeMinimalADTS(3, false, false, false)},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "mismatch"+tc.ext)
				tc.write(t, p)

				err := ValidateAudio(p)
				if !errors.Is(err, ErrInvalidAudio) {
					t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", p, err)
				}
			})
		}
	})

	t.Run("unsupported extensions", func(t *testing.T) {
		for _, name := range []string{"notes.txt", "cover.jpg", "noext", "track.wma"} {
			t.Run(name, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), name)
				testutil.WriteBytes(t, p, []byte("some content"))

				err := ValidateAudio(p)
				if !errors.Is(err, ErrInvalidAudio) {
					t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", p, err)
				}
			})
		}
	})

	t.Run("directory path", func(t *testing.T) {
		dir := t.TempDir()
		err := ValidateAudio(dir)
		if !errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", dir, err)
		}

		mp3NamedDir := filepath.Join(t.TempDir(), "album.mp3")
		if err := os.MkdirAll(mp3NamedDir, 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q): %v", mp3NamedDir, err)
		}
		err = ValidateAudio(mp3NamedDir)
		if !errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, ErrInvalidAudio)", mp3NamedDir, err)
		}
	})
}

func TestValidateAudio_OSErrorsAndTagLibSeam(t *testing.T) {
	t.Run("missing file does not match ErrInvalidAudio", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "missing.mp3")
		err := ValidateAudio(p)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, os.ErrNotExist)", p, err)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("ValidateAudio(%q) error = %v, must not wrap ErrInvalidAudio", p, err)
		}
	})

	t.Run("unreadable file does not match ErrInvalidAudio", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permission checks")
		}
		p := filepath.Join(t.TempDir(), "locked.mp3")
		testutil.WriteMinimalMP3(t, p)
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatalf("os.Chmod(%q, 0o000): %v", p, err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(p, 0o600)
		})

		err := ValidateAudio(p)
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("ValidateAudio(%q) error = %v, want errors.Is(_, os.ErrPermission)", p, err)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("ValidateAudio(%q) error = %v, must not wrap ErrInvalidAudio", p, err)
		}
	})

	t.Run("unexpected readTags error propagates without ErrInvalidAudio", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "boom.mp3")
		testutil.WriteMinimalMP3(t, p)
		sentinel := errors.New("wasm runtime init failed")
		readPropsCalled := false

		err := validateAudio(
			p,
			func(string) (map[string][]string, error) { return nil, sentinel },
			func(string) (taglib.Properties, error) {
				readPropsCalled = true
				return taglib.Properties{}, nil
			},
		)
		if !errors.Is(err, sentinel) {
			t.Fatalf("validateAudio() error = %v, want errors.Is(_, %v)", err, sentinel)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("validateAudio() error = %v, must not wrap ErrInvalidAudio", err)
		}
		if readPropsCalled {
			t.Errorf("readProps was called after unexpected readTags error, want skipped")
		}
	})

	t.Run("unexpected readProps error propagates without ErrInvalidAudio", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "boom.mp3")
		testutil.WriteMinimalMP3(t, p)
		sentinel := errors.New("wasm call failed")

		err := validateAudio(
			p,
			func(string) (map[string][]string, error) { return map[string][]string{}, nil },
			func(string) (taglib.Properties, error) { return taglib.Properties{}, sentinel },
		)
		if !errors.Is(err, sentinel) {
			t.Fatalf("validateAudio() error = %v, want errors.Is(_, %v)", err, sentinel)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("validateAudio() error = %v, must not wrap ErrInvalidAudio", err)
		}
	})

	t.Run("TOCTOU deletion during readTags ErrInvalidFile returns os.ErrNotExist", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "vanishing.mp3")
		testutil.WriteMinimalMP3(t, p)
		readPropsCalled := false

		err := validateAudio(
			p,
			func(string) (map[string][]string, error) {
				if rmErr := os.Remove(p); rmErr != nil {
					t.Errorf("os.Remove(%q): %v", p, rmErr)
				}
				return nil, taglib.ErrInvalidFile
			},
			func(string) (taglib.Properties, error) {
				readPropsCalled = true
				return taglib.Properties{}, nil
			},
		)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("validateAudio() error = %v, want errors.Is(_, os.ErrNotExist)", err)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("validateAudio() error = %v, must not wrap ErrInvalidAudio", err)
		}
		if readPropsCalled {
			t.Errorf("readProps was called after TOCTOU deletion in readTags, want skipped")
		}
	})

	t.Run("TOCTOU deletion during readProps zero properties returns os.ErrNotExist", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "vanishing.mp3")
		testutil.WriteMinimalMP3(t, p)

		err := validateAudio(
			p,
			taglib.ReadTags,
			func(readPath string) (taglib.Properties, error) {
				if rmErr := os.Remove(p); rmErr != nil {
					t.Errorf("os.Remove(%q): %v", p, rmErr)
				}
				return taglib.ReadProperties(readPath)
			},
		)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("validateAudio() error = %v, want errors.Is(_, os.ErrNotExist)", err)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("validateAudio() error = %v, must not wrap ErrInvalidAudio", err)
		}
	})

	t.Run("TOCTOU chmod 0000 during readProps ErrInvalidFile returns os.ErrPermission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permission checks")
		}
		p := filepath.Join(t.TempDir(), "chmodded.mp3")
		testutil.WriteMinimalMP3(t, p)
		t.Cleanup(func() {
			_ = os.Chmod(p, 0o600)
		})

		err := validateAudio(
			p,
			func(string) (map[string][]string, error) { return map[string][]string{}, nil },
			func(string) (taglib.Properties, error) {
				if chErr := os.Chmod(p, 0o000); chErr != nil {
					t.Errorf("os.Chmod(%q, 0o000): %v", p, chErr)
				}
				return taglib.Properties{}, taglib.ErrInvalidFile
			},
		)
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("validateAudio() error = %v, want errors.Is(_, os.ErrPermission)", err)
		}
		if errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("validateAudio() error = %v, must not wrap ErrInvalidAudio", err)
		}
	})

	t.Run("readProps zero properties on readable file returns ErrInvalidAudio", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "zeroprops.mp3")
		testutil.WriteMinimalMP3(t, p)

		err := validateAudio(
			p,
			func(string) (map[string][]string, error) { return map[string][]string{}, nil },
			func(string) (taglib.Properties, error) { return taglib.Properties{}, nil },
		)
		if !errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("validateAudio() error = %v, want errors.Is(_, ErrInvalidAudio)", err)
		}
	})
}

func TestValidateAudio_Concurrent(t *testing.T) {
	dir := t.TempDir()
	mp3Path := filepath.Join(dir, "01.mp3")
	flacPath := filepath.Join(dir, "02.flac")
	aacPath := filepath.Join(dir, "03.aac")
	badPath := filepath.Join(dir, "04.mp3")

	testutil.WriteMinimalMP3(t, mp3Path)
	testutil.WriteMinimalFLAC(t, flacPath)
	writeMinimalADTS(2, false, true, false)(t, aacPath)
	testutil.WriteBytes(t, badPath, []byte("plain text"))

	type target struct {
		path    string
		wantErr bool
	}
	targets := []target{
		{path: mp3Path, wantErr: false},
		{path: flacPath, wantErr: false},
		{path: aacPath, wantErr: false},
		{path: badPath, wantErr: true},
	}

	const workers = 12
	errs := make([]error, workers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(id int) {
			defer wg.Done()
			<-start
			errs[id] = ValidateAudio(targets[id%len(targets)].path)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range workers {
		tc := targets[i%len(targets)]
		if tc.wantErr {
			if !errors.Is(errs[i], ErrInvalidAudio) {
				t.Errorf("worker %d ValidateAudio(%q) = %v, want ErrInvalidAudio", i, tc.path, errs[i])
			}
			continue
		}
		if errs[i] != nil {
			t.Errorf("worker %d ValidateAudio(%q) unexpected error: %v", i, tc.path, errs[i])
		}
	}
}

// --- Pure-Go deterministic audio fixture builders ---

func writeMinimalADTS(frames int, withCRC, withID3v2, withID3v2Footer bool) func(testing.TB, string) {
	return func(t testing.TB, path string) {
		t.Helper()
		var buf bytes.Buffer
		if withID3v2 {
			const tagBodySize = 16
			buf.Write(makeID3v2Header(tagBodySize, withID3v2Footer))
			buf.Write(make([]byte, tagBodySize))
			if withID3v2Footer {
				buf.WriteString("3DI")
				buf.Write(makeID3v2Header(tagBodySize, true)[3:])
			}
		}
		for range frames {
			buf.Write(makeADTSFrame(withCRC, 4, 2, 32))
		}
		testutil.WriteBytes(t, path, buf.Bytes())
	}
}

func writeMinimalADTSWithTrailer(kind string) func(testing.TB, string) {
	return func(t testing.TB, path string) {
		t.Helper()
		var buf bytes.Buffer
		buf.Write(makeADTSFrame(false, 4, 2, 32))
		switch kind {
		case "id3v1":
			tag := make([]byte, 128)
			copy(tag[0:3], "TAG")
			buf.Write(tag)
		case "apev2":
			tag := make([]byte, 32)
			copy(tag[0:8], "APETAGEX")
			buf.Write(tag)
		default:
			t.Fatalf("unknown trailer kind %q", kind)
		}
		testutil.WriteBytes(t, path, buf.Bytes())
	}
}

func makeID3v2Header(size int, footer bool) []byte {
	hdr := make([]byte, 10)
	copy(hdr[0:3], "ID3")
	hdr[3] = 4 // v2.4
	hdr[4] = 0
	if footer {
		hdr[5] = 0x10
	}
	hdr[6] = byte((size >> 21) & 0x7F)
	hdr[7] = byte((size >> 14) & 0x7F)
	hdr[8] = byte((size >> 7) & 0x7F)
	hdr[9] = byte(size & 0x7F)
	return hdr
}

func makeADTSFrame(withCRC bool, sampleRateIdx, channelConfig byte, frameLen int) []byte {
	frame := make([]byte, frameLen)
	copy(frame[:7], makeADTSFrameHeader(withCRC, sampleRateIdx, channelConfig, frameLen))
	return frame
}

func makeADTSFrameHeader(withCRC bool, sampleRateIdx, channelConfig byte, frameLen int) []byte {
	var hdr [7]byte
	hdr[0] = 0xFF
	hdr[1] = 0xF1 // MPEG-4, layer 00, protection_absent=1
	if withCRC {
		hdr[1] = 0xF0 // protection_absent=0 (9-byte header)
	}
	// profile=1 (AAC-LC -> 01 in top 2 bits)
	hdr[2] = (1 << 6) | ((sampleRateIdx & 0x0F) << 2) | ((channelConfig >> 2) & 0x01)
	hdr[3] = ((channelConfig & 0x03) << 6) | byte((frameLen>>11)&0x03)
	hdr[4] = byte((frameLen >> 3) & 0xFF)
	hdr[5] = byte((frameLen&0x07)<<5) | 0x1F
	hdr[6] = 0xFC
	return hdr[:]
}

func writeMinimalOGG(t testing.TB, path string) {
	t.Helper()
	var buf bytes.Buffer

	// Vorbis identification header (30 bytes)
	idPkt := make([]byte, 30)
	idPkt[0] = 0x01
	copy(idPkt[1:7], "vorbis")
	binary.LittleEndian.PutUint32(idPkt[7:11], 0)      // version
	idPkt[11] = 2                                      // channels
	binary.LittleEndian.PutUint32(idPkt[12:16], 44100) // sample_rate
	binary.LittleEndian.PutUint32(idPkt[20:24], 128000)
	idPkt[28] = 0xB8 // blocksize0=256, blocksize1=2048
	idPkt[29] = 0x01 // framing
	buf.Write(makeOggPage(0x02, 0, 1, 0, idPkt))

	// Vorbis comment header
	commentPkt := make([]byte, 16)
	commentPkt[0] = 0x03
	copy(commentPkt[1:7], "vorbis")
	commentPkt[15] = 0x01
	buf.Write(makeOggPage(0x00, 0, 1, 1, commentPkt))

	// EOS page with granule position = 88200 samples (2s at 44100 Hz)
	buf.Write(makeOggPage(0x04, 88200, 1, 2, []byte{0x00, 0x00, 0x00, 0x00}))

	testutil.WriteBytes(t, path, buf.Bytes())
}

func writeMinimalOPUS(t testing.TB, path string) {
	t.Helper()
	var buf bytes.Buffer

	// OpusHead (19 bytes)
	headPkt := make([]byte, 19)
	copy(headPkt[0:8], "OpusHead")
	headPkt[8] = 1                                     // version
	headPkt[9] = 2                                     // channels
	binary.LittleEndian.PutUint16(headPkt[10:12], 312) // pre-skip
	binary.LittleEndian.PutUint32(headPkt[12:16], 48000)
	buf.Write(makeOggPage(0x02, 0, 2, 0, headPkt))

	// OpusTags (16 bytes)
	tagsPkt := make([]byte, 16)
	copy(tagsPkt[0:8], "OpusTags")
	buf.Write(makeOggPage(0x00, 0, 2, 1, tagsPkt))

	// EOS page with 2s at 48000 Hz + pre-skip
	buf.Write(makeOggPage(0x04, 96312, 2, 2, []byte{0xf8, 0xff, 0xfe, 0x00}))

	testutil.WriteBytes(t, path, buf.Bytes())
}

func makeOggPage(headerType byte, granulePos uint64, serial, seq uint32, packet []byte) []byte {
	page := make([]byte, 27+1+len(packet))
	copy(page[0:4], "OggS")
	page[4] = 0 // stream_structure_version
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], granulePos)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	// CRC at [22:26] left zero during calculation.
	page[26] = 1 // 1 segment
	page[27] = byte(len(packet))
	copy(page[28:], packet)

	binary.LittleEndian.PutUint32(page[22:26], oggCRC32(page))
	return page
}

func oggCRC32(data []byte) uint32 {
	const poly = 0x04C11DB7
	var crc uint32
	for _, b := range data {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ poly
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func writeMinimalM4A(t testing.TB, path string) {
	t.Helper()

	ftyp := mp4Box("ftyp",
		[]byte("M4A "),
		be32(0),
		[]byte("M4A "),
		[]byte("mp42"),
		[]byte("isom"),
	)

	// mvhd (version 0, 96 bytes payload)
	mvhdPayload := make([]byte, 96)
	binary.BigEndian.PutUint32(mvhdPayload[12:16], 44100) // timescale
	binary.BigEndian.PutUint32(mvhdPayload[16:20], 88200) // duration (2s)
	binary.BigEndian.PutUint32(mvhdPayload[20:24], 0x00010000)
	binary.BigEndian.PutUint16(mvhdPayload[24:26], 0x0100)
	binary.BigEndian.PutUint32(mvhdPayload[36:40], 0x00010000)
	binary.BigEndian.PutUint32(mvhdPayload[52:56], 0x00010000)
	binary.BigEndian.PutUint32(mvhdPayload[68:72], 0x40000000)
	binary.BigEndian.PutUint32(mvhdPayload[92:96], 2) // next_track_ID
	mvhd := mp4Box("mvhd", mvhdPayload)

	// tkhd (version 0, flags 1, 80 bytes payload)
	tkhdPayload := make([]byte, 80)
	tkhdPayload[3] = 0x01
	binary.BigEndian.PutUint32(tkhdPayload[12:16], 1)     // track_ID
	binary.BigEndian.PutUint32(tkhdPayload[20:24], 88200) // duration
	binary.BigEndian.PutUint16(tkhdPayload[36:38], 0x0100)
	binary.BigEndian.PutUint32(tkhdPayload[40:44], 0x00010000)
	binary.BigEndian.PutUint32(tkhdPayload[56:60], 0x00010000)
	binary.BigEndian.PutUint32(tkhdPayload[72:76], 0x40000000)
	tkhd := mp4Box("tkhd", tkhdPayload)

	// mdhd (version 0, 24 bytes payload)
	mdhdPayload := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhdPayload[12:16], 44100) // timescale
	binary.BigEndian.PutUint32(mdhdPayload[16:20], 88200) // duration
	mdhd := mp4Box("mdhd", mdhdPayload)

	// hdlr
	hdlrPayload := make([]byte, 25)
	copy(hdlrPayload[8:12], "soun")
	hdlr := mp4Box("hdlr", hdlrPayload)

	// smhd + dinf/dref
	smhd := mp4Box("smhd", make([]byte, 8))
	urlBox := mp4Box("url ", []byte{0, 0, 0, 1})
	dref := mp4Box("dref", be32(0), be32(1), urlBox)
	dinf := mp4Box("dinf", dref)

	// esds (AAC-LC 44100Hz stereo, 128 kbps)
	esdsPayload := []byte{
		0x00, 0x00, 0x00, 0x00, // version + flags
		0x03, 0x19, 0x00, 0x01, 0x00, // ES_Descriptor (id=1)
		0x04, 0x11, 0x40, 0x15, 0x00, 0x00, 0x00, // DecoderConfigDescriptor (AAC-LC)
		0x00, 0x01, 0xF4, 0x00, // maxBitrate = 128000
		0x00, 0x01, 0xF4, 0x00, // avgBitrate = 128000
		0x05, 0x02, 0x12, 0x10, // AudioSpecificConfig (AAC-LC, 44100Hz, 2ch)
		0x06, 0x01, 0x02, // SLConfigDescriptor
	}
	esds := mp4Box("esds", esdsPayload)

	// mp4a sample entry (28 bytes + esds)
	mp4aHeader := make([]byte, 28)
	binary.BigEndian.PutUint16(mp4aHeader[6:8], 1)           // data_reference_index
	binary.BigEndian.PutUint16(mp4aHeader[16:18], 2)         // channelcount
	binary.BigEndian.PutUint16(mp4aHeader[18:20], 16)        // samplesize
	binary.BigEndian.PutUint32(mp4aHeader[24:28], 44100<<16) // samplerate (16.16 fixed)
	mp4a := mp4Box("mp4a", mp4aHeader, esds)

	stsd := mp4Box("stsd", be32(0), be32(1), mp4a)
	stts := mp4Box("stts", be32(0), be32(1), be32(1), be32(88200))
	stsc := mp4Box("stsc", be32(0), be32(1), be32(1), be32(1), be32(1))
	stsz := mp4Box("stsz", be32(0), be32(4), be32(1))

	mdat := mp4Box("mdat", []byte{0x00, 0x00, 0x00, 0x00})

	// Build moov skeleton first to compute chunk offset into mdat payload.
	stcoZero := mp4Box("stco", be32(0), be32(1), be32(0))
	stblZero := mp4Box("stbl", stsd, stts, stsc, stsz, stcoZero)
	minfZero := mp4Box("minf", smhd, dinf, stblZero)
	mdiaZero := mp4Box("mdia", mdhd, hdlr, minfZero)
	trakZero := mp4Box("trak", tkhd, mdiaZero)
	moovZero := mp4Box("moov", mvhd, trakZero)

	mdatDataOffset := uint32(len(ftyp) + len(moovZero) + 8)
	stco := mp4Box("stco", be32(0), be32(1), be32(mdatDataOffset))
	stbl := mp4Box("stbl", stsd, stts, stsc, stsz, stco)
	minf := mp4Box("minf", smhd, dinf, stbl)
	mdia := mp4Box("mdia", mdhd, hdlr, minf)
	trak := mp4Box("trak", tkhd, mdia)
	moov := mp4Box("moov", mvhd, trak)

	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(moov)
	out.Write(mdat)
	testutil.WriteBytes(t, path, out.Bytes())
}

func mp4Box(boxType string, parts ...[]byte) []byte {
	size := 8
	for _, p := range parts {
		size += len(p)
	}
	buf := make([]byte, 8, size)
	binary.BigEndian.PutUint32(buf[0:4], uint32(size))
	copy(buf[4:8], boxType)
	for _, p := range parts {
		buf = append(buf, p...)
	}
	return buf
}

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}
