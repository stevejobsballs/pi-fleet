// Package blobs stores attachment files by content hash (DESIGN.md §5.10)
// and sanitises them on the way in (§3.6).
package blobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// MaxSize is the largest attachment accepted.
const MaxSize = 10 << 20

// Allowed MIME types.
const (
	JPEG = "image/jpeg"
	PNG  = "image/png"
	WebP = "image/webp"
	PDF  = "application/pdf"
)

var (
	ErrTooLarge = fmt.Errorf("blobs: files must be at most %d MiB", MaxSize>>20)
	ErrType     = errors.New("blobs: only JPEG, PNG, WebP and PDF files are accepted")
	// ErrHEIC: an iPhone's own photo format, which can't be checked here.
	ErrHEIC = errors.New("blobs: this is an iPhone HEIC photo, which can't be accepted. Add the photo from the iPhone itself (its browser sends it as JPEG), " +
		"or set the iPhone to Settings, Camera, Formats, Most Compatible")
	ErrHash        = errors.New("blobs: content does not match its hash")
	ErrMalformed   = errors.New("blobs: the file is damaged or not what its type claims")
	ErrNotFound    = errors.New("blobs: not found")
	shaRE          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errBadHashName = errors.New("blobs: bad hash")
)

// Store keeps blobs under Dir/ab/cd/<sha256>.
type Store struct{ Dir string }

func (s Store) path(sha string) (string, error) {
	if !shaRE.MatchString(sha) {
		return "", errBadHashName
	}
	return filepath.Join(s.Dir, sha[:2], sha[2:4], sha), nil
}

// Has reports whether a blob is stored.
func (s Store) Has(sha string) bool {
	p, err := s.path(sha)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Put stores data and returns its hash. Blobs are immutable, so storing
// existing content is a no-op.
func (s Store) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	return sha, s.PutExpected(sha, data)
}

// PutExpected stores data only if it hashes to sha.
func (s Store) PutExpected(sha string, data []byte) error {
	p, err := s.path(sha)
	if err != nil {
		return err
	}
	if len(data) > MaxSize {
		return ErrTooLarge
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
		return ErrHash
	}
	if s.Has(sha) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	part := p + ".part"
	if err := os.WriteFile(part, data, 0o600); err != nil {
		return err
	}
	if f, err := os.Open(part); err == nil {
		f.Sync()
		f.Close()
	}
	return os.Rename(part, p)
}

// Get reads a blob, checking its hash.
func (s Store) Get(sha string) ([]byte, error) {
	p, err := s.path(sha)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != sha {
		return nil, ErrHash
	}
	return b, nil
}

// Remove deletes a blob if present.
func (s Store) Remove(sha string) error {
	p, err := s.path(sha)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns every stored hash.
func (s Store) List() ([]string, error) {
	var out []string
	err := filepath.WalkDir(s.Dir, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if !d.IsDir() && shaRE.MatchString(d.Name()) {
			out = append(out, d.Name())
		}
		return nil
	})
	return out, err
}

// Sanitize identifies a file by its content and strips metadata that can
// carry location or personal details: EXIF/XMP/comments from JPEG (but
// keeping which way up the photo is, as phones record it there), text/
// EXIF/time chunks from PNG and EXIF/XMP from WebP. PDFs are accepted as
// they are.
func Sanitize(data []byte) (mime string, clean []byte, err error) {
	if len(data) > MaxSize {
		return "", nil, ErrTooLarge
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		clean, err = stripJPEG(data)
		return JPEG, clean, err
	case bytes.HasPrefix(data, pngSig):
		clean, err = stripPNG(data)
		return PNG, clean, err
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		clean, err = stripWebP(data)
		return WebP, clean, err
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return PDF, data, nil
	case isHEIC(data):
		return "", nil, ErrHEIC
	}
	return "", nil, ErrType
}

// isHEIC recognises HEIC/HEIF files by their ftyp box's brand.
func isHEIC(b []byte) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	switch string(b[8:12]) {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
		return true
	}
	return false
}

// exifOrientation reads the Orientation tag (1-8) from an APP1 Exif
// segment's payload ("Exif\0\0" + TIFF), or 0.
func exifOrientation(p []byte) int {
	if !bytes.HasPrefix(p, []byte("Exif\x00\x00")) || len(p) < 14 {
		return 0
	}
	t := p[6:]
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[off:]))
	for i := 0; i < n; i++ {
		e := off + 2 + 12*i
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 0
}

// orientationSegment is an APP1 Exif segment holding only an orientation.
func orientationSegment(o int) []byte {
	seg := []byte{0xFF, 0xE1, 0, 34, 'E', 'x', 'i', 'f', 0, 0, 'M', 'M', 0, 42, 0, 0, 0, 8,
		0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, byte(o), 0, 0, 0, 0, 0, 0}
	return seg
}

// stripJPEG drops APPn segments other than JFIF (APP0), ICC profiles
// (APP2 "ICC_PROFILE") and Adobe colour info (APP14), and COM segments.
// A photo's orientation (Exif tag 274) is kept, alone, so phone photos
// stay the right way up. Everything from the start of scan on is copied
// unchanged.
func stripJPEG(b []byte) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, len(b)))
	out.Write(b[:2]) // SOI
	if o := jpegOrientation(b); o > 1 {
		out.Write(orientationSegment(o))
	}
	i := 2
	for {
		if i+4 > len(b) || b[i] != 0xFF {
			return nil, ErrMalformed
		}
		marker := b[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA { // start of scan: the rest is image data
			out.Write(b[i:])
			return out.Bytes(), nil
		}
		if marker == 0xD9 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			out.Write(b[i : i+2])
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return nil, ErrMalformed
		}
		seg := b[i : i+2+n]
		keep := true
		switch {
		case marker == 0xFE: // comment
			keep = false
		case marker >= 0xE1 && marker <= 0xEF:
			keep = marker == 0xEE || (marker == 0xE2 && bytes.HasPrefix(seg[4:], []byte("ICC_PROFILE\x00")))
		}
		if keep {
			out.Write(seg)
		}
		i += 2 + n
	}
}

// jpegOrientation finds the orientation in a JPEG's Exif segment, or 0.
func jpegOrientation(b []byte) int {
	for i := 2; i+4 <= len(b) && b[i] == 0xFF; {
		marker := b[i+1]
		if marker == 0xDA || marker == 0xD9 {
			return 0
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return 0
		}
		if marker == 0xE1 {
			if o := exifOrientation(b[i+4 : i+2+n]); o > 0 {
				return o
			}
		}
		i += 2 + n
	}
	return 0
}

// stripWebP drops the EXIF and XMP chunks of a WebP file and clears their
// flags in the VP8X header.
func stripWebP(b []byte) ([]byte, error) {
	if len(b) < 12 || int(binary.LittleEndian.Uint32(b[4:8]))+8 > len(b) {
		return nil, ErrMalformed
	}
	out := bytes.NewBuffer(make([]byte, 0, len(b)))
	out.Write(b[:12])
	end := int(binary.LittleEndian.Uint32(b[4:8])) + 8
	for i := 12; i < end; {
		if i+8 > end {
			return nil, ErrMalformed
		}
		typ := string(b[i : i+4])
		n := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		size := 8 + n + n%2
		if n < 0 || i+8+n > end {
			return nil, ErrMalformed
		}
		chunk := b[i:min(i+size, end)]
		switch typ {
		case "EXIF", "XMP ":
		case "VP8X":
			c := append([]byte{}, chunk...)
			if len(c) > 8 {
				c[8] &^= 0x08 | 0x04 // EXIF and XMP present
			}
			out.Write(c)
		default:
			out.Write(chunk)
		}
		i += size
	}
	res := out.Bytes()
	binary.LittleEndian.PutUint32(res[4:8], uint32(len(res)-8))
	return res, nil
}

var pngSig = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// stripPNG drops tEXt, zTXt, iTXt, eXIf and tIME chunks, checking every
// chunk's CRC on the way.
func stripPNG(b []byte) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, len(b)))
	out.Write(pngSig)
	i := len(pngSig)
	for i < len(b) {
		if i+12 > len(b) {
			return nil, ErrMalformed
		}
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n < 0 || i+12+n > len(b) {
			return nil, ErrMalformed
		}
		typ := string(b[i+4 : i+8])
		chunk := b[i : i+12+n]
		if crc32.ChecksumIEEE(chunk[4:8+n]) != binary.BigEndian.Uint32(chunk[8+n:]) {
			return nil, ErrMalformed
		}
		switch typ {
		case "tEXt", "zTXt", "iTXt", "eXIf", "tIME":
		default:
			out.Write(chunk)
		}
		i += 12 + n
		if typ == "IEND" {
			return out.Bytes(), nil
		}
	}
	return nil, ErrMalformed
}

// CopyTo streams a blob to w.
func (s Store) CopyTo(w io.Writer, sha string) error {
	b, err := s.Get(sha)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
