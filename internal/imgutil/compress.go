package imgutil

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
)

type CompressedImage struct {
	JPEG       []byte
	OrigWidth  int
	OrigHeight int
	Width      int
	Height     int
}

// DecodeRawScreencap decodes Android's raw `screencap` RGBA_8888 / RGBX_8888 binary output.
// Modern Android (11..16) writes a 16-byte header: [width:u32, height:u32, format:u32, colorspace:u32].
// Older Android writes a 12-byte header: [width:u32, height:u32, format:u32].
func DecodeRawScreencap(raw []byte) (*image.RGBA, error) {
	if len(raw) < 16 {
		return nil, fmt.Errorf("raw screencap too short: %d bytes", len(raw))
	}
	w := int(binary.LittleEndian.Uint32(raw[0:4]))
	h := int(binary.LittleEndian.Uint32(raw[4:8]))
	format := binary.LittleEndian.Uint32(raw[8:12])
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return nil, fmt.Errorf("invalid raw screencap dimensions: %dx%d", w, h)
	}
	// PIXEL_FORMAT_RGBA_8888 = 1, PIXEL_FORMAT_RGBX_8888 = 2
	if format != 1 && format != 2 {
		return nil, fmt.Errorf("unsupported raw screencap pixel format: %d", format)
	}
	pixelBytes := w * h * 4
	var offset int
	switch len(raw) - pixelBytes {
	case 16:
		offset = 16
	case 12:
		offset = 12
	default:
		if len(raw) >= 16+pixelBytes {
			offset = 16
		} else if len(raw) >= 12+pixelBytes {
			offset = 12
		} else {
			return nil, fmt.Errorf("raw screencap size mismatch: got %d, expected %d+12/16", len(raw), pixelBytes)
		}
	}
	return &image.RGBA{
		Pix:    raw[offset : offset+pixelBytes],
		Stride: w * 4,
		Rect:   image.Rect(0, 0, w, h),
	}, nil
}

// CompressRawScreencap decodes raw RGBA screencap bytes, scales proportionally to targetWidth
// (never upscaling), and encodes as JPEG with the requested quality (1-100).
func CompressRawScreencap(raw []byte, targetWidth, quality int) (*CompressedImage, error) {
	rgba, err := DecodeRawScreencap(raw)
	if err != nil {
		return nil, err
	}
	return CompressImage(rgba, targetWidth, quality)
}

// CompressPNG decodes PNG bytes (e.g. `screencap -p` fallback), scales proportionally to targetWidth,
// and encodes as JPEG.
func CompressPNG(pngBytes []byte, targetWidth, quality int) (*CompressedImage, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode png: %w", err)
	}
	return CompressImage(img, targetWidth, quality)
}

// CompressImage resizes src proportionally to targetWidth (if src.Width > targetWidth)
// and encodes it as JPEG.
func CompressImage(src image.Image, targetWidth, quality int) (*CompressedImage, error) {
	if src == nil {
		return nil, errors.New("nil image")
	}
	b := src.Bounds()
	origW, origH := b.Dx(), b.Dy()
	if origW <= 0 || origH <= 0 {
		return nil, fmt.Errorf("invalid image bounds: %dx%d", origW, origH)
	}
	if quality <= 0 || quality > 100 {
		quality = 70
	}

	dstW, dstH := origW, origH
	if targetWidth > 0 && origW > targetWidth {
		dstW = targetWidth
		dstH = int(math.Round(float64(origH) * float64(targetWidth) / float64(origW)))
		if dstH < 1 {
			dstH = 1
		}
	}

	var outImg image.Image
	if dstW == origW && dstH == origH {
		outImg = src
	} else {
		outImg = resizeBilinear(src, dstW, dstH)
	}

	var buf bytes.Buffer
	buf.Grow(64 * 1024)
	if err := jpeg.Encode(&buf, outImg, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return &CompressedImage{
		JPEG:       buf.Bytes(),
		OrigWidth:  origW,
		OrigHeight: origH,
		Width:      dstW,
		Height:     dstH,
	}, nil
}

// resizeBilinear performs fast bilinear interpolation, with a specialized zero-allocation
// inner loop for *image.RGBA (which is what raw screencap and most PNGs produce).
func resizeBilinear(src image.Image, dstW, dstH int) *image.RGBA {
	sb := src.Bounds()
	srcW, srcH := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	scaleX := float64(srcW) / float64(dstW)
	scaleY := float64(srcH) / float64(dstH)

	// Precompute X sampling coordinates and fixed-point weights (0..256)
	x0 := make([]int, dstW)
	x1 := make([]int, dstW)
	wx := make([]uint32, dstW)
	for dx := 0; dx < dstW; dx++ {
		fx := (float64(dx)+0.5)*scaleX - 0.5
		if fx < 0 {
			fx = 0
		}
		ix := int(fx)
		if ix >= srcW-1 {
			ix = srcW - 1
			x0[dx] = ix
			x1[dx] = ix
			wx[dx] = 0
		} else {
			x0[dx] = ix
			x1[dx] = ix + 1
			wx[dx] = uint32((fx - float64(ix)) * 256.0)
		}
	}

	if rgba, ok := src.(*image.RGBA); ok {
		srcPix := rgba.Pix
		srcStride := rgba.Stride
		dstPix := dst.Pix
		dstStride := dst.Stride

		for dy := 0; dy < dstH; dy++ {
			fy := (float64(dy)+0.5)*scaleY - 0.5
			if fy < 0 {
				fy = 0
			}
			iy0 := int(fy)
			var iy1 int
			var wy uint32
			if iy0 >= srcH-1 {
				iy0 = srcH - 1
				iy1 = iy0
				wy = 0
			} else {
				iy1 = iy0 + 1
				wy = uint32((fy - float64(iy0)) * 256.0)
			}
			wyInv := 256 - wy
			row0 := (iy0+sb.Min.Y-rgba.Rect.Min.Y)*srcStride + (sb.Min.X-rgba.Rect.Min.X)*4
			row1 := (iy1+sb.Min.Y-rgba.Rect.Min.Y)*srcStride + (sb.Min.X-rgba.Rect.Min.X)*4
			dRow := dy * dstStride

			for dx := 0; dx < dstW; dx++ {
				w1 := wx[dx]
				w0 := 256 - w1
				i00 := row0 + x0[dx]*4
				i01 := row0 + x1[dx]*4
				i10 := row1 + x0[dx]*4
				i11 := row1 + x1[dx]*4

				w00 := w0 * wyInv
				w01 := w1 * wyInv
				w10 := w0 * wy
				w11 := w1 * wy

				r := (uint32(srcPix[i00])*w00 + uint32(srcPix[i01])*w01 + uint32(srcPix[i10])*w10 + uint32(srcPix[i11])*w11 + 32768) >> 16
				g := (uint32(srcPix[i00+1])*w00 + uint32(srcPix[i01+1])*w01 + uint32(srcPix[i10+1])*w10 + uint32(srcPix[i11+1])*w11 + 32768) >> 16
				b := (uint32(srcPix[i00+2])*w00 + uint32(srcPix[i01+2])*w01 + uint32(srcPix[i10+2])*w10 + uint32(srcPix[i11+2])*w11 + 32768) >> 16

				di := dRow + dx*4
				dstPix[di] = uint8(r)
				dstPix[di+1] = uint8(g)
				dstPix[di+2] = uint8(b)
				dstPix[di+3] = 255
			}
		}
		return dst
	}

	// Generic fallback for NRGBA/YCbCr/Paletted/etc.
	for dy := 0; dy < dstH; dy++ {
		fy := (float64(dy)+0.5)*scaleY - 0.5
		if fy < 0 {
			fy = 0
		}
		iy0 := int(fy)
		iy1 := iy0 + 1
		var wy uint32
		if iy0 >= srcH-1 {
			iy0 = srcH - 1
			iy1 = iy0
			wy = 0
		} else {
			wy = uint32((fy - float64(iy0)) * 256.0)
		}
		wyInv := 256 - wy
		dRow := dy * dst.Stride

		for dx := 0; dx < dstW; dx++ {
			w1 := wx[dx]
			w0 := 256 - w1
			r00, g00, b00, _ := src.At(sb.Min.X+x0[dx], sb.Min.Y+iy0).RGBA()
			r01, g01, b01, _ := src.At(sb.Min.X+x1[dx], sb.Min.Y+iy0).RGBA()
			r10, g10, b10, _ := src.At(sb.Min.X+x0[dx], sb.Min.Y+iy1).RGBA()
			r11, g11, b11, _ := src.At(sb.Min.X+x1[dx], sb.Min.Y+iy1).RGBA()

			w00 := w0 * wyInv
			w01 := w1 * wyInv
			w10 := w0 * wy
			w11 := w1 * wy

			r := ((r00>>8)*w00 + (r01>>8)*w01 + (r10>>8)*w10 + (r11>>8)*w11 + 32768) >> 16
			g := ((g00>>8)*w00 + (g01>>8)*w01 + (g10>>8)*w10 + (g11>>8)*w11 + 32768) >> 16
			b := ((b00>>8)*w00 + (b01>>8)*w01 + (b10>>8)*w10 + (b11>>8)*w11 + 32768) >> 16

			di := dRow + dx*4
			dst.Pix[di] = uint8(r)
			dst.Pix[di+1] = uint8(g)
			dst.Pix[di+2] = uint8(b)
			dst.Pix[di+3] = 255
		}
	}
	return dst
}
