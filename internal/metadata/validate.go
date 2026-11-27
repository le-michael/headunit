package metadata

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.senan.xyz/taglib"
)

// ErrInvalidAudio is returned (wrapped) by ValidateAudio when a file has an
// unsupported extension, is empty or non-regular, fails container/frame parsing,
// or does not match its declared audio format.
var ErrInvalidAudio = errors.New("metadata: invalid audio file")

// ValidateAudio strictly verifies that absPath is a readable, non-empty regular
// audio file whose contents match one of Headunit's 7 supported formats.
// Invalid/corrupt/mismatched audio returns an error matching errors.Is(err, ErrInvalidAudio);
// OS, permission, and unexpected TagLib runtime failures return unwrapped errors
// so callers can distinguish HTTP 422 from HTTP 500.
func ValidateAudio(absPath string) error {
	return validateAudio(absPath, taglib.ReadTags, taglib.ReadProperties)
}

func validateAudio(
	absPath string,
	readTags func(string) (map[string][]string, error),
	readProps func(string) (taglib.Properties, error),
) error {
	if err := checkValidatableFile(absPath); err != nil {
		return err
	}

	suffix, _, ok := LookupFormat(absPath)
	if !ok {
		return fmt.Errorf("%w: unsupported audio extension for %q", ErrInvalidAudio, absPath)
	}

	readPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", absPath, err)
	}

	if suffix == "aac" {
		return validateADTS(readPath)
	}

	if _, err := readTags(readPath); err != nil {
		if !errors.Is(err, taglib.ErrInvalidFile) {
			return fmt.Errorf("read tags %q: %w", absPath, err)
		}
		if err := checkReadableRegularFile(absPath); err != nil {
			return err
		}
		return fmt.Errorf("%w: invalid %s container or tags", ErrInvalidAudio, suffix)
	}

	props, err := readProps(readPath)
	if err != nil {
		if !errors.Is(err, taglib.ErrInvalidFile) {
			return fmt.Errorf("read properties %q: %w", absPath, err)
		}
		if err := checkReadableRegularFile(absPath); err != nil {
			return err
		}
		return fmt.Errorf("%w: invalid %s audio properties", ErrInvalidAudio, suffix)
	}
	if hasNoProperties(props) {
		// Re-check readability so TOCTOU removal or permission loss surfaces
		// as an OS error rather than ErrInvalidAudio.
		if err := checkReadableRegularFile(absPath); err != nil {
			return err
		}
		return fmt.Errorf("%w: missing %s audio properties", ErrInvalidAudio, suffix)
	}

	return validateProperties(suffix, props)
}

func checkValidatableFile(absPath string) error {
	st, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat %q: %w", absPath, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file %q", ErrInvalidAudio, absPath)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open %q: %w", absPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", absPath, err)
	}
	if st.Size() <= 0 {
		return fmt.Errorf("%w: empty file %q", ErrInvalidAudio, absPath)
	}
	return nil
}

func validateProperties(suffix string, props taglib.Properties) error {
	if props.SampleRate == 0 || props.Channels == 0 {
		return fmt.Errorf("%w: %s stream has sample_rate=%d channels=%d",
			ErrInvalidAudio, suffix, props.SampleRate, props.Channels)
	}

	switch suffix {
	case "mp3":
		if props.Format != "mpeg" {
			return fmt.Errorf("%w: expected mpeg format for mp3, got %q", ErrInvalidAudio, props.Format)
		}
	case "flac":
		if props.Format != "flac" {
			return fmt.Errorf("%w: expected flac format for flac, got %q", ErrInvalidAudio, props.Format)
		}
	case "m4a":
		if props.Format != "mp4" {
			return fmt.Errorf("%w: expected mp4 format for m4a, got %q", ErrInvalidAudio, props.Format)
		}
	case "ogg":
		if props.Format != "ogg" {
			return fmt.Errorf("%w: expected ogg format for ogg, got %q", ErrInvalidAudio, props.Format)
		}
	case "opus":
		if props.Format != "ogg" || props.InnerCodec != "opus" {
			return fmt.Errorf("%w: expected ogg/opus for opus, got %q/%q",
				ErrInvalidAudio, props.Format, props.InnerCodec)
		}
	case "wav":
		if props.Format != "wav" {
			return fmt.Errorf("%w: expected wav format for wav, got %q", ErrInvalidAudio, props.Format)
		}
	default:
		return fmt.Errorf("%w: unsupported format %q", ErrInvalidAudio, suffix)
	}
	return nil
}

func validateADTS(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer func() {
		_ = f.Close()
	}()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file %q", ErrInvalidAudio, path)
	}
	fileSize := st.Size()
	if fileSize <= 0 {
		return fmt.Errorf("%w: empty file %q", ErrInvalidAudio, path)
	}

	offset, err := skipOptionalID3v2(f, path, fileSize)
	if err != nil {
		return err
	}

	frameLen, err := readAndParseADTSFrame(f, path, offset, fileSize)
	if err != nil {
		return err
	}

	nextOffset := offset + frameLen
	if nextOffset == fileSize {
		return nil
	}

	rem := fileSize - nextOffset
	var tail [8]byte
	n, err := f.ReadAt(tail[:], nextOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if rem == 128 && n >= 3 && string(tail[:3]) == "TAG" {
		return nil
	}
	if rem >= 32 && n == 8 && string(tail[:8]) == "APETAGEX" {
		return nil
	}

	if _, err := readAndParseADTSFrame(f, path, nextOffset, fileSize); err != nil {
		return err
	}
	return nil
}

func skipOptionalID3v2(r io.ReaderAt, path string, fileSize int64) (int64, error) {
	if fileSize < 3 {
		return 0, fmt.Errorf("%w: truncated ADTS file", ErrInvalidAudio)
	}

	var id3Hdr [10]byte
	n, err := r.ReadAt(id3Hdr[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read %q: %w", path, err)
	}
	if n < 3 || string(id3Hdr[:3]) != "ID3" {
		return 0, nil
	}
	if n < 10 {
		return 0, fmt.Errorf("%w: truncated ID3v2 header", ErrInvalidAudio)
	}
	if id3Hdr[3] == 0xFF || id3Hdr[4] == 0xFF {
		return 0, fmt.Errorf("%w: invalid ID3v2 version", ErrInvalidAudio)
	}
	if (id3Hdr[6]|id3Hdr[7]|id3Hdr[8]|id3Hdr[9])&0x80 != 0 {
		return 0, fmt.Errorf("%w: invalid ID3v2 syncsafe size", ErrInvalidAudio)
	}

	tagSize := int64(id3Hdr[6])<<21 |
		int64(id3Hdr[7])<<14 |
		int64(id3Hdr[8])<<7 |
		int64(id3Hdr[9])
	offset := 10 + tagSize
	if id3Hdr[5]&0x10 != 0 {
		offset += 10
	}
	if offset >= fileSize {
		return 0, fmt.Errorf("%w: no audio frames after ID3v2 header", ErrInvalidAudio)
	}
	return offset, nil
}

func readAndParseADTSFrame(r io.ReaderAt, path string, offset, fileSize int64) (int64, error) {
	if offset < 0 || fileSize-offset < 7 {
		return 0, fmt.Errorf("%w: truncated ADTS frame header", ErrInvalidAudio)
	}

	var hdr [7]byte
	if _, err := r.ReadAt(hdr[:], offset); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("%w: truncated ADTS frame header", ErrInvalidAudio)
		}
		return 0, fmt.Errorf("read %q: %w", path, err)
	}

	if hdr[0] != 0xFF || (hdr[1]&0xF6) != 0xF0 {
		return 0, fmt.Errorf("%w: invalid ADTS syncword or layer", ErrInvalidAudio)
	}

	sampleRateIdx := (hdr[2] >> 2) & 0x0F
	if sampleRateIdx > 12 {
		return 0, fmt.Errorf("%w: reserved ADTS sampling frequency index %d", ErrInvalidAudio, sampleRateIdx)
	}

	channelConfig := ((hdr[2] & 0x01) << 2) | ((hdr[3] >> 6) & 0x03)
	if channelConfig < 1 || channelConfig > 7 {
		return 0, fmt.Errorf("%w: invalid ADTS channel configuration %d", ErrInvalidAudio, channelConfig)
	}

	headerLen := int64(7)
	if hdr[1]&0x01 == 0 {
		headerLen = 9
	}

	frameLen := (int64(hdr[3]&0x03) << 11) | (int64(hdr[4]) << 3) | (int64(hdr[5]>>5) & 0x07)
	if frameLen < headerLen {
		return 0, fmt.Errorf("%w: invalid ADTS frame length %d", ErrInvalidAudio, frameLen)
	}
	if offset+frameLen > fileSize {
		return 0, fmt.Errorf("%w: truncated ADTS frame", ErrInvalidAudio)
	}

	return frameLen, nil
}
