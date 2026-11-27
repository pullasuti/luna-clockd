package output

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hajimehoshi/go-mp3"
)

// MP3 playback (USB drive files, /api/play and HA URLs). The decoder is
// github.com/hajimehoshi/go-mp3 (Apache-2.0, pure Go; credited in the web UI):
// MPEG-1/2/2.5 Layer III, always 16-bit stereo out at the file's rate, which
// the shared resampler takes to 48 kHz. Length and tags come from our own
// header reader below (go-mp3 would scan the whole file for its length).

// isMP3 reports whether the first bytes look like an MP3: an ID3v2 tag or a
// Layer III frame sync. (ADTS AAC has the same sync but layer bits 00.)
func isMP3(b []byte) bool {
	if len(b) >= 3 && string(b[:3]) == "ID3" {
		return true
	}
	return len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0 && (b[1]>>1)&3 == 1
}

// DecodeMP3 streams an MP3 into b as 48 kHz stereo. It always closes b.
func DecodeMP3(r io.Reader, b *Buffer) (err error) {
	defer func() { b.Close(err) }()
	// a plain reader: go-mp3 scans a seekable source end to end at open
	d, err := mp3.NewDecoder(struct{ io.Reader }{r})
	if err != nil {
		return err
	}
	return pumpMP3(d, b)
}

// decodeMP3From starts skip into a seekable MP3 (resuming a USB track). The
// CALLER closes b. Seeking needs go-mp3's frame index, built by scanning the
// file once at open (a few hundred ms for a long file on the USB drive).
func decodeMP3From(r io.ReadSeeker, skip time.Duration, b *Buffer) error {
	d, err := mp3.NewDecoder(r)
	if err != nil {
		return err
	}
	if off := int64(skip.Seconds()*float64(d.SampleRate())) * 4; off > 0 {
		if l := d.Length(); l >= 0 && off >= l {
			return nil
		}
		if _, err := d.Seek(off, io.SeekStart); err != nil {
			return err
		}
	}
	return pumpMP3(d, b)
}

func pumpMP3(d *mp3.Decoder, b *Buffer) error {
	rs := newResampler(d.SampleRate())
	raw := make([]byte, 4608*4) // 4 frames of 1152 stereo 16-bit samples
	frames := make([]float32, 0, len(raw)/2)
	var carry []byte
	for {
		n, err := d.Read(raw[len(carry):])
		if n > 0 || len(carry) > 0 {
			copy(raw, carry)
			n += len(carry)
			whole := n &^ 3
			frames = frames[:0]
			for i := 0; i < whole; i += 2 {
				frames = append(frames, float32(int16(binary.LittleEndian.Uint16(raw[i:])))/32768)
			}
			carry = append(carry[:0], raw[whole:n]...)
			if out := rs.process(frames); len(out) > 0 && !b.Write(out) {
				return nil // aborted by the reader (stop, new media)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// mp3Header reads length and tags without decoding: ID3v2 (TIT2/TPE1, or
// v2.2 TT2/TP1) at the start, ID3v1 at the end when there is no v2 name; the
// length from the first frame's Xing/Info or VBRI header, else from the
// bitrate and the file size (constant-bitrate files). r should be seekable
// for the size and ID3v1; without it only ID3v2 and VBR headers count.
func mp3Header(r io.Reader) (info TrackInfo, err error) {
	var size int64 = -1
	if s, ok := r.(io.Seeker); ok {
		if cur, err := s.Seek(0, io.SeekCurrent); err == nil {
			if end, err := s.Seek(0, io.SeekEnd); err == nil {
				size = end
				// ID3v1: the last 128 bytes
				if end >= 128 {
					var v1 [128]byte
					if _, err := s.Seek(end-128, io.SeekStart); err == nil {
						if _, err := io.ReadFull(r, v1[:]); err == nil && string(v1[:3]) == "TAG" {
							info.Title, info.Artist, info.Album = strings.TrimSpace(latin1(v1[3:33])), strings.TrimSpace(latin1(v1[33:63])), strings.TrimSpace(latin1(v1[63:93]))
							info.Year = strings.TrimSpace(latin1(v1[93:97]))
							if v1[125] == 0 && v1[126] != 0 { // ID3v1.1: the track number
								info.Track = int(v1[126])
							}
							info.Name = joinName(info.Title, info.Artist)
							size -= 128
						}
					}
				}
			}
			if _, err := s.Seek(cur, io.SeekStart); err != nil {
				return info, err
			}
		}
	}
	// 64 KB covers the tag (unless it carries cover art) and the first frame
	head := make([]byte, 64<<10)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return info, err
	}
	head = head[:n]
	start := 0
	if len(head) >= 10 && string(head[:3]) == "ID3" {
		tagLen := 10 + synchsafe(head[6:10])
		if head[5]&0x10 != 0 { // footer
			tagLen += 10
		}
		end := min(tagLen, len(head))
		if t := id3v2Tags(head[3], head[10:end]); t.Title != "" || t.Artist != "" || t.Album != "" {
			info.Title, info.Artist, info.Album = t.Title, t.Artist, t.Album
			info.AlbumArtist, info.Year, info.Track, info.Disc = t.AlbumArtist, t.Year, t.Track, t.Disc
			info.Name = joinName(t.Title, t.Artist)
		}
		start = tagLen
		if start+4 > len(head) {
			// big tag (cover art): read the first frame from after it
			if s, ok := r.(io.Seeker); ok {
				if _, err := s.Seek(int64(start), io.SeekStart); err == nil {
					m, _ := io.ReadFull(r, head[:cap(head)])
					head, start = head[:m], 0
					size -= int64(tagLen)
					tagLen = 0
				}
			}
		}
		if tagLen > 0 {
			size -= int64(tagLen)
		}
	}
	// the first frame: a valid header whose follower is also a header (or EOF)
	for i := start; i+4 <= len(head); i++ {
		h, ok := parseMP3Frame(head[i:])
		if !ok {
			continue
		}
		if j := i + h.size; j+4 <= len(head) {
			if _, ok := parseMP3Frame(head[j:]); !ok {
				continue
			}
		}
		info.Rate = h.rate
		if frames := vbrFrames(head[i:], h); frames > 0 {
			info.Duration = time.Duration(frames) * time.Duration(h.samples) * time.Second / time.Duration(h.rate)
		} else if size > 0 && h.bitrate > 0 {
			audio := size - int64(i-start)
			info.Duration = time.Duration(audio*8) * time.Second / time.Duration(h.bitrate)
		}
		return info, nil
	}
	return info, errors.New("mp3: no frame found")
}

// findMP3Sync finds the first Layer III frame header followed by another
// one right after it (two in a row rule out a chance 0xFFE pattern), or -1.
func findMP3Sync(b []byte) int {
	for i := 0; i+4 <= len(b); i++ {
		h, ok := parseMP3Frame(b[i:])
		if !ok {
			continue
		}
		if j := i + h.size; j+4 <= len(b) {
			if n, ok := parseMP3Frame(b[j:]); ok && n.rate == h.rate {
				return i
			}
		}
	}
	return -1
}

type mp3Frame struct {
	mpeg1   bool
	mono    bool
	rate    int // Hz
	bitrate int // bit/s
	samples int // per frame
	size    int // bytes
}

var (
	mp3Rates    = [3][3]int{{44100, 48000, 32000}, {22050, 24000, 16000}, {11025, 12000, 8000}}
	mp3BitrateL = [2][16]int{
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}, // MPEG-1
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},     // MPEG-2/2.5
	}
)

// parseMP3Frame decodes a Layer III frame header.
func parseMP3Frame(b []byte) (f mp3Frame, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return f, false
	}
	ver := (b[1] >> 3) & 3 // 0: 2.5, 2: 2, 3: 1
	if ver == 1 || (b[1]>>1)&3 != 1 {
		return f, false
	}
	bi, ri := b[2]>>4, (b[2]>>2)&3
	if bi == 0 || bi == 15 || ri == 3 {
		return f, false
	}
	f.mpeg1 = ver == 3
	vi := map[byte]int{3: 0, 2: 1, 0: 2}[ver]
	f.rate = mp3Rates[vi][ri]
	if f.mpeg1 {
		f.bitrate = mp3BitrateL[0][bi] * 1000
		f.samples = 1152
	} else {
		f.bitrate = mp3BitrateL[1][bi] * 1000
		f.samples = 576
	}
	f.mono = b[3]>>6 == 3
	pad := int(b[2]>>1) & 1
	f.size = f.samples/8*f.bitrate/f.rate + pad
	return f, f.size > 4
}

// vbrFrames reads the frame count from a Xing/Info or VBRI header in the
// first frame (0 when there is none).
func vbrFrames(b []byte, f mp3Frame) int64 {
	side := 32
	switch {
	case f.mpeg1 && f.mono:
		side = 17
	case !f.mpeg1 && !f.mono:
		side = 17
	case !f.mpeg1 && f.mono:
		side = 9
	}
	if x := 4 + side; x+12 <= len(b) {
		if tag := string(b[x : x+4]); tag == "Xing" || tag == "Info" {
			if binary.BigEndian.Uint32(b[x+4:])&1 != 0 {
				return int64(binary.BigEndian.Uint32(b[x+8:]))
			}
			return 0
		}
	}
	if v := 36; v+18 <= len(b) && string(b[v:v+4]) == "VBRI" {
		return int64(binary.BigEndian.Uint32(b[v+14:]))
	}
	return 0
}

func synchsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// id3v2Tags finds the tags in an ID3v2 tag body (after the 10-byte header).
// Unsynchronised and compressed frames are skipped.
func id3v2Tags(ver byte, body []byte) (t TrackInfo) {
	id3Frames(ver, body, func(id string, data []byte) {
		switch id {
		case "TIT2", "TT2":
			t.Title = id3Text(data)
		case "TPE1", "TP1":
			t.Artist = id3Text(data)
		case "TALB", "TAL":
			t.Album = id3Text(data)
		case "TPE2", "TP2":
			t.AlbumArtist = id3Text(data)
		case "TDRC", "TYER", "TYE":
			if t.Year == "" {
				t.Year = id3Text(data)
			}
		case "TRCK", "TRK":
			t.Track = tagNum(id3Text(data))
		case "TPOS", "TPA":
			t.Disc = tagNum(id3Text(data))
		}
	})
	return t
}

// id3Frames calls fn with each readable frame of an ID3v2 tag body (after the
// 10-byte header): unsynchronised, compressed and encrypted frames are left out.
func id3Frames(ver byte, body []byte, fn func(id string, data []byte)) {
	idLen, hdrLen := 4, 10
	if ver == 2 {
		idLen, hdrLen = 3, 6
	}
	for len(body) >= hdrLen && body[0] != 0 {
		id := string(body[:idLen])
		var size int
		switch ver {
		case 2:
			size = int(body[3])<<16 | int(body[4])<<8 | int(body[5])
		case 3:
			size = int(binary.BigEndian.Uint32(body[4:8]))
		default:
			size = synchsafe(body[4:8])
		}
		if size < 0 || hdrLen+size > len(body) {
			break
		}
		data := body[hdrLen : hdrLen+size]
		plain := ver == 2 || body[9]&0x0E == 0 // not compressed/encrypted/unsynced (v2.4 bits; v2.3 0xC0)
		if ver == 3 {
			plain = body[9]&0xC0 == 0
		}
		if ver == 4 && plain && body[9]&0x01 != 0 && len(data) >= 4 {
			data = data[4:] // a data length indicator comes first
		}
		if plain {
			fn(id, data)
		}
		body = body[hdrLen+size:]
	}
}

// id3Text decodes an ID3v2 text frame (first value of a multi-value frame).
func id3Text(b []byte) string {
	if len(b) < 1 {
		return ""
	}
	enc, b := b[0], b[1:]
	var s string
	switch enc {
	case 1, 2: // UTF-16 with BOM / UTF-16BE
		be := enc == 2
		if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
			be, b = true, b[2:]
		} else if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
			be, b = false, b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			var c uint16
			if be {
				c = binary.BigEndian.Uint16(b[i:])
			} else {
				c = binary.LittleEndian.Uint16(b[i:])
			}
			if c == 0 {
				break
			}
			u = append(u, c)
		}
		s = string(utf16.Decode(u))
	case 3:
		s, _, _ = strings.Cut(string(b), "\x00")
	default:
		s = latin1(b)
	}
	return strings.ToValidUTF8(strings.TrimSpace(s), "")
}

func latin1(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return strings.TrimSpace(string(r))
}

// joinName is "Artist - Title", or whichever exists.
func joinName(title, artist string) string {
	title, artist = strings.TrimSpace(title), strings.TrimSpace(artist)
	switch {
	case artist == "":
		return title
	case title == "":
		return artist
	}
	return artist + " - " + title
}
