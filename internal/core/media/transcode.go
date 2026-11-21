package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	imagedraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Transcoder adapts media bytes from srcMIME into a format accepted by spec.
// Returning (nil, "", nil) means "cannot transcode this media class; caller
// should decide whether to pass through or refuse."
type Transcoder interface {
	Transcode(src []byte, srcMIME string, spec TargetSpec) (out []byte, outMIME string, err error)
}

// ImageTranscoder is the pure-Go default transcoder supporting JPEG, PNG, GIF,
// and WebP inputs, emitting JPEG or PNG and optionally stepping down quality
// and dimensions to fit spec.MaxBytes.
type ImageTranscoder struct{}

// Transcode converts an image into spec.PreferMIME (or the first supported
// image format in spec.AcceptMIME) and fits it within spec.MaxBytes.
func (ImageTranscoder) Transcode(src []byte, srcMIME string, spec TargetSpec) ([]byte, string, error) {
	if !strings.HasPrefix(strings.ToLower(srcMIME), "image/") {
		return nil, "", nil
	}
	target := chooseImageTargetMIME(srcMIME, spec)
	if target == "" {
		return nil, "", fmt.Errorf("media transcode: no supported target image format in %v", spec.AcceptMIME)
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", fmt.Errorf("media transcode: decode %s: %w", srcMIME, err)
	}

	if target == "image/png" {
		out, err := encodePNG(img)
		if err != nil {
			return nil, "", err
		}
		if spec.MaxBytes <= 0 || int64(len(out)) <= spec.MaxBytes {
			return out, "image/png", nil
		}
		// Downscale until it fits (up to 4 passes).
		cur := img
		for pass := 0; pass < 4; pass++ {
			cur = downscale(cur, 0.75)
			out, err = encodePNG(cur)
			if err != nil {
				return nil, "", err
			}
			if int64(len(out)) <= spec.MaxBytes {
				return out, "image/png", nil
			}
		}
		return nil, "", fmt.Errorf("%w: png remains %d bytes after downscale (limit %d)", ErrTooLarge, len(out), spec.MaxBytes)
	}

	// Default target: image/jpeg. Composite onto white so transparent PNG/WebP
	// pixels don't turn black in JPEG.
	opaque := flattenAlphaWhite(img)
	qualities := []int{88, 76, 62}
	for _, q := range qualities {
		out, err := encodeJPEG(opaque, q)
		if err != nil {
			return nil, "", err
		}
		if spec.MaxBytes <= 0 || int64(len(out)) <= spec.MaxBytes {
			return out, "image/jpeg", nil
		}
	}
	// Still over MaxBytes: downscale and re-encode at quality 75.
	cur := image.Image(opaque)
	var last []byte
	for pass := 0; pass < 5; pass++ {
		cur = downscale(cur, 0.7)
		out, err := encodeJPEG(cur, 75)
		if err != nil {
			return nil, "", err
		}
		last = out
		if int64(len(out)) <= spec.MaxBytes {
			return out, "image/jpeg", nil
		}
	}
	return nil, "", fmt.Errorf("%w: jpeg remains %d bytes after downscale (limit %d)", ErrTooLarge, len(last), spec.MaxBytes)
}

func chooseImageTargetMIME(srcMIME string, spec TargetSpec) string {
	pref := strings.ToLower(strings.TrimSpace(spec.PreferMIME))
	if pref == "image/jpeg" || pref == "image/png" {
		return pref
	}
	for _, m := range spec.AcceptMIME {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "image/jpeg" || m == "image/png" {
			return m
		}
	}
	// Size-only adaptation (AcceptMIME empty): keep PNG if source is PNG and
	// JPEG otherwise.
	if len(spec.AcceptMIME) == 0 {
		if strings.ToLower(srcMIME) == "image/png" {
			return "image/jpeg" // JPEG compresses dramatically better when fitting MaxBytes
		}
		return "image/jpeg"
	}
	return ""
}

func flattenAlphaWhite(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	imagedraw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, imagedraw.Src)
	imagedraw.Draw(dst, dst.Bounds(), src, b.Min, imagedraw.Over)
	return dst
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("media transcode: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("media transcode: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func downscale(src image.Image, factor float64) image.Image {
	b := src.Bounds()
	w := int(math.Max(1, math.Round(float64(b.Dx())*factor)))
	h := int(math.Max(1, math.Round(float64(b.Dy())*factor)))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}
