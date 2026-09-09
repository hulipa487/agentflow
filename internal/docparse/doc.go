package docparse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// DocToText extracts plain text from a legacy Word 97-2003 .doc file (an OLE2
// compound document). It reads the WordDocument stream and, when present, the
// piece table (CLX) in the 0Table/1Table stream, which is the authoritative
// map of where the text lives and in which encoding (UTF-16LE or CP1252).
// Without a usable piece table it falls back to the contiguous fcMin..fcMac
// range. Best-effort: fast-saved, encrypted, or otherwise exotic files may not
// extract cleanly — but those are rare from government/institutional APIs.
func DocToText(data []byte) (string, error) {
	streams, err := ole2Streams(data)
	if err != nil {
		return "", err
	}
	wd, ok := streams["WordDocument"]
	if !ok {
		return "", fmt.Errorf("docparse doc: no WordDocument stream (not a Word document?)")
	}
	if len(wd) < 0x1AA {
		return "", fmt.Errorf("docparse doc: WordDocument stream too small")
	}
	// fWhichTblStm (bit 9 of the FIB flags at 0x0A) selects 1Table vs 0Table.
	tableName := "0Table"
	if binary.LittleEndian.Uint16(wd[0x0A:])&0x0200 != 0 {
		tableName = "1Table"
	}
	table, ok := streams[tableName]
	if !ok { // tolerate a mis-set flag by taking whichever table stream exists
		if t, ok2 := streams["0Table"]; ok2 {
			table = t
		} else if t, ok2 := streams["1Table"]; ok2 {
			table = t
		}
	}
	if len(table) > 0 {
		fcClx := binary.LittleEndian.Uint32(wd[0x1A2:])
		lcbClx := binary.LittleEndian.Uint32(wd[0x1A6:])
		if lcbClx > 0 && uint64(fcClx)+uint64(lcbClx) <= uint64(len(table)) {
			if text, ok := docTextFromClx(wd, table[fcClx:fcClx+lcbClx]); ok {
				return text, nil
			}
		}
	}
	return docTextSimple(wd)
}

// docTextFromClx walks the piece table. Each piece maps a run of characters to
// a WordDocument offset and encoding. Returns ok=false when the CLX has no
// piece table so the caller can fall back.
func docTextFromClx(wd, clx []byte) (string, bool) {
	i := 0
	for i < len(clx) && clx[i] == 1 { // skip Prc entries (clxt=1)
		if i+3 > len(clx) {
			return "", false
		}
		cb := int(binary.LittleEndian.Uint16(clx[i+1:]))
		i += 3 + cb
	}
	if i+5 > len(clx) || clx[i] != 2 { // Pcdt must start with clxt=2
		return "", false
	}
	lcb := int(binary.LittleEndian.Uint32(clx[i+1:]))
	plc := clx[i+5:]
	if lcb > len(plc) {
		lcb = len(plc)
	}
	n := (lcb - 4) / 12 // 4*(n+1) CPs + 8*n PCDs = 12n+4
	if n <= 0 || (n+1)*4+n*8 > len(plc) {
		return "", false
	}
	cps := make([]uint32, n+1)
	for j := 0; j <= n; j++ {
		cps[j] = binary.LittleEndian.Uint32(plc[j*4:])
	}
	pcdOff := (n + 1) * 4
	var sb strings.Builder
	for j := 0; j < n; j++ {
		fc := binary.LittleEndian.Uint32(plc[pcdOff+j*8+2:])
		compressed := fc&0x40000000 != 0
		fc &= 0x3FFFFFFF
		chars := int(cps[j+1] - cps[j])
		if chars <= 0 {
			continue
		}
		if compressed { // CP1252, 1 byte/char; stored offset is fc/2
			off := int(fc) / 2
			if off >= len(wd) {
				continue
			}
			if off+chars > len(wd) {
				chars = len(wd) - off
			}
			sb.WriteString(cp1252ToUTF8(wd[off : off+chars]))
		} else { // UTF-16LE, 2 bytes/char
			off := int(fc)
			nbytes := chars * 2
			if off >= len(wd) {
				continue
			}
			if off+nbytes > len(wd) {
				nbytes = len(wd) - off
			}
			sb.WriteString(utf16leToUTF8(wd[off : off+nbytes]))
		}
	}
	return cleanDocText(sb.String()), true
}

// docTextSimple is the no-piece-table fallback: the contiguous fcMin..fcMac
// range, read as UTF-16LE. Heuristic, but fine for simple non-fast-saved docs.
func docTextSimple(wd []byte) (string, error) {
	fcMin := binary.LittleEndian.Uint32(wd[0x18:])
	fcMac := binary.LittleEndian.Uint32(wd[0x1C:])
	if fcMac <= fcMin || fcMin >= uint32(len(wd)) {
		return "", fmt.Errorf("docparse doc: cannot locate document text")
	}
	if fcMac > uint32(len(wd)) {
		fcMac = uint32(len(wd))
	}
	return cleanDocText(utf16leToUTF8(wd[fcMin:fcMac])), nil
}

// cleanDocText maps Word control characters onto text structure and drops the
// rest (field markers, object/annotation sentinels), then normalizes.
func cleanDocText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\r', '\v', '\f': // paragraph / line / page marks (0x0D, 0x0B, 0x0C)
			b.WriteByte('\n')
		case 0x07: // table cell / row mark
			b.WriteByte('\t')
		case 0x13, 0x14, 0x15, 0x01, 0x02, 0x05, 0x08, 0x1E, 0x1F: // field/object markers
			// drop
		default:
			if r < 0x20 && r != '\n' && r != '\t' {
				continue
			}
			b.WriteRune(r)
		}
	}
	return normalizeText(b.String())
}

func utf16leToUTF8(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// cp1252High maps CP1252 bytes 0x80..0x9F (which differ from Latin-1).
var cp1252High = [32]rune{
	'€', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 'Ž', '‘', '’',
	'“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 'ž', 'Ÿ',
}

func cp1252ToUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c >= 0xA0: // 0xA0..0xFF are identical to Latin-1 / Unicode
			sb.WriteRune(rune(c))
		default:
			sb.WriteRune(cp1252High[c-0x80])
		}
	}
	return sb.String()
}

// --- OLE2 compound-file reader (MS-CFB) ------------------------------------

const (
	oleEndOfChain = 0xFFFFFFFE
	oleFreeSect   = 0xFFFFFFFF
)

type ole2 struct {
	data       []byte
	sectorSize int
	miniCutoff uint32
	fat        []uint32
	miniFAT    []uint32
	miniStream []byte
}

// ole2Streams reads an OLE2 compound file and returns its streams by name.
func ole2Streams(data []byte) (map[string][]byte, error) {
	if len(data) < 512 || !bytes.Equal(data[:8], oleMagic) {
		return nil, fmt.Errorf("docparse doc: not an OLE2 compound file")
	}
	o := &ole2{data: data}
	o.sectorSize = 1 << binary.LittleEndian.Uint16(data[30:])
	if o.sectorSize < 512 {
		return nil, fmt.Errorf("docparse doc: bad sector size")
	}
	o.miniCutoff = binary.LittleEndian.Uint32(data[56:])
	numFAT := binary.LittleEndian.Uint32(data[44:])
	firstDir := binary.LittleEndian.Uint32(data[48:])
	firstMiniFAT := binary.LittleEndian.Uint32(data[60:])
	numMiniFAT := binary.LittleEndian.Uint32(data[64:])
	firstDIFAT := binary.LittleEndian.Uint32(data[68:])
	numDIFAT := binary.LittleEndian.Uint32(data[72:])

	// DIFAT: sector ids of the FAT sectors. First 109 live in the header.
	difat := []uint32{}
	for i := 0; i < 109; i++ {
		if sid := binary.LittleEndian.Uint32(data[76+i*4:]); sid != oleFreeSect {
			difat = append(difat, sid)
		}
	}
	for sid, i := firstDIFAT, uint32(0); i < numDIFAT && sid < oleEndOfChain; i++ {
		sec := o.sector(sid)
		n := o.sectorSize/4 - 1
		for j := 0; j < n; j++ {
			if fid := binary.LittleEndian.Uint32(sec[j*4:]); fid != oleFreeSect {
				difat = append(difat, fid)
			}
		}
		sid = binary.LittleEndian.Uint32(sec[n*4:])
	}
	// FAT entries, from the FAT sectors named by the DIFAT.
	for i := uint32(0); i < numFAT && i < uint32(len(difat)); i++ {
		sec := o.sector(difat[i])
		for j := 0; j < o.sectorSize/4; j++ {
			o.fat = append(o.fat, binary.LittleEndian.Uint32(sec[j*4:]))
		}
	}

	// Directory entries.
	dirData := o.readChain(firstDir)
	type entry struct {
		name  string
		otype byte
		start uint32
		size  uint64
	}
	entries := []entry{}
	var rootIdx = -1
	for off := 0; off+128 <= len(dirData); off += 128 {
		e := dirData[off : off+128]
		nameLen := int(binary.LittleEndian.Uint16(e[64:]))
		otype := e[66]
		if otype == 0 || nameLen < 2 || nameLen > 64 {
			continue
		}
		name := utf16leToUTF8(e[:nameLen-2])
		entries = append(entries, entry{
			name:  name,
			otype: otype,
			start: binary.LittleEndian.Uint32(e[116:]),
			size:  binary.LittleEndian.Uint64(e[120:]),
		})
		if otype == 5 {
			rootIdx = len(entries) - 1
		}
	}

	// The mini stream is the root entry's stream; the mini FAT indexes it.
	if rootIdx >= 0 && entries[rootIdx].start < oleEndOfChain {
		o.miniStream = o.readChain(entries[rootIdx].start)
		if uint64(len(o.miniStream)) > entries[rootIdx].size {
			o.miniStream = o.miniStream[:entries[rootIdx].size]
		}
	}
	if numMiniFAT > 0 && firstMiniFAT < oleEndOfChain {
		mf := o.readChainN(firstMiniFAT, numMiniFAT)
		for j := 0; j+4 <= len(mf); j += 4 {
			o.miniFAT = append(o.miniFAT, binary.LittleEndian.Uint32(mf[j:]))
		}
	}

	out := map[string][]byte{}
	for _, e := range entries {
		if e.otype != 2 { // streams only
			continue
		}
		var b []byte
		if e.size < uint64(o.miniCutoff) && len(o.miniStream) > 0 {
			b = o.readMiniChain(e.start)
		} else {
			b = o.readChain(e.start)
		}
		if uint64(len(b)) > e.size {
			b = b[:e.size]
		}
		out[e.name] = b
	}
	return out, nil
}

func (o *ole2) sector(sid uint32) []byte {
	off := int(sid+1) * o.sectorSize
	if off < 0 || off+o.sectorSize > len(o.data) {
		return make([]byte, o.sectorSize)
	}
	return o.data[off : off+o.sectorSize]
}

func (o *ole2) readChain(start uint32) []byte {
	var out []byte
	seen := map[uint32]bool{}
	for sid := start; sid < oleEndOfChain && !seen[sid]; {
		seen[sid] = true
		out = append(out, o.sector(sid)...)
		if int(sid) >= len(o.fat) {
			break
		}
		sid = o.fat[sid]
	}
	return out
}

func (o *ole2) readChainN(start uint32, n uint32) []byte {
	var out []byte
	for sid, i := start, uint32(0); i < n && sid < oleEndOfChain; i++ {
		out = append(out, o.sector(sid)...)
		if int(sid) >= len(o.fat) {
			break
		}
		sid = o.fat[sid]
	}
	return out
}

func (o *ole2) readMiniChain(start uint32) []byte {
	var out []byte
	seen := map[uint32]bool{}
	for sid := start; sid < oleEndOfChain && !seen[sid]; {
		seen[sid] = true
		if off := int(sid) * 64; off+64 <= len(o.miniStream) {
			out = append(out, o.miniStream[off:off+64]...)
		}
		if int(sid) >= len(o.miniFAT) {
			break
		}
		sid = o.miniFAT[sid]
	}
	return out
}
