package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// DocxToText extracts plain text from an OOXML .docx file. A docx is a zip;
// the body is word/document.xml, with text in <w:t> runs and structure in
// <w:p> (paragraph), <w:br>/<w:cr> (line break), and <w:tab>. Text inside
// deleted-content (<w:delText>) and field instructions is skipped.
func DocxToText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docparse docx: not a zip: %w", err)
	}
	var docXML io.ReadCloser
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML, err = f.Open()
			if err != nil {
				return "", fmt.Errorf("docparse docx: open document.xml: %w", err)
			}
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("docparse docx: word/document.xml not found (not a Word document?)")
	}
	defer docXML.Close()
	return docxText(docXML)
}

// docxText streams document.xml, emitting paragraph breaks for <w:p> and line
// breaks for <w:br>, collecting <w:t> character data.
func docxText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var sb strings.Builder
	atParaStart := true // suppress a leading blank line
	// skipDepth tracks nesting inside content we drop (deleted text, instrText).
	skipDepth := 0

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docparse docx: parse xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "delText", "instrText":
				skipDepth++
			case "p":
				if skipDepth == 0 && !atParaStart {
					sb.WriteByte('\n')
				}
			case "br", "cr":
				if skipDepth == 0 {
					sb.WriteByte('\n')
					atParaStart = false
				}
			case "tab":
				if skipDepth == 0 {
					sb.WriteByte('\t')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "delText", "instrText":
				if skipDepth > 0 {
					skipDepth--
				}
			case "p":
				if skipDepth == 0 {
					atParaStart = false
				}
			}
		case xml.CharData:
			if skipDepth == 0 {
				sb.Write(t)
			}
		}
	}
	return normalizeText(sb.String()), nil
}

// normalizeText tidies extracted text: collapse blank-line runs and trailing
// per-line whitespace, keeping paragraph breaks.
func normalizeText(s string) string {
	s = strings.TrimPrefix(s, "\ufeff") // strip a leading BOM (common in .docx/.doc)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := false
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
