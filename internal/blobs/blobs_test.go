package blobs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		img.Set(x, x, color.RGBA{200, 10, 10, 255})
	}
	return img
}

func TestStripJPEGMetadata(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, testImage(), nil)
	plain := buf.Bytes()
	// Insert an EXIF APP1 (with a fake GPS tag) and a comment after SOI.
	exif := append([]byte("Exif\x00\x00"), []byte("GPS 40.7128N 74.0060W")...)
	app1 := append([]byte{0xFF, 0xE1, 0, 0}, exif...)
	binary.BigEndian.PutUint16(app1[2:], uint16(len(exif)+2))
	com := []byte{0xFF, 0xFE, 0, 9, 'S', 'e', 'c', 'r', 'e', 't', '!'}
	dirty := append(append(append([]byte{}, plain[:2]...), append(app1, com...)...), plain[2:]...)

	mime, clean, err := Sanitize(dirty)
	if err != nil || mime != JPEG {
		t.Fatalf("Sanitize: %s %v", mime, err)
	}
	if bytes.Contains(clean, []byte("GPS")) || bytes.Contains(clean, []byte("Secret")) {
		t.Fatal("metadata survived")
	}
	if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
		t.Fatalf("cleaned JPEG no longer decodes: %v", err)
	}
	if !bytes.Equal(clean, plain) {
		t.Error("stripping changed more than the metadata")
	}
}

func pngChunk(typ string, data []byte) []byte {
	c := make([]byte, 8, 12+len(data))
	binary.BigEndian.PutUint32(c, uint32(len(data)))
	copy(c[4:], typ)
	c = append(c, data...)
	return binary.BigEndian.AppendUint32(c, crc32.ChecksumIEEE(c[4:]))
}

func TestStripPNGMetadata(t *testing.T) {
	var buf bytes.Buffer
	png.Encode(&buf, testImage())
	plain := buf.Bytes()
	// Insert a text chunk after IHDR (8 sig + 25 IHDR).
	text := pngChunk("tEXt", []byte("Comment\x00Patient room 4"))
	dirty := append(append(append([]byte{}, plain[:33]...), text...), plain[33:]...)
	mime, clean, err := Sanitize(dirty)
	if err != nil || mime != PNG {
		t.Fatalf("Sanitize: %s %v", mime, err)
	}
	if bytes.Contains(clean, []byte("Patient")) || !bytes.Equal(clean, plain) {
		t.Fatal("text chunk survived or image changed")
	}
	if _, err := png.Decode(bytes.NewReader(clean)); err != nil {
		t.Fatal(err)
	}
	// A corrupted chunk is refused.
	dirty[40] ^= 0xFF
	if _, _, err := Sanitize(dirty); !errors.Is(err, ErrMalformed) {
		t.Fatalf("corrupt PNG: %v", err)
	}
}

func TestSanitizeTypes(t *testing.T) {
	if mime, _, err := Sanitize([]byte("%PDF-1.7\n...")); err != nil || mime != PDF {
		t.Errorf("PDF: %s %v", mime, err)
	}
	for _, b := range [][]byte{[]byte("<html><script>"), []byte("MZ\x90\x00"), []byte("GIF89a"), {0xFF, 0xD8, 0xFF, 0xE0, 0x00}} {
		if _, _, err := Sanitize(b); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	if _, _, err := Sanitize(make([]byte, MaxSize+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize: %v", err)
	}
}

func TestStore(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	sha, err := s.Put([]byte("%PDF-1.4 certificate"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has(sha) {
		t.Fatal("not stored")
	}
	if b, err := s.Get(sha); err != nil || string(b) != "%PDF-1.4 certificate" {
		t.Fatalf("Get: %q %v", b, err)
	}
	if err := s.PutExpected(sha[:63]+"0", []byte("other")); err == nil {
		t.Fatal("stored content under the wrong hash")
	}
	list, _ := s.List()
	if len(list) != 1 || list[0] != sha {
		t.Fatalf("List = %v", list)
	}
	s.Remove(sha)
	if _, err := s.Get(sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Remove: %v", err)
	}
	if _, err := s.Get("../../etc/passwd"); err == nil {
		t.Fatal("path traversal")
	}
}

// A phone photo turned on its side keeps that (so it shows the right way
// up), and nothing else from its Exif.
func TestJPEGKeepsOrientationOnly(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, testImage(), nil)
	plain := buf.Bytes()
	// Exif, little-endian (as iPhones write it): IFD0 with Orientation 6,
	// a make and a GPS pointer, then some "GPS" text.
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 3, 0,
		0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, // Orientation = 6
		0x0F, 0x01, 2, 0, 6, 0, 0, 0, 50, 0, 0, 0, // Make, at 50
		0x25, 0x88, 4, 0, 1, 0, 0, 0, 56, 0, 0, 0, // GPS IFD pointer
		0, 0, 0, 0}
	tiff = append(tiff, []byte("Apple\x00GPS 40.7128N")...)
	exif := append([]byte("Exif\x00\x00"), tiff...)
	app1 := append([]byte{0xFF, 0xE1, 0, 0}, exif...)
	binary.BigEndian.PutUint16(app1[2:], uint16(len(exif)+2))
	dirty := append(append(append([]byte{}, plain[:2]...), app1...), plain[2:]...)

	_, clean, err := Sanitize(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clean, []byte("GPS")) || bytes.Contains(clean, []byte("Apple")) {
		t.Fatal("metadata survived")
	}
	if jpegOrientation(clean) != 6 {
		t.Fatalf("orientation = %d", jpegOrientation(clean))
	}
	if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
		t.Fatalf("cleaned JPEG no longer decodes: %v", err)
	}
	// Upright photos get no Exif at all.
	if _, clean, _ := Sanitize(plain); !bytes.Equal(clean, plain) {
		t.Fatal("an upright photo was changed")
	}
}

func webpChunk(typ string, data []byte) []byte {
	c := make([]byte, 8, 8+len(data)+1)
	copy(c, typ)
	binary.LittleEndian.PutUint32(c[4:], uint32(len(data)))
	c = append(c, data...)
	if len(data)%2 == 1 {
		c = append(c, 0)
	}
	return c
}

func TestStripWebPMetadata(t *testing.T) {
	vp8x := webpChunk("VP8X", []byte{0x08 | 0x04, 0, 0, 0, 7, 0, 0, 7, 0, 0})
	body := append(append(append(append([]byte("WEBP"), vp8x...), webpChunk("VP8 ", []byte("imagedata!"))...),
		webpChunk("EXIF", []byte("GPS 40.7128N"))...), webpChunk("XMP ", []byte("<x>secret</x>"))...)
	file := append([]byte("RIFF\x00\x00\x00\x00"), body...)
	binary.LittleEndian.PutUint32(file[4:], uint32(len(body)))
	mime, clean, err := Sanitize(file)
	if err != nil || mime != WebP {
		t.Fatalf("%s %v", mime, err)
	}
	if bytes.Contains(clean, []byte("GPS")) || bytes.Contains(clean, []byte("secret")) || !bytes.Contains(clean, []byte("imagedata!")) {
		t.Fatalf("clean = %q", clean)
	}
	if clean[20]&(0x08|0x04) != 0 {
		t.Fatal("VP8X still says EXIF/XMP are present")
	}
	if int(binary.LittleEndian.Uint32(clean[4:]))+8 != len(clean) {
		t.Fatal("RIFF size not updated")
	}
}

func TestHEICIsExplained(t *testing.T) {
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...)
	if _, _, err := Sanitize(heic); !errors.Is(err, ErrHEIC) || !bytes.Contains([]byte(err.Error()), []byte("Most Compatible")) {
		t.Fatalf("err = %v", err)
	}
}
