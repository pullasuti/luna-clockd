package output

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strings"

	"github.com/mewkiz/flac/meta"
)

// maxArt bounds an embedded picture (and an ID3 tag read for one): covers are
// a few hundred kB, anything far bigger is not worth holding in memory.
const maxArt = 16 << 20

// CoverArt finds the picture embedded in a FLAC or MP3 file: the front cover
// when there are several, else the first. It reads only the tags. mime comes
// from the data itself (image/jpeg, image/png, ...); ok is false without one.
func CoverArt(r io.ReadSeeker) (data []byte, mime string, ok bool) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, "", false
	}
	switch {
	case string(magic[:]) == "fLaC":
		data = flacArt(r)
	case string(magic[:3]) == "ID3":
		if _, err := r.Seek(0, io.SeekStart); err == nil {
			data = id3Art(r)
		}
	}
	return imageData(data)
}

// imageData accepts data only when it sniffs as an image.
func imageData(data []byte) ([]byte, string, bool) {
	if len(data) == 0 {
		return nil, "", false
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", false
	}
	return data, mime, true
}

// flacArt walks the metadata blocks after the "fLaC" magic for PICTURE ones.
func flacArt(r io.Reader) []byte {
	var first []byte
	for {
		block, err := meta.New(r)
		if err != nil {
			return first
		}
		if block.Type == meta.TypePicture && block.Length <= maxArt {
			if err := block.Parse(); err != nil {
				return first
			}
			if p, ok := block.Body.(*meta.Picture); ok && p.MIME != "-->" && len(p.Data) > 0 {
				if p.Type == 3 { // front cover
					return p.Data
				}
				if first == nil {
					first = p.Data
				}
			}
		} else if err := block.Skip(); err != nil {
			return first
		}
		if block.IsLast {
			return first
		}
	}
}

// id3Art reads an ID3v2 tag from the start of r for APIC (v2.3/2.4) or PIC
// (v2.2) frames.
func id3Art(r io.Reader) []byte {
	var h [10]byte
	if _, err := io.ReadFull(r, h[:]); err != nil || string(h[:3]) != "ID3" {
		return nil
	}
	if h[5]&0x80 != 0 { // the whole tag unsynchronised: pictures would come out wrong
		return nil
	}
	n := synchsafe(h[6:10])
	if n <= 0 || n > maxArt {
		return nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil
	}
	if h[5]&0x40 != 0 && len(body) >= 4 { // an extended header before the frames
		x := int(binary.BigEndian.Uint32(body[:4])) + 4 // v2.3: its size leaves out these 4 bytes
		if h[3] == 4 {
			x = synchsafe(body[:4])
		}
		if x < 0 || x > len(body) {
			return nil
		}
		body = body[x:]
	}
	var front, first []byte
	id3Frames(h[3], body, func(id string, data []byte) {
		if id != "APIC" && id != "PIC" {
			return
		}
		pic, typ := id3Picture(id == "PIC", data)
		if len(pic) == 0 {
			return
		}
		if typ == 3 && front == nil {
			front = pic
		}
		if first == nil {
			first = pic
		}
	})
	if front != nil {
		return front
	}
	return first
}

// id3Picture takes an APIC/PIC frame apart: text encoding, MIME type (PIC: a
// 3-letter format), picture type, description, then the image.
func id3Picture(v22 bool, b []byte) (data []byte, typ byte) {
	if len(b) < 4 {
		return nil, 0
	}
	enc := b[0]
	b = b[1:]
	if v22 {
		b = b[3:]
	} else {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, 0
		}
		b = b[i+1:]
	}
	if len(b) < 1 {
		return nil, 0
	}
	typ, b = b[0], b[1:]
	// the description ends with one zero byte, or two (aligned) in UTF-16
	if enc == 1 || enc == 2 {
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return b[i+2:], typ
			}
		}
		return nil, 0
	}
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil, 0
	}
	return b[i+1:], typ
}
