package output

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// a tiny PNG header is enough for the sniffing
var pngData = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 40)...)
var jpgData = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 40)...)

func flacWithPictures(pics ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("fLaC")
	b.Write([]byte{0, 0, 0, 34}) // STREAMINFO, not last
	b.Write(make([]byte, 34))
	for i, p := range pics {
		var body bytes.Buffer
		typ := uint32(4) // back cover...
		if i == len(pics)-1 {
			typ = 3 // ...then the front one
		}
		for _, v := range []uint32{typ, 9} {
			binary.Write(&body, binary.BigEndian, v)
		}
		body.WriteString("image/xyz")
		binary.Write(&body, binary.BigEndian, uint32(0)) // description
		for range 4 {
			binary.Write(&body, binary.BigEndian, uint32(0))
		}
		binary.Write(&body, binary.BigEndian, uint32(len(p)))
		body.Write(p)
		hdr := byte(6)
		if i == len(pics)-1 {
			hdr |= 0x80
		}
		n := body.Len()
		b.Write([]byte{hdr, byte(n >> 16), byte(n >> 8), byte(n)})
		b.Write(body.Bytes())
	}
	return b.Bytes()
}

func id3WithPicture(ver byte, utf16 bool, pic []byte) []byte {
	var f bytes.Buffer
	if utf16 {
		f.Write([]byte{1})
		f.WriteString("image/png\x00")
		f.Write([]byte{3})
		f.Write([]byte{0xFF, 0xFE, 'c', 0, 0, 0}) // "c" in UTF-16LE, then 00 00
	} else {
		f.Write([]byte{0})
		f.WriteString("image/png\x00")
		f.Write([]byte{3})
		f.WriteString("cover\x00")
	}
	f.Write(pic)
	frame := []byte("APIC")
	n := f.Len()
	if ver == 4 {
		frame = append(frame, byte(n>>21)&0x7F, byte(n>>14)&0x7F, byte(n>>7)&0x7F, byte(n)&0x7F, 0, 0)
	} else {
		frame = binary.BigEndian.AppendUint32(frame, uint32(n))
		frame = append(frame, 0, 0)
	}
	frame = append(frame, f.Bytes()...)
	title := []byte("TIT2\x00\x00\x00\x03\x00\x00\x03Hi")
	body := append(title, frame...)
	t := len(body)
	h := []byte{'I', 'D', '3', ver, 0, 0, byte(t>>21) & 0x7F, byte(t>>14) & 0x7F, byte(t>>7) & 0x7F, byte(t) & 0x7F}
	return append(h, body...)
}

func TestCoverArt(t *testing.T) {
	for _, tc := range []struct {
		name string
		file []byte
		want []byte
		mime string
	}{
		{"flac front cover", flacWithPictures(jpgData, pngData), pngData, "image/png"},
		{"flac only picture", flacWithPictures(jpgData), jpgData, "image/jpeg"},
		{"id3v2.3 latin1", id3WithPicture(3, false, pngData), pngData, "image/png"},
		{"id3v2.4 utf-16", id3WithPicture(4, true, jpgData), jpgData, "image/jpeg"},
		{"flac without", flacWithPictures(), nil, ""},
		{"not an image", flacWithPictures([]byte("hello, not a picture at all")), nil, ""},
	} {
		data, mime, ok := CoverArt(bytes.NewReader(tc.file))
		if ok != (tc.want != nil) || !bytes.Equal(data, tc.want) || mime != tc.mime {
			t.Errorf("%s: ok=%v mime=%q %d bytes", tc.name, ok, mime, len(data))
		}
	}
	// the tag walk still finds the title next to a picture
	if ti := id3v2Tags(3, id3WithPicture(3, false, pngData)[10:]).Title; ti != "Hi" {
		t.Errorf("title next to APIC: %q", ti)
	}
}

func TestLibraryTags(t *testing.T) {
	fr := func(id, text string) []byte {
		b := binary.BigEndian.AppendUint32([]byte(id), uint32(len(text)+1))
		return append(append(b, 0, 0, 3), text...)
	}
	var body []byte
	for _, f := range [][2]string{{"TIT2", "Song"}, {"TPE1", "Band"}, {"TALB", "Record"}, {"TPE2", "Various"}, {"TYER", "1991"}, {"TRCK", "03/12"}, {"TPOS", "2/2"}} {
		body = append(body, fr(f[0], f[1])...)
	}
	got := id3v2Tags(3, body)
	if got.Title != "Song" || got.Artist != "Band" || got.Album != "Record" || got.AlbumArtist != "Various" || got.Year != "1991" || got.Track != 3 || got.Disc != 2 {
		t.Errorf("%+v", got)
	}
	for in, want := range map[string]int{"7": 7, " 07 ": 7, "3/12": 3, "": 0, "A1": 0, "123456": 0} {
		if n := tagNum(in); n != want {
			t.Errorf("tagNum(%q) = %d, want %d", in, n, want)
		}
	}
}
