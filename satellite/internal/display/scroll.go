package display

import "strings"

// translit maps characters the 7-segment font lacks to close ASCII.
var translit = strings.NewReplacer(
	"ä", "a", "Ä", "A", "ö", "o", "Ö", "O", "å", "a", "Å", "A", "ü", "u", "Ü", "U",
	"é", "e", "è", "e", "ê", "e", "É", "E", "á", "a", "à", "a", "í", "i", "ó", "o", "ú", "u",
	"ñ", "n", "ç", "c", "ß", "ss", "æ", "ae", "Æ", "AE", "ø", "o", "Ø", "O",
	"\n", " ", "\t", " ", "–", "-", "—", "-", "’", "'", "‘", "'", "“", "\"", "”", "\"",
	".", " ", ",", " ", ":", " ", "!", " ", ";", " ",
)

// Strip renders text as a row of glyphs for scrolling.
func Strip(text string) []byte {
	text = translit.Replace(text)
	var out []byte
	for _, r := range text {
		out = append(out, Glyph(r))
	}
	// collapse runs of blanks so punctuation doesn't leave long gaps
	var c []byte
	for i, g := range out {
		if g == 0 && i > 0 && out[i-1] == 0 {
			continue
		}
		c = append(c, g)
	}
	return c
}

// Window is the 4-digit view of strip scrolled to step: the text enters from
// the right (step 0 = blank) and leaves to the left. Steps = len(strip)+4.
func Window(strip []byte, step int) [4]byte {
	var w [4]byte
	for i := 0; i < 4; i++ {
		j := step - 4 + i
		if j >= 0 && j < len(strip) {
			w[i] = strip[j]
		}
	}
	return w
}

// ScrollSteps is how many steps one pass of strip takes.
func ScrollSteps(strip []byte) int { return len(strip) + 4 }

// SetBig puts raw glyphs on the big digits.
func (f *Frame) SetBig(g [4]byte) {
	copy(f.A[:4], g[:])
}
