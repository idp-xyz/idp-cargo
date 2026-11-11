// Package pdf renders DOC's document versions to PDF without a third-party library: a small PDF 1.4 writer with
// Helvetica for Latin text and the non-embedded STSong-Light CJK font (Adobe-GB1, UniGB-UCS2-H) for Chinese, which
// PDF readers supply or substitute. The output is deterministic: the same input gives the same bytes.
package pdf

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A4 portrait in points.
const (
	PageWidth  = 595.0
	PageHeight = 842.0
)

// helvetica holds the standard Helvetica widths of the printable ASCII characters (32-126), per 1000 units.
var helvetica = [...]int{278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278, 556, 556, 556, 556, 556, 556, 556, 556,
	556, 556, 278, 278, 584, 584, 584, 556, 1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778, 667, 778, 722, 667, 611,
	722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556, 333, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556, 556, 556,
	333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 278, 334, 584}

// latin1 holds Helvetica's widths of U+00A0-U+00FF, WinAnsi codes 0xA0-0xFF.
var latin1 = [...]int{278, 333, 556, 556, 556, 556, 260, 556, 333, 737, 370, 556, 584, 333, 737, 333, 400, 584, 333, 333, 333, 556, 537, 278, 333, 333,
	365, 556, 834, 834, 834, 611, 667, 667, 667, 667, 667, 667, 1000, 722, 667, 667, 667, 667, 278, 278, 278, 278, 722, 722, 778, 778, 778, 778, 778, 584,
	778, 722, 722, 722, 722, 667, 667, 611, 556, 556, 556, 556, 556, 556, 889, 500, 556, 556, 556, 556, 278, 278, 278, 278, 556, 556, 556, 556, 556, 556,
	556, 584, 611, 556, 556, 556, 556, 500, 556, 500}

// cp1252 holds the WinAnsi codes 0x80-0x9F with Helvetica's widths: the euro sign, typographic quotes and dashes.
var cp1252 = map[rune]struct{ code, width int }{'€': {0x80, 556}, '‚': {0x82, 222}, 'ƒ': {0x83, 556}, '„': {0x84, 333}, '…': {0x85, 1000},
	'†': {0x86, 556}, '‡': {0x87, 556}, 'ˆ': {0x88, 333}, '‰': {0x89, 1000}, 'Š': {0x8A, 667}, '‹': {0x8B, 333}, 'Œ': {0x8C, 1000}, 'Ž': {0x8E, 611},
	'‘': {0x91, 222}, '’': {0x92, 222}, '“': {0x93, 333}, '”': {0x94, 333}, '•': {0x95, 350}, '–': {0x96, 556}, '—': {0x97, 1000}, '˜': {0x98, 333},
	'™': {0x99, 1000}, 'š': {0x9A, 500}, '›': {0x9B, 333}, 'œ': {0x9C, 944}, 'ž': {0x9E, 500}, 'Ÿ': {0x9F, 667}}

// winAnsi returns a rune's WinAnsi code and Helvetica width, when Helvetica (F1) can show it.
func winAnsi(r rune) (byte, int, bool) {
	switch {
	case r >= 32 && r <= 126:
		return byte(r), helvetica[r-32], true
	case r >= 0xA0 && r <= 0xFF:
		return byte(r), latin1[r-0xA0], true
	}
	if c, ok := cp1252[r]; ok {
		return byte(c.code), c.width, true
	}
	return 0, 0, false
}

func latin(r rune) bool { _, _, ok := winAnsi(r); return ok }

// clean makes a text printable: tabs become spaces, other control characters go, and characters beyond the Basic
// Multilingual Plane, which the CJK font's UCS-2 encoding cannot address, become '?'.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteRune(' ')
		case r == '\n':
			b.WriteRune(r)
		case r < 32 || (r >= 0x7F && r < 0xA0) || r == utf8.RuneError:
		case r > 0xFFFF:
			b.WriteRune('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Width is the width of a text at a size, in points: Helvetica's widths for what it shows, a full em for the rest,
// which the CJK font shows at its default width.
func Width(s string, size float64) float64 {
	w := 0
	for _, r := range clean(s) {
		if _, width, ok := winAnsi(r); ok {
			w += width
		} else {
			w += 1000
		}
	}
	return float64(w) * size / 1000
}

// Page is a page's content stream.
type Page struct{ ops bytes.Buffer }

func num(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// Text writes a line of text with its baseline at x, y; what Helvetica can show in Helvetica (WinAnsi), the rest in
// the CJK font.
func (p *Page) Text(x, y, size float64, s string) {
	for _, run := range runs(clean(s)) {
		if run == "" {
			continue
		}
		if latin([]rune(run)[0]) {
			fmt.Fprintf(&p.ops, "BT /F1 %s Tf 1 0 0 1 %s %s Tm (%s) Tj ET\n", num(size), num(x), num(y), escape(run))
		} else {
			fmt.Fprintf(&p.ops, "BT /F2 %s Tf 1 0 0 1 %s %s Tm <%s> Tj ET\n", num(size), num(x), num(y), ucs2(run))
		}
		x += Width(run, size)
	}
}

// Wrap writes text in lines no wider than width, returning the y below the last line.
func (p *Page) Wrap(x, y, width, size float64, s string) float64 {
	for _, line := range wrap(s, width, size) {
		p.Text(x, y, size, line)
		y -= size * 1.25
	}
	return y
}

// Rect strokes a rectangle with its lower left corner at x, y.
func (p *Page) Rect(x, y, w, h float64) {
	fmt.Fprintf(&p.ops, "0.6 w %s %s %s %s re S\n", num(x), num(y), num(w), num(h))
}

// Line strokes a line.
func (p *Page) Line(x1, y1, x2, y2 float64) {
	fmt.Fprintf(&p.ops, "0.6 w %s %s m %s %s l S\n", num(x1), num(y1), num(x2), num(y2))
}

// Watermark puts a Latin text diagonally across the page in light gray, beneath everything drawn on it.
func (p *Page) Watermark(size float64, s string) {
	const cos, sin = 0.8, 0.6 // about 37 degrees
	w := Width(s, size)
	x, y := (PageWidth-w*cos)/2, (PageHeight-w*sin)/2
	mark := fmt.Sprintf("q 0.85 g BT /F1 %s Tf %s %s %s %s %s %s Tm (%s) Tj ET Q\n", num(size), num(cos), num(sin), num(-sin), num(cos), num(x), num(y),
		escape(s))
	ops := append([]byte(mark), p.ops.Bytes()...)
	p.ops.Reset()
	p.ops.Write(ops)
}

// runs splits a cleaned text into runs Helvetica shows and runs for the CJK font.
func runs(s string) []string {
	var out []string
	var b strings.Builder
	inLatin := true
	for i, r := range s {
		a := latin(r)
		if i > 0 && a != inLatin {
			out = append(out, b.String())
			b.Reset()
		}
		inLatin = a
		b.WriteRune(r)
	}
	return append(out, b.String())
}

// escape writes a Helvetica run as a literal string in WinAnsi, with codes above 127 as octal escapes so that
// content streams stay ASCII.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		code, _, _ := winAnsi(r)
		switch {
		case code == '\\' || code == '(' || code == ')':
			b.WriteByte('\\')
			b.WriteByte(code)
		case code >= 0x80:
			fmt.Fprintf(&b, "\\%03o", code)
		default:
			b.WriteByte(code)
		}
	}
	return b.String()
}

func ucs2(s string) string {
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, "%04X", u)
	}
	return b.String()
}

// wrap breaks a text into lines at spaces, or anywhere in CJK text, so that no line is wider than width.
func wrap(s string, width, size float64) []string {
	var lines []string
	for _, para := range strings.Split(clean(s), "\n") {
		line := ""
		var words []string
		for _, word := range splitWords(para) {
			words = append(words, breakWord(word, width, size)...)
		}
		for _, word := range words {
			if line != "" && Width(line+word, size) > width {
				lines = append(lines, strings.TrimRight(line, " "))
				line = strings.TrimLeft(word, " ")
				continue
			}
			line += word
		}
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return lines
}

// breakWord splits a word wider than width, such as an e-mail address, into pieces that fit.
func breakWord(word string, width, size float64) []string {
	if Width(word, size) <= width {
		return []string{word}
	}
	var out []string
	piece := ""
	for _, r := range word {
		if piece != "" && Width(piece+string(r), size) > width {
			out = append(out, piece)
			piece = ""
		}
		piece += string(r)
	}
	return append(out, piece)
}

// splitWords keeps each word with its trailing space; each CJK character is a word of its own.
func splitWords(s string) []string {
	var out []string
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0x2E80:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
			out = append(out, string(r))
		case r == ' ':
			b.WriteRune(r)
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// Document assembles pages into a PDF file. Title goes into the document information; nothing time-dependent
// does, so the bytes depend on the content only.
func Document(title string, pages []*Page) []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	first := 8 // objects 1-7 are fixed; pages start at 8, two objects each
	var kids []string
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", first+2*i))
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type0 /BaseFont /STSong-Light-UniGB-UCS2-H /Encoding /UniGB-UCS2-H /DescendantFonts [5 0 R] >>")
	obj("<< /Type /Font /Subtype /CIDFontType0 /BaseFont /STSong-Light /CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 4 >> " +
		"/FontDescriptor 6 0 R /DW 1000 >>")
	obj("<< /Type /FontDescriptor /FontName /STSong-Light /Flags 6 /FontBBox [-25 -254 1000 880] /ItalicAngle 0 /Ascent 880 /Descent -120 " +
		"/CapHeight 880 /StemV 93 >>")
	obj(fmt.Sprintf("<< /Producer (IDP-Cargo DOC) /Title <%s> >>", "FEFF"+ucs2(title)))
	for i, p := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			num(PageWidth), num(PageHeight), first+2*i+1))
		content := p.ops.String()
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 7 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}
