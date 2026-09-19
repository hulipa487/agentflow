// Package docparse extracts plain text from the document formats APIs commonly
// return, so each new source doesn't need its own parser. DOCX (OOXML) is fully
// supported; legacy DOC (Word 97-2003, OLE2) is best-effort via the piece
// table; RTF and HTML are tag/control-stripped; TXT passes through. PDF and
// unrecognized data are detected but return an honest unsupported error rather
// than garbage. Stdlib only.
package docparse

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

// Format is a detected document type.
type Format int

const (
	Unknown Format = iota
	DOCX           // OOXML zip (word/document.xml)
	DOC            // legacy OLE2 compound (Word 97-2003)
	PDF            // %PDF (detected; extraction unsupported)
	RTF            // {\rtf
	HTML           // markup
	TXT            // plain text fallback
)

func (f Format) String() string {
	switch f {
	case DOCX:
		return "docx"
	case DOC:
		return "doc"
	case PDF:
		return "pdf"
	case RTF:
		return "rtf"
	case HTML:
		return "html"
	case TXT:
		return "txt"
	}
	return "unknown"
}

var (
	zipMagic = []byte("PK\x03\x04")
	oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	pdfMagic = []byte("%PDF-")
	rtfMagic = []byte("{\\rtf")
)

// sniff detects the document format from magic bytes (and a light content
// probe for HTML/TXT). Magic bytes are authoritative.
func sniff(data []byte) Format {
	if bytes.HasPrefix(data, zipMagic) {
		return DOCX
	}
	if bytes.HasPrefix(data, oleMagic) {
		return DOC
	}
	if bytes.HasPrefix(data, pdfMagic) {
		return PDF
	}
	if bytes.HasPrefix(data, rtfMagic) {
		return RTF
	}
	head := bytes.TrimSpace(data)
	if len(head) > 0 && head[0] == '<' {
		return HTML
	}
	if utf8.Valid(data) {
		return TXT
	}
	return Unknown
}

// Text detects the format and extracts plain text. PDF and Unknown return an
// honest error; the caller can then fall back to offering the raw download.
func Text(data []byte) (string, Format, error) {
	f := sniff(data)
	switch f {
	case DOCX:
		s, err := DocxToText(data)
		return s, f, err
	case DOC:
		s, err := DocToText(data)
		return s, f, err
	case RTF:
		s, err := RtfToText(data)
		return s, f, err
	case HTML:
		s, err := HTMLToText(data)
		return s, f, err
	case TXT:
		return string(data), f, nil
	case PDF:
		return "", f, fmt.Errorf("docparse: pdf text extraction is not supported")
	}
	return "", f, fmt.Errorf("docparse: unrecognized document format")
}
