package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	pngcodec "image/png"
	"io"
	"net/http"
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

func TestImageProviderErrorsAndWorkspace(t *testing.T) {
	for _, origin := range []string{"https://chatgpt.com.example.org", "https://chatgpt.com@other.invalid", "http://chatgpt.com", "https://chatgpt.com:7443", "https://chatgpt.com/extra"} {
		if _, err := (imageWorkspace{origin, "fixture", "NO_CONSTRAINT"}).endpoint(); err == nil {
			t.Fatal("accepted an unexpected workspace origin", origin)
		}
	}
	resp := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"model_not_available","message":"private diagnostic text"}}`))}
	_, err := readProviderImage(resp, imageRequest{Model: "gpt-image-2.5-sunburst"})
	if err == nil || errorStatus(context.Background(), err) != 403 || !strings.Contains(err.Error(), "model_not_available") || strings.Contains(err.Error(), "private") {
		t.Fatalf("incorrect provider error: %v", err)
	}
	resp = &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"model":"gpt-image-2","data":[{"b64_json":"` + png + `"}]}`))}
	if _, err = readProviderImage(resp, imageRequest{Model: "gpt-image-2.5-sunburst"}); err == nil {
		t.Fatal("accepted the wrong image model")
	}
}
