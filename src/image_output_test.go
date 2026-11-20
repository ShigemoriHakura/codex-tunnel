package main

import (
	"bytes"
	"image"
	"image/color"
	pngcodec "image/png"
	"strings"
	"testing"
)

func TestGeneratedImageFormats(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	src.SetNRGBA(1, 1, color.NRGBA{R: 120, G: 200, B: 40, A: 255})
	var original bytes.Buffer
	if err := pngcodec.Encode(&original, src); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "png", "jpg", "jpeg", "webp"} {
		opts := imageOutput{Format: name}
		if err := opts.validate(); err != nil {
			t.Fatal(err)
		}
		data, err := opts.encode(original.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		got, typ, err := image.Decode(bytes.NewReader(data))
		if err != nil || typ != opts.Format || got.Bounds() != src.Bounds() {
			t.Fatalf("invalid %q result, format=%q, err=%v", name, typ, err)
		}
		if typ == "png" && !bytes.Equal(data, original.Bytes()) {
			t.Fatal("PNG passthrough changed an existing image")
		}
		if typ == "jpeg" {
			r, g, b, a := got.At(0, 0).RGBA()
			if a != 0xffff || min(r, g, b) < 0xb000 {
				t.Fatal("transparent pixels were not flattened for JPEG")
			}
		}
		if typ == "webp" {
			_, _, _, a := got.At(0, 0).RGBA()
			if a != 0 {
				t.Fatal("lost alpha during WebP conversion")
			}
		}
		converted, err := (imageOutput{Format: "png"}).encode(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pngcodec.Decode(bytes.NewReader(converted)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidImageOptionsDoNotStartCLI(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	for _, req := range []string{
		`{"prompt":"draw","output_format":"pdf"}`,
		`{"prompt":"draw","output_format":"png","output_compression":50}`,
		`{"prompt":"draw","output_format":"jpeg","output_compression":101}`,
		`{"prompt":"draw","output_format":"webp","output_compression":-1}`,
	} {
		body := hit(t, a, "POST", "/v1/images/generations", key, req, "", 400)
		if !strings.Contains(body, `"code":"invalid_image_output"`) {
			t.Fatal(body)
		}
	}
	var count int
	if err := a.db.QueryRow("SELECT count(*) FROM requests").Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid inputs reached the generator", err)
	}
}

func TestCLIImageOptions(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	for _, field := range []string{`"model":"gpt-image-2.5-sunburst"`, `"size":"1024x1024"`, `"quality":"high"`, `"stream":true,"partial_images":1`} {
		hit(t, a, "POST", "/v1/images/generations", key, `{"prompt":"draw",`+field+"}", "", 400)
	}
}
