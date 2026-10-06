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
	PDF  = "application/pdf"
)

var (
	ErrTooLarge    = fmt.Errorf("blobs: files must be at most %d MiB", MaxSize>>20)
	ErrType        = errors.New("blobs: only JPEG, PNG and PDF files are accepted")
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
// carry location or personal details: EXIF/XMP/comments from JPEG and
// text/EXIF/time chunks from PNG. PDFs are accepted as they are.
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
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return PDF, data, nil
	}
	return "", nil, ErrType
}

// stripJPEG drops APPn segments other than JFIF (APP0), ICC profiles
// (APP2 "ICC_PROFILE") and Adobe colour info (APP14), and COM segments.
// Everything from the start of scan on is copied unchanged.
func stripJPEG(b []byte) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, len(b)))
	out.Write(b[:2]) // SOI
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
