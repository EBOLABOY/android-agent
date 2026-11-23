package imgutil

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestCompressRawScreencap1080x2400(t *testing.T) {
	w, h := 1080, 2400
	raw := make([]byte, 16+w*h*4)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(w))
	binary.LittleEndian.PutUint32(raw[4:8], uint32(h))
	binary.LittleEndian.PutUint32(raw[8:12], 1) // RGBA_8888
	binary.LittleEndian.PutUint32(raw[12:16], 1)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			off := 16 + (y*w+x)*4
			raw[off] = uint8(x % 256)
			raw[off+1] = uint8(y % 256)
			raw[off+2] = 128
			raw[off+3] = 255
		}
	}

	res, err := CompressRawScreencap(raw, 720, 70)
	if err != nil {
		t.Fatalf("CompressRawScreencap failed: %v", err)
	}
	if res.OrigWidth != 1080 || res.OrigHeight != 2400 {
		t.Fatalf("unexpected orig dims: %dx%d", res.OrigWidth, res.OrigHeight)
	}
	if res.Width != 720 || res.Height != 1600 {
		t.Fatalf("expected 720x1600, got %dx%d", res.Width, res.Height)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(res.JPEG))
	if err != nil {
		t.Fatalf("invalid jpeg output: %v", err)
	}
	if cfg.Width != 720 || cfg.Height != 1600 {
		t.Fatalf("jpeg header dims mismatch: %dx%d", cfg.Width, cfg.Height)
	}
}

func TestCompressPNGFallbackLandscape(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2400, 1080))
	for y := 0; y < 1080; y++ {
		for x := 0; x < 2400; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 40, G: 120, B: 220, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	res, err := CompressPNG(pngBuf.Bytes(), 720, 70)
	if err != nil {
		t.Fatalf("CompressPNG failed: %v", err)
	}
	if res.Width != 720 || res.Height != 324 {
		t.Fatalf("expected 720x324, got %dx%d", res.Width, res.Height)
	}
}
