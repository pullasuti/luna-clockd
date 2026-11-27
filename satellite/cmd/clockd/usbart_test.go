package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// big covers come back as a 600 px JPEG with their colours; small ones as they are
func TestSmallArt(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := range 800 {
		for x := range 1200 {
			c := color.RGBA{200, 30, 30, 255} // red left half, blue right
			if x >= 600 {
				c = color.RGBA{30, 30, 200, 255}
			}
			big.SetRGBA(x, y, c)
		}
	}
	var jb bytes.Buffer
	jpeg.Encode(&jb, big, &jpeg.Options{Quality: 90})
	out, mime := smallArt(jb.Bytes(), "image/jpeg", 600)
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil || mime != "image/jpeg" {
		t.Fatal(err, mime)
	}
	if b := img.Bounds(); b.Dx() != 600 || b.Dy() != 400 {
		t.Errorf("size %v", b)
	}
	if r, _, bl, _ := img.At(100, 200).RGBA(); r>>8 < 170 || bl>>8 > 70 {
		t.Errorf("left side not red: %d %d", r>>8, bl>>8)
	}
	if r, _, bl, _ := img.At(500, 200).RGBA(); bl>>8 < 170 || r>>8 > 70 {
		t.Errorf("right side not blue: %d %d", r>>8, bl>>8)
	}
	var pb bytes.Buffer
	png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 300, 300)))
	if out, mime := smallArt(pb.Bytes(), "image/png", 600); !bytes.Equal(out, pb.Bytes()) || mime != "image/png" {
		t.Error("a small cover was changed")
	}
	if out, _ := smallArt([]byte("junk"), "image/png", 600); string(out) != "junk" {
		t.Error("undecodable data was changed")
	}
}
