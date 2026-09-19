package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSniff(t *testing.T) {
	cases := []struct {
		data []byte
		want Format
	}{
		{[]byte("PK\x03\x04\x14\x00\x00\x00"), DOCX},
		{[]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1, 1, 2}, DOC},
		{[]byte("%PDF-1.7 stuff"), PDF},
		{[]byte("{\\rtf1\\ansi}"), RTF},
		{[]byte("<html><body>x</body></html>"), HTML},
		{[]byte("just plain text"), TXT},
	}
	for _, c := range cases {
		if got := sniff(c.data); got != c.want {
			t.Errorf("sniff = %v, want %v", got, c.want)
		}
	}
}

func TestTextDispatch(t *testing.T) {
	if s, f, err := Text([]byte("hello")); err != nil || f != TXT || s != "hello" {
		t.Fatalf("txt: %v %v %q", f, err, s)
	}
	if _, f, err := Text([]byte("%PDF-1.4")); err == nil || f != PDF {
		t.Fatalf("pdf should be unsupported: %v %v", f, err)
	}
}

// --- DOCX ------------------------------------------------------------------

func buildZipDocx(t *testing.T, docXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(docXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDocxToText(t *testing.T) {
	x := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		`<w:p><w:r><w:t>第一条</w:t></w:r><w:r><w:t> 总则</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Second para</w:t><w:br/><w:t>with break</w:t></w:r></w:p>` +
		`<w:p><w:r><w:delText>deleted</w:delText><w:t>kept</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	got, err := DocxToText(buildZipDocx(t, x))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "第一条 总则") {
		t.Fatalf("para 1: %q", got)
	}
	if !strings.Contains(got, "Second para\nwith break") {
		t.Fatalf("line break: %q", got)
	}
	if !strings.Contains(got, "kept") || strings.Contains(got, "deleted") {
		t.Fatalf("delText should be dropped: %q", got)
	}
}

func TestDocxNotZip(t *testing.T) {
	if _, err := DocxToText([]byte("not a zip")); err == nil {
		t.Fatal("expected error for non-zip")
	}
}

// --- DOC (OLE2 + piece table) ----------------------------------------------

func utf16leBytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

// buildOLE2 lays out a minimal OLE2 compound file (512-byte sectors, regular
// FAT only — the mini stream is left empty) holding WordDocument and 1Table.
func buildOLE2(t *testing.T, wordDoc, table []byte) []byte {
	t.Helper()
	const ss = 512
	wdSecs := (len(wordDoc) + ss - 1) / ss
	tbSecs := (len(table) + ss - 1) / ss
	wdStart := 0
	tbStart := wdStart + wdSecs
	dirSec := tbStart + tbSecs
	fatSec := dirSec + 1
	totalSecs := fatSec + 1

	file := make([]byte, ss*(1+totalSecs))
	copy(file[0:8], []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
	binary.LittleEndian.PutUint16(file[24:], 0x003E)         // minor version
	binary.LittleEndian.PutUint16(file[26:], 0x0003)         // major version (512B sectors)
	binary.LittleEndian.PutUint16(file[28:], 0xFFFE)         // byte order
	binary.LittleEndian.PutUint16(file[30:], 9)              // sector shift
	binary.LittleEndian.PutUint16(file[32:], 6)              // mini sector shift
	binary.LittleEndian.PutUint32(file[44:], 1)              // num FAT sectors
	binary.LittleEndian.PutUint32(file[48:], uint32(dirSec)) // first dir sector
	binary.LittleEndian.PutUint32(file[56:], 4096)           // mini cutoff
	binary.LittleEndian.PutUint32(file[60:], 0xFFFFFFFE)     // first mini FAT
	binary.LittleEndian.PutUint32(file[64:], 0)              // num mini FAT
	binary.LittleEndian.PutUint32(file[68:], 0xFFFFFFFE)     // first DIFAT
	binary.LittleEndian.PutUint32(file[72:], 0)              // num DIFAT
	binary.LittleEndian.PutUint32(file[76:], uint32(fatSec)) // DIFAT[0]
	for i := 1; i < 109; i++ {
		binary.LittleEndian.PutUint32(file[76+i*4:], 0xFFFFFFFF)
	}

	sec := func(n int) []byte { return file[ss*(1+n) : ss*(2+n)] }
	span := func(start, length int) []byte { return file[ss*(1+start) : ss*(1+start)+length] }
	copy(span(wdStart, len(wordDoc)), wordDoc) // streams span multiple sectors
	copy(span(tbStart, len(table)), table)

	fat := make([]uint32, ss/4)
	for i := range fat {
		fat[i] = 0xFFFFFFFF
	}
	chain := func(start, n int) {
		for i := 0; i < n; i++ {
			if i == n-1 {
				fat[start+i] = 0xFFFFFFFE
			} else {
				fat[start+i] = uint32(start + i + 1)
			}
		}
	}
	chain(wdStart, wdSecs)
	chain(tbStart, tbSecs)
	fat[dirSec] = 0xFFFFFFFE
	fat[fatSec] = 0xFFFFFFFD
	for i, v := range fat {
		binary.LittleEndian.PutUint32(sec(fatSec)[i*4:], v)
	}

	dir := sec(dirSec)
	putEntry := func(idx int, name string, otype byte, start uint32, size uint64) {
		e := dir[idx*128:]
		nb := utf16leBytes(name)
		copy(e[0:64], nb)
		binary.LittleEndian.PutUint16(e[64:], uint16(len(nb)+2)) // + null terminator
		e[66] = otype
		binary.LittleEndian.PutUint32(e[116:], start)
		binary.LittleEndian.PutUint64(e[120:], size)
	}
	putEntry(0, "Root Entry", 5, 0xFFFFFFFE, 0)
	putEntry(1, "WordDocument", 2, uint32(wdStart), uint64(len(wordDoc)))
	putEntry(2, "1Table", 2, uint32(tbStart), uint64(len(table)))
	return file
}

func TestDocToText(t *testing.T) {
	text := "第一条 中华人民共和国安全生产法"
	u16 := utf16leBytes(text)
	// WordDocument: 512-byte FIB, text (UTF-16LE) at 0x200.
	wd := make([]byte, 0x200+len(u16))
	binary.LittleEndian.PutUint16(wd[0x0A:], 0x0200)                 // fWhichTblStm -> 1Table
	binary.LittleEndian.PutUint32(wd[0x18:], 0x200)                  // fcMin
	binary.LittleEndian.PutUint32(wd[0x1C:], uint32(0x200+len(u16))) // fcMac
	copy(wd[0x200:], u16)
	// CLX in 1Table at offset 0: one uncompressed piece spanning the text.
	charCount := uint32(len([]rune(text)))
	clx := []byte{0x02, 16, 0, 0, 0, 0, 0, 0, 0, byte(charCount), 0, 0, 0, 0, 0, 0x00, 0x02, 0x00, 0x00, 0, 0}
	binary.LittleEndian.PutUint32(wd[0x1A2:], 0)                // fcClx
	binary.LittleEndian.PutUint32(wd[0x1A6:], uint32(len(clx))) // lcbClx

	got, err := DocToText(buildOLE2(t, wd, clx))
	if err != nil {
		t.Fatal(err)
	}
	if got != text {
		t.Fatalf("got %q, want %q", got, text)
	}
}

func TestDocNotOLE2(t *testing.T) {
	if _, err := DocToText([]byte("not ole2")); err == nil {
		t.Fatal("expected error for non-OLE2")
	}
}

// --- RTF / HTML --------------------------------------------------------------

func TestRtfToText(t *testing.T) {
	rtf := `{\rtf1\ansi{\fonttbl{\f0 Arial;}}\f0\pard First line\par Second with \'e9 accent\par}`
	got, err := RtfToText([]byte(rtf))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "First line") || !strings.Contains(got, "Second with é accent") {
		t.Fatalf("text: %q", got)
	}
	if strings.Contains(got, "fonttbl") || strings.Contains(got, "Arial") {
		t.Fatalf("header group should be dropped: %q", got)
	}
}

func TestHTMLToText(t *testing.T) {
	h := `<html><head><style>body{color:red}</style></head><body><p>第一条</p><p>Second &amp; final</p><script>var x=1;</script></body></html>`
	got, err := HTMLToText([]byte(h))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "第一条") || !strings.Contains(got, "Second & final") {
		t.Fatalf("text: %q", got)
	}
	if strings.Contains(got, "color") || strings.Contains(got, "var x") {
		t.Fatalf("style/script should be dropped: %q", got)
	}
}
