package web

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

func (b *browser) upload(path, filename string, data []byte, fields map[string]string) (int, string) {
	b.e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", b.csrf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		_, _, page := b.get(resp.Header.Get("Location"))
		return resp.StatusCode, page
	}
	return resp.StatusCode, string(body)
}

func photoWithGPS() []byte {
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4)), nil)
	p := buf.Bytes()
	exif := []byte("Exif\x00\x00GPSLatitude 40.71")
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(exif)+2))
	return append(append(append([]byte{}, p[:2]...), append(seg, exif...)...), p[2:]...)
}

func TestAttachThroughTheUI(t *testing.T) {
	e := newEnv(t)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "A1", LocationID: e.loc, Manufacturer: "Fluke", Model: "M"})
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	b.get("/assets/" + asset)

	_, page := b.upload("/assets/"+asset+"/attachments", "nameplate.jpg", photoWithGPS(), map[string]string{"description": "nameplate"})
	if !strings.Contains(page, "File attached") || !strings.Contains(page, "nameplate.jpg") {
		t.Fatalf("upload:\n%s", page)
	}
	link := regexp.MustCompile(`href="(/attachments/[0-9a-f-]{36})"`).FindStringSubmatch(page)
	if link == nil {
		t.Fatal("no download link")
	}
	resp, err := b.c.Get(e.srv.URL + link[1])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/jpeg" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download headers: %v", resp.Header)
	}
	if bytes.Contains(data, []byte("GPS")) {
		t.Fatal("location metadata survived upload")
	}

	_, page = b.upload("/assets/"+asset+"/attachments", "invoice.html", []byte("<html><script>alert(1)</script>"), nil)
	if !strings.Contains(page, "only JPEG, PNG and PDF files are accepted") {
		t.Fatalf("HTML upload:\n%s", page)
	}
	_, page = b.upload("/assets/"+asset+"/attachments", "huge.pdf", append([]byte("%PDF-"), make([]byte, 11<<20)...), nil)
	if !strings.Contains(page, "too large") && !strings.Contains(page, "at most 10 MiB") {
		t.Fatalf("oversize upload:\n%.300s", page)
	}
}
