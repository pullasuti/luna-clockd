package output

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/meta"
)

// TrackInfo is what the USB player reads from a file's header: its length and
// tags, for the transport line and the clock face.
type TrackInfo struct {
	Duration time.Duration
	Rate     int
	// Name is "Artist - Title" (or just one of them) from the tags (FLAC
	// Vorbis comments, ID3); "" when the file has none.
	Name string
	// the tags one by one ("" when missing), for players that show them apart
	Title, Artist, Album string
	// for a music library: album artist, year (as tagged, usually "2006" or a
	// date), track and disc number (0 when missing)
	AlbumArtist, Year string
	Track, Disc       int
}

// tagNum reads a track or disc number ("3", "03", "3/12"); 0 when there is none.
func tagNum(s string) int {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "/")
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		if n > 9999 {
			return 0
		}
	}
	return n
}

// TrackMeta reads a WAV, FLAC or MP3 header without touching the audio:
// length, sample rate and tags. Cheap enough to run per file when a folder is
// queued (cover-art blocks are skipped unread). WAV carries no tags here.
// Pass the file itself (an io.ReadSeeker) so MP3 can read its size and ID3v1.
func TrackMeta(r io.Reader) (info TrackInfo, err error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return info, err
	}
	rest := io.MultiReader(bytes.NewReader(magic[:]), r)
	if isMP3(magic[:]) {
		if s, ok := r.(io.Seeker); ok {
			if _, err := s.Seek(-4, io.SeekCurrent); err == nil {
				return mp3Header(r)
			}
		}
		return mp3Header(rest)
	}
	switch string(magic[:]) {
	case "fLaC":
		return flacHeader(rest)
	default:
		_, _, ch, rate, bits, dataSize, err := wavData(rest)
		if err != nil {
			return info, err
		}
		info.Rate = rate
		byteRate := rate * ch * bits / 8
		if byteRate > 0 && dataSize > 0 && dataSize != 0xFFFFFFFF {
			info.Duration = time.Duration(dataSize) * time.Second / time.Duration(byteRate)
		}
		return info, nil
	}
}

// flacHeader walks a FLAC stream's metadata blocks (starting AT the "fLaC"
// magic): StreamInfo gives the length, the first Vorbis comment the tags,
// everything else is skipped.
func flacHeader(r io.Reader) (info TrackInfo, err error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return info, err
	}
	if string(magic[:]) != "fLaC" {
		return info, io.ErrUnexpectedEOF
	}
	for {
		block, err := meta.New(r)
		if err != nil {
			return info, err
		}
		switch block.Type {
		case meta.TypeStreamInfo:
			if err := block.Parse(); err != nil {
				return info, err
			}
			if si, ok := block.Body.(*meta.StreamInfo); ok {
				info.Rate = int(si.SampleRate)
				if n := si.NSamples; n > 0 && si.SampleRate > 0 {
					info.Duration = time.Duration(n) * time.Second / time.Duration(si.SampleRate)
				}
			}
		case meta.TypeVorbisComment:
			if err := block.Parse(); err != nil {
				return info, err
			}
			if vc, ok := block.Body.(*meta.VorbisComment); ok && info.Name == "" {
				for _, tag := range vc.Tags {
					switch strings.ToUpper(tag[0]) {
					case "TITLE":
						info.Title = strings.TrimSpace(tag[1])
					case "ARTIST":
						info.Artist = strings.TrimSpace(tag[1])
					case "ALBUM":
						info.Album = strings.TrimSpace(tag[1])
					case "ALBUMARTIST", "ALBUM ARTIST", "ALBUM_ARTIST":
						info.AlbumArtist = strings.TrimSpace(tag[1])
					case "DATE", "YEAR":
						if info.Year == "" {
							info.Year = strings.TrimSpace(tag[1])
						}
					case "TRACKNUMBER":
						info.Track = tagNum(tag[1])
					case "DISCNUMBER":
						info.Disc = tagNum(tag[1])
					}
				}
				info.Name = joinName(info.Title, info.Artist)
			}
		}
		last := block.IsLast
		if err := block.Skip(); err != nil {
			return info, err
		}
		if last {
			return info, nil
		}
	}
}

// DecodeFrom decodes a seekable WAV, FLAC or MP3 stream starting skip into the
// audio (resuming a paused USB track). skip <= 0 is a plain Decode. It always
// closes b exactly once.
//
// FLAC seeks by frame: up to one block before the position replays (well
// under a tenth of a second). WAV skips exact bytes.
func DecodeFrom(r io.ReadSeeker, skip time.Duration, b *Buffer) error {
	if skip <= 0 {
		return Decode(r, b)
	}
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		b.Close(err)
		return err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		b.Close(err)
		return err
	}
	if isMP3(magic[:]) {
		err := decodeMP3From(r, skip, b)
		b.Close(err)
		return err
	}
	switch string(magic[:]) {
	case "fLaC":
		// NewSeek (not New): the only constructor whose Stream.Seek works —
		// New wraps the reader in a plain bufio.Reader and Seek panics on it
		s, err := flac.NewSeek(r)
		if err != nil {
			b.Close(err)
			return err
		}
		defer s.Close()
		frames := uint64(skip.Seconds() * float64(s.Info.SampleRate))
		if frames > 0 {
			if _, err := s.Seek(frames); err != nil {
				b.Close(err)
				return err
			}
		}
		err = decodeFLACStream(s, b)
		b.Close(err)
		return err
	default:
		br, format, ch, rate, bits, _, err := wavData(r)
		if err != nil {
			b.Close(err)
			return err
		}
		if ch > 0 && rate > 0 && bits > 0 {
			skipBytes := int64(skip.Seconds() * float64(rate*ch*bits/8))
			if _, err := io.CopyN(io.Discard, br, skipBytes); err != nil {
				b.Close(err)
				return err
			}
		}
		return decodeWAVData(br, format, ch, rate, bits, b)
	}
}
