package main

// Cover art for the drive's music in the browser (Player, lock screen):
// the picture embedded in the file, else an image next to it (folder.jpg and
// friends), scaled down so a phone gets a small JPEG.

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // covers come as JPEG or PNG
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"lenovo-satellite/internal/output"
)

// artSide: the longest side the browser gets (the lock screen shows ~512 px).
const artSide = 600

// artNames: images in a folder taken as its cover, best first.
var artNames = []string{"cover", "folder", "front", "album", "albumart", "albumartlarge"}

var errNoArt = errors.New("no cover art")

// FolderArt reads a cover image from a folder of the drive (confined to the
// mount, no symlinks): the first of artNames with .jpg, .jpeg or .png, any case.
func (u *usbSock) FolderArt(dir string) ([]byte, error) {
	if _, _, _, ok := usbMounted(); !ok {
		return nil, errors.New("no drive mounted")
	}
	full := filepath.Join(usbMount, filepath.Clean("/"+strings.TrimPrefix(dir, usbMount)))
	if full != usbMount && !strings.HasPrefix(full, usbMount+string(filepath.Separator)) {
		return nil, errors.New("path outside the drive")
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	found := map[string]string{}
	for _, e := range ents {
		n := strings.ToLower(e.Name())
		ext := filepath.Ext(n)
		if !e.Type().IsRegular() || (ext != ".jpg" && ext != ".jpeg" && ext != ".png") {
			continue
		}
		found[strings.TrimSuffix(n, ext)] = e.Name()
	}
	for _, n := range artNames {
		if name, ok := found[n]; ok {
			f, err := os.Open(filepath.Join(full, name))
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return io.ReadAll(io.LimitReader(f, 16<<20))
		}
	}
	return nil, errNoArt
}

type artEntry struct {
	key  string
	data []byte // nil: the file has none
	mime string
}

// artCache keeps the last few covers (a queue asks for the same album's again
// and again); misses are kept too.
var artCache struct {
	sync.Mutex
	list []artEntry
}

// USBArt: the cover for a drive file, embedded first, then from its folder.
func (b *webBackend) USBArt(file string, side int) ([]byte, string, error) {
	if side <= 0 || side > artSide {
		side = artSide
	}
	side = max(side, 64)
	f, p, err := b.usb.OpenAudio(file)
	if err != nil {
		return nil, "", err
	}
	key := b.usb.Drive() + p + "@" + strconv.Itoa(side)
	artCache.Lock()
	for _, e := range artCache.list {
		if e.key == key {
			artCache.Unlock()
			f.Close()
			if e.data == nil {
				return nil, "", errNoArt
			}
			return e.data, e.mime, nil
		}
	}
	artCache.Unlock()
	data, mime, ok := output.CoverArt(f)
	f.Close()
	if !ok {
		if raw, err := b.usb.FolderArt(filepath.Dir(p)); err == nil {
			data, mime = raw, "image/jpeg"
			if bytes.HasPrefix(raw, []byte("\x89PNG")) {
				mime = "image/png"
			}
			ok = true
		}
	}
	e := artEntry{key: key}
	if ok {
		e.data, e.mime = smallArt(data, mime, side)
	}
	artCache.Lock()
	artCache.list = append(artCache.list, e)
	if len(artCache.list) > 96 { // thumbnails ~10 kB, covers ~50 kB
		artCache.list = artCache.list[1:]
	}
	artCache.Unlock()
	if e.data == nil {
		return nil, "", errNoArt
	}
	return e.data, e.mime, nil
}

// smallArt scales a cover down to side (a JPEG then). Small ones, huge ones
// (not worth the memory) and undecodable ones go out as they are.
func smallArt(data []byte, mime string, side int) ([]byte, string) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || max(cfg.Width, cfg.Height) <= side || cfg.Width*cfg.Height > 40_000_000 {
		return data, mime
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, shrink(img, side), &jpeg.Options{Quality: 85}); err != nil {
		return data, mime
	}
	return out.Bytes(), "image/jpeg"
}

// shrink scales img so its longer side is side, averaging up to 4x4 samples
// per output pixel (a box filter, enough for a cover).
func shrink(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := side, h*side/w
	if h > w {
		dw, dh = w*side/h, side
	}
	dw, dh = max(dw, 1), max(dh, 1)
	at := img.At
	if yc, ok := img.(*image.YCbCr); ok { // JPEG: skip the interface per pixel
		at = func(x, y int) color.Color {
			yi, ci := yc.YOffset(x, y), yc.COffset(x, y)
			r, g, bl := color.YCbCrToRGB(yc.Y[yi], yc.Cb[ci], yc.Cr[ci])
			return color.RGBA{r, g, bl, 255}
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := range dh {
		y0, y1 := b.Min.Y+dy*h/dh, b.Min.Y+(dy+1)*h/dh
		for dx := range dw {
			x0, x1 := b.Min.X+dx*w/dw, b.Min.X+(dx+1)*w/dw
			sx, sy := max(1, (x1-x0)/4), max(1, (y1-y0)/4)
			var r, g, bl, n uint32
			for y := y0; y < max(y1, y0+1); y += sy {
				for x := x0; x < max(x1, x0+1); x += sx {
					cr, cg, cb, _ := at(x, y).RGBA()
					r, g, bl, n = r+cr, g+cg, bl+cb, n+1
				}
			}
			dst.SetRGBA(dx, dy, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	return dst
}
