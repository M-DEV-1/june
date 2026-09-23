package recorder

import "strings"

// Since whisper runs with -l auto, a Hindi call comes back in Devanagari and a Malayalam one in Malayalam script, and every transcript is meant to be readable in Latin letters. The nine Brahmic scripts from Devanagari (U+0900) to Malayalam (U+0D00) each take a 128-codepoint block and put the same sound at the same offset inside it, so one table keyed on that offset spells all of them.
// The spelling is the casual one people type Hinglish or Manglish in, plain ASCII with no diacritics: "है" is "hai", "करके" is "karke", "അത്" in Malayalam is "athu".
const (
	indicFirst = 0x0900
	indicLast  = 0x0DFF
)

// Script blocks that need a rule of their own.
const (
	devanagari = 0x0900
	bengali    = 0x0980
	gurmukhi   = 0x0A00
	gujarati   = 0x0A80
	tamil      = 0x0B80
	telugu     = 0x0C00
	kannada    = 0x0C80
	malayalam  = 0x0D00
)

// Offsets of the signs every block shares.
const (
	candrabindu = 0x01
	anusvara    = 0x02
	visarga     = 0x03
	nukta       = 0x3C
	virama      = 0x4D
	tippi       = 0x70 // Gurmukhi's nasal sign
)

// indicConsonants spells each consonant offset without its inherent vowel. Offsets 0x58 to 0x5F are the precomposed nukta letters.
var indicConsonants = map[rune]string{
	0x15: "k", 0x16: "kh", 0x17: "g", 0x18: "gh", 0x19: "n",
	0x1A: "ch", 0x1B: "chh", 0x1C: "j", 0x1D: "jh", 0x1E: "nj",
	0x1F: "t", 0x20: "th", 0x21: "d", 0x22: "dh", 0x23: "n",
	0x24: "t", 0x25: "th", 0x26: "d", 0x27: "dh", 0x28: "n", 0x29: "n",
	0x2A: "p", 0x2B: "ph", 0x2C: "b", 0x2D: "bh", 0x2E: "m",
	0x2F: "y", 0x30: "r", 0x31: "r", 0x32: "l", 0x33: "l", 0x34: "zh", 0x35: "v",
	0x36: "sh", 0x37: "sh", 0x38: "s", 0x39: "h",
	0x58: "q", 0x59: "kh", 0x5A: "gh", 0x5B: "z", 0x5C: "d", 0x5D: "dh", 0x5E: "f", 0x5F: "y",
}

// withNukta is what a dot below turns a consonant into, for the few where Hindi speech actually changes: क़ q, ज़ z, फ़ f.
var withNukta = map[string]string{"k": "q", "j": "z", "ph": "f"}

// indicVowels spells the independent vowel letters (0x05 to 0x14, 0x60, 0x61) and the vowel signs that follow a consonant (0x3E to 0x4C, 0x57, 0x62, 0x63). Long vowels are doubled here and shortened again at the end of a word.
var indicVowels = map[rune]string{
	0x05: "a", 0x06: "aa", 0x07: "i", 0x08: "ee", 0x09: "u", 0x0A: "oo", 0x0B: "ri", 0x0C: "li",
	0x0D: "e", 0x0E: "e", 0x0F: "e", 0x10: "ai", 0x11: "o", 0x12: "o", 0x13: "o", 0x14: "au",
	0x60: "ri", 0x61: "li",
	0x3E: "aa", 0x3F: "i", 0x40: "ee", 0x41: "u", 0x42: "oo", 0x43: "ri", 0x44: "ri",
	0x45: "e", 0x46: "e", 0x47: "e", 0x48: "ai", 0x49: "o", 0x4A: "o", 0x4B: "o", 0x4C: "au",
	0x57: "au", 0x62: "li", 0x63: "li",
}

// vowelSign reports whether an offset is a vowel sign that follows a consonant rather than a vowel letter standing alone.
func vowelSign(off rune) bool {
	return (off >= 0x3E && off <= 0x4C) || off == 0x57 || off == 0x62 || off == 0x63
}

// malayalamChillus are Malayalam's consonants that never carry a vowel, which sit outside the shared layout.
var malayalamChillus = map[rune]string{0x54: "m", 0x55: "y", 0x56: "zh", 0x7A: "n", 0x7B: "n", 0x7C: "r", 0x7D: "l", 0x7E: "l", 0x7F: "k"}

// syllable is one consonant or vowel letter with what follows it: the vowel it carries and any nasal or visarga after that.
type syllable struct {
	cons  string // "" for a vowel letter standing alone
	vowel string // "" when a virama silences the consonant
	schwa bool   // vowel is the inherent "a" nobody wrote, which Hindi drops in some places
	mark  rune   // anusvara, candrabindu, tippi or visarga after the vowel, 0 for none
	halt  bool   // a virama ended this syllable, rather than a chillu having no vowel to begin with
}

// Romanize rewrites every run of Indic-script text in s in Latin letters and leaves all other text, Latin, Arabic or anything else, as it is. Input: one transcript segment's text. Output: the same text with Indic words spelt out, digits as ASCII digits and a danda as a full stop.
func Romanize(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		r := rs[i]
		if r < indicFirst || r > indicLast {
			b.WriteRune(r)
			i++
			continue
		}
		off := r & 0x7F
		switch {
		case off == 0x64 || off == 0x65:
			b.WriteByte('.')
			i++
			continue
		case off >= 0x66 && off <= 0x6F:
			b.WriteByte(byte('0' + off - 0x66))
			i++
			continue
		}
		j := i
		for j < len(rs) && inWord(rs[j]) {
			j++
		}
		b.WriteString(romanizeWord(rs[i:j]))
		i = j
	}
	return b.String()
}

// inWord reports whether r continues an Indic word: a letter or sign of one of the blocks, or the zero-width joiners that shape them.
func inWord(r rune) bool {
	if r == 0x200C || r == 0x200D {
		return true
	}
	if r < indicFirst || r > indicLast {
		return false
	}
	off := r & 0x7F
	return off < 0x64 || off > 0x6F
}

// romanizeWord spells one Indic word. Input: the word's runes, all from the Indic blocks or zero-width joiners. Output: its Latin spelling.
func romanizeWord(word []rune) string {
	block := word[0] &^ 0x7F
	var syl []syllable
	last := func() *syllable {
		if len(syl) == 0 {
			syl = append(syl, syllable{})
		}
		return &syl[len(syl)-1]
	}
	for _, r := range word {
		if r == 0x200C || r == 0x200D {
			// A joiner after a virama is the old way of writing a Malayalam chillu, so the virama is not the word's last sign.
			if len(syl) > 0 {
				last().halt = false
			}
			continue
		}
		off, blk := r&0x7F, r&^0x7F
		if c, ok := malayalamChillus[off]; ok && blk == malayalam {
			syl = append(syl, syllable{cons: c})
			continue
		}
		if c, ok := indicConsonants[off]; ok {
			if dravidian(blk) && (off == 0x24 || off == 0x25) {
				// The dental t is "th" in the way Tamil, Telugu, Kannada and Malayalam are typed ("athu"), and "t" in Hindi ("tum").
				c = "th"
			}
			syl = append(syl, syllable{cons: c, vowel: "a", schwa: true})
			continue
		}
		switch {
		case off == nukta:
			if s := last(); withNukta[s.cons] != "" {
				s.cons = withNukta[s.cons]
			}
		case off == virama:
			s := last()
			s.vowel, s.schwa, s.halt = "", false, true
		case vowelSign(off):
			s := last()
			s.vowel, s.schwa = indicVowels[off], false
		case indicVowels[off] != "":
			syl = append(syl, syllable{vowel: indicVowels[off]})
		case off == anusvara || off == candrabindu || off == visarga || (off == tippi && blk == gurmukhi):
			last().mark = off
		}
	}
	if block == devanagari || block == bengali || block == gurmukhi || block == gujarati {
		dropSchwa(syl)
	}
	var b strings.Builder
	for i, s := range syl {
		end := i == len(syl)-1
		vowel := s.vowel
		if end {
			// A long vowel is written single at the end of a word: "raha", "bhi", "hun", not "rahaa".
			vowel = strings.NewReplacer("aa", "a", "ee", "i", "oo", "u").Replace(vowel)
			if vowel == "e" && s.mark == anusvara && block == devanagari {
				vowel = "ei" // में is typed "mein", हमें "hamein".
			}
			if s.halt && block == malayalam {
				vowel = "u" // A Malayalam word ending in a virama is said with a short u: അത് is "athu".
			}
		}
		b.WriteString(s.cons + vowel)
		switch s.mark {
		case visarga:
			b.WriteString("h")
		case 0:
		default:
			b.WriteString(nasal(syl, i, block))
		}
	}
	return b.String()
}

// nasal spells the nasal sign on syllable i: "m" before p, b or m, "m" at the end of a Dravidian word (ചെയ്യാം is "cheyyam"), and "n" everywhere else (मैं is "main").
func nasal(syl []syllable, i int, block rune) string {
	if i+1 < len(syl) {
		if strings.HasPrefix(syl[i+1].cons, "p") || strings.HasPrefix(syl[i+1].cons, "b") || syl[i+1].cons == "m" {
			return "m"
		}
		return "n"
	}
	if dravidian(block) {
		return "m"
	}
	return "n"
}

// dravidian reports whether a script block is Tamil, Telugu, Kannada or Malayalam.
func dravidian(block rune) bool {
	return block == tamil || block == telugu || block == kannada || block == malayalam
}

// dropSchwa removes the inherent "a" Hindi, Bengali, Punjabi and Gujarati do not say. The last one goes unless the word is a single letter or ends in a cluster onto r, y or v ("mitra", "satya"), so "kar", "samay" and "test" come out right. Inside the word it goes between a vowel-and-consonant and a consonant-with-vowel, worked from the right so each decision sees the ones after it: "karke", "sabse", "badalna".
func dropSchwa(syl []syllable) {
	n := len(syl)
	if n > 1 && syl[n-1].schwa {
		afterCluster := syl[n-2].cons != "" && syl[n-2].vowel == ""
		if !afterCluster || (syl[n-1].cons != "r" && syl[n-1].cons != "y" && syl[n-1].cons != "v") {
			syl[n-1].vowel, syl[n-1].schwa = "", false
		}
	}
	for i := n - 2; i >= 1; i-- {
		s := &syl[i]
		if s.schwa && s.mark == 0 && syl[i-1].vowel != "" && syl[i+1].cons != "" && syl[i+1].vowel != "" {
			s.vowel, s.schwa = "", false
		}
	}
}
