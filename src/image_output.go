package main

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	pngcodec "image/png"

	"github.com/chai2010/webp"
)

const maxGeneratedImageBytes = 24 << 20

type imageOutput struct {
	Format      string `json:"output_format,omitempty"`
	Compression *int   `json:"output_compression,omitempty"`
}

func (o *imageOutput) validate() error {
	switch o.Format {
	case "":
		o.Format = "png"
	case "jpg":
		o.Format = "jpeg"
	case "png", "jpeg", "webp":
	default:
		return errors.New("output_format must be png, jpeg or webp")
	}
	if o.Compression != nil && (o.Format == "png" || *o.Compression < 0 || *o.Compression > 100) {
		return errors.New("output_compression must be 0–100 and requires jpeg or webp output")
	}
	return nil
}

type imageBuffer struct{ bytes.Buffer }

func (b *imageBuffer) Write(p []byte) (int, error) {
	if len(p) > maxGeneratedImageBytes-b.Len() {
		return 0, errors.New("encoded image exceeds the size limit")
	}
	return b.Buffer.Write(p)
}

func (o imageOutput) encode(data []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if format == o.Format && o.Compression == nil {
		return data, nil
	}
	// Bound the decoded raster before allocating memory for conversion.
	const maxPixels = 16 << 20
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPixels/cfg.Height {
		return nil, errors.New("generated image exceeds conversion pixel limit")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	quality := 100
	if o.Compression != nil {
		quality = *o.Compression
	}
	var out imageBuffer
	switch o.Format {
	case "png":
		err = pngcodec.Encode(&out, img)
	case "jpeg":
		if src, ok := img.(interface{ Opaque() bool }); !ok || !src.Opaque() {
			flat := image.NewRGBA(img.Bounds())
			draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
			draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
			img = flat
		}
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: max(1, quality)})
	case "webp":
		err = webp.Encode(&out, img, &webp.Options{Quality: float32(quality)})
	default:
		return nil, errors.New("unknown generated image format")
	}
	return out.Bytes(), err
}
