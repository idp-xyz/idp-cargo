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

func ascii(r rune) bool { return r >= 32 && r <= 126 }

// Width is the width of a text at a size, in points.
func Width(s string, size float64) float64 {
	w := 0.0
	for _, r := range s {
		switch {
		case ascii(r):
			w += float64(helvetica[r-32])
		case r < 0x2E80:
			w += 500 // Latin and symbols in the CJK font are half width
		default:
			w += 1000
		}
	}
	return w * size / 1000
}

// Page is a page's content stream.
type Page struct{ ops bytes.Buffer }

func num(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// Text writes a line of text with its baseline at x, y; Latin runs in Helvetica, the rest in the CJK font.
func (p *Page) Text(x, y, size float64, s string) {
	for _, run := range runs(s) {
		if run == "" {
			continue
		}
		if ascii([]rune(run)[0]) {
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

// runs splits a text into Latin and other runs.
func runs(s string) []string {
	var out []string
	var b strings.Builder
	latin := true
	for i, r := range s {
		if r > 0xFFFF {
			r = '?'
		}
		a := ascii(r)
		if i > 0 && a != latin {
			out = append(out, b.String())
			b.Reset()
		}
		latin = a
		b.WriteRune(r)
	}
	return append(out, b.String())
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(s)
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
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, word := range splitWords(para) {
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
