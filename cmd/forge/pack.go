package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// ---------------------------------------------------------------- pkt-line framing

func writePkt(buf *bytes.Buffer, payload string) {
	writePktBytes(buf, []byte(payload))
}

func writePktBytes(buf *bytes.Buffer, payload []byte) {
	fmt.Fprintf(buf, "%04x", len(payload)+4)
	buf.Write(payload)
}

func writeFlush(buf *bytes.Buffer) {
	buf.WriteString("0000")
}

func writeDelim(buf *bytes.Buffer) {
	buf.WriteString("0001")
}

const (
	pktData = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

type pktReader struct {
	data []byte
	pos  int
}

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}

func (p *pktReader) next() (int, []byte, error) {
	if p.pos+4 > len(p.data) {
		return 0, nil, io.ErrUnexpectedEOF
	}
	n := 0
	for i := 0; i < 4; i++ {
		v := hexVal(p.data[p.pos+i])
		if v < 0 {
			return 0, nil, errors.New("bad pkt-line length")
		}
		n = n<<4 | v
	}
	start := p.pos + 4
	switch n {
	case 0:
		p.pos = start
		return pktFlush, nil, nil
	case 1:
		p.pos = start
		return pktDelim, nil, nil
	case 2:
		p.pos = start
		return pktResponseEnd, nil, nil
	case 3:
		return 0, nil, errors.New("invalid pkt-line length 3")
	}
	if n < 4 {
		return 0, nil, errors.New("invalid pkt-line length")
	}
	end := start + n - 4
	if end > len(p.data) {
		return 0, nil, io.ErrUnexpectedEOF
	}
	payload := p.data[start:end]
	p.pos = end
	return pktData, payload, nil
}

// parsePktSegments splits a pkt-line stream into text segments separated by flush/delim.
func parsePktSegments(data []byte) ([][]string, int, error) {
	reader := &pktReader{data: data}
	var segments [][]string
	current := []string{}
	for {
		kind, payload, err := reader.next()
		if err != nil {
			return nil, reader.pos, err
		}
		switch kind {
		case pktData:
			current = append(current, string(payload))
		case pktDelim:
			segments = append(segments, current)
			current = []string{}
		case pktFlush, pktResponseEnd:
			segments = append(segments, current)
			return segments, reader.pos, nil
		}
	}
}

// splitPktCommands reads the command section of a receive-pack body and returns the bytes after it.
func splitPktCommands(data []byte) ([]string, []byte, error) {
	reader := &pktReader{data: data}
	var lines []string
	for {
		kind, payload, err := reader.next()
		if err != nil {
			return nil, nil, err
		}
		switch kind {
		case pktData:
			lines = append(lines, string(payload))
		case pktDelim:
			continue
		case pktFlush, pktResponseEnd:
			return lines, data[reader.pos:], nil
		}
	}
}

// ---------------------------------------------------------------- pack writing

type fetchPack struct {
	pack      []byte
	shallow   []string
	unshallow []string
}

func writeObjHeader(buf *bytes.Buffer, typCode byte, size int64) {
	b := typCode<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		buf.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(b)
}

var objTypeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// buildPack serializes objects into a version-2 pack, deterministically ordered by oid.
func buildPack(objects []*gObj) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	buf.Write([]byte{0, 0, 0, 2})
	count := len(objects)
	buf.Write([]byte{byte(count >> 24), byte(count >> 16), byte(count >> 8), byte(count)})
	for _, obj := range objects {
		code, ok := objTypeCode[obj.typ]
		if !ok {
			code = 3
		}
		writeObjHeader(&buf, code, int64(len(obj.data)))
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		zw.Write(obj.data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// packObjects returns the objects of a set sorted by oid (deterministic response bytes).
func packObjects(repo *Repo, set map[string]bool) []*gObj {
	oids := make([]string, 0, len(set))
	for oid := range set {
		oids = append(oids, oid)
	}
	sortStrings(oids)
	objs := make([]*gObj, 0, len(oids))
	for _, oid := range oids {
		if obj := repo.Objects[oid]; obj != nil {
			objs = append(objs, obj)
		}
	}
	return objs
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// ---------------------------------------------------------------- pack reading

type rawPackEntry struct {
	offset int
	typ    string
	data   []byte
}

// readPackDir parses a pack and returns every resolved object. Deltas are applied against bases
// found inside the pack or already stored in the repository.
func readPackDir(data []byte, repo *Repo) ([]*gObj, error) {
	if len(data) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(data[:4]) != "PACK" {
		return nil, errors.New("bad pack signature")
	}
	version := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	body := data[:len(data)-20]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	pos := 12
	byOffset := make(map[int]*rawPackEntry)
	byOID := make(map[string]*gObj)
	var out []*gObj
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(body) {
			return nil, errors.New("truncated pack")
		}
		b := body[pos]
		pos++
		code := (b >> 4) & 7
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(body) {
				return nil, errors.New("truncated pack header")
			}
			b = body[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		baseOffset := 0
		baseOID := ""
		switch code {
		case 6:
			if pos >= len(body) {
				return nil, errors.New("truncated ofs-delta")
			}
			c := body[pos]
			pos++
			ofs := int64(c & 0x7f)
			for c&0x80 != 0 {
				if pos >= len(body) {
					return nil, errors.New("truncated ofs-delta")
				}
				c = body[pos]
				pos++
				ofs = ((ofs + 1) << 7) | int64(c&0x7f)
			}
			baseOffset = start - int(ofs)
		case 7:
			if pos+20 > len(body) {
				return nil, errors.New("truncated ref-delta")
			}
			baseOID = hex.EncodeToString(body[pos : pos+20])
			pos += 20
		}
		inflated, consumed, err := inflateFrom(body[pos:])
		if err != nil {
			return nil, err
		}
		pos += consumed
		if int64(len(inflated)) != size {
			return nil, fmt.Errorf("object size mismatch: header %d, data %d", size, len(inflated))
		}
		entry := &rawPackEntry{offset: start, data: inflated}
		var typ string
		switch code {
		case 1:
			typ = "commit"
		case 2:
			typ = "tree"
		case 3:
			typ = "blob"
		case 4:
			typ = "tag"
		case 6:
			base, ok := byOffset[baseOffset]
			if !ok {
				return nil, errors.New("ofs-delta base not found")
			}
			typ = base.typ
			resolved, err := applyDelta(base.data, inflated)
			if err != nil {
				return nil, err
			}
			entry.data = resolved
		case 7:
			var base *gObj
			if obj, ok := byOID[baseOID]; ok {
				base = obj
			} else if obj, ok := repo.Objects[baseOID]; ok {
				base = obj
			} else {
				return nil, errors.New("ref-delta base not found")
			}
			typ = base.typ
			resolved, err := applyDelta(base.data, inflated)
			if err != nil {
				return nil, err
			}
			entry.data = resolved
		default:
			return nil, fmt.Errorf("bad object type %d", code)
		}
		entry.typ = typ
		byOffset[start] = entry
		obj := &gObj{typ: typ, data: entry.data}
		byOID[objOID(typ, entry.data)] = obj
		out = append(out, obj)
	}
	if pos != len(body) {
		return nil, errors.New("trailing bytes after pack objects")
	}
	return out, nil
}

// inflateFrom inflates one zlib stream and reports how many input bytes it consumed.
func inflateFrom(data []byte) ([]byte, int, error) {
	r := bytes.NewReader(data)
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(zr)
	closeErr := zr.Close()
	consumed := len(data) - r.Len()
	if err != nil {
		return nil, consumed, err
	}
	if closeErr != nil {
		return nil, consumed, closeErr
	}
	return out, consumed, nil
}

func readVarintLE(data []byte, pos int) (uint64, int, error) {
	var value uint64
	var shift uint
	for {
		if pos >= len(data) {
			return 0, pos, errors.New("truncated delta varint")
		}
		b := data[pos]
		pos++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, pos, nil
		}
		shift += 7
		if shift > 63 {
			return 0, pos, errors.New("delta varint overflow")
		}
	}
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	sourceSize, next, err := readVarintLE(delta, pos)
	if err != nil {
		return nil, err
	}
	pos = next
	targetSize, next, err := readVarintLE(delta, pos)
	if err != nil {
		return nil, err
	}
	pos = next
	if uint64(len(base)) != sourceSize {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, 0, targetSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		switch {
		case cmd&0x80 != 0:
			var offset, size uint32
			for bit := 0; bit < 4; bit++ {
				if cmd&(1<<uint(bit)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					offset |= uint32(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if cmd&(0x10<<uint(bit)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					size |= uint32(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if int(offset)+int(size) > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[offset:int(offset)+int(size)]...)
		case cmd != 0:
			n := int(cmd)
			if pos+n > len(delta) {
				return nil, errors.New("truncated delta literal")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		default:
			return nil, errors.New("invalid delta opcode 0")
		}
	}
	if uint64(len(out)) != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out, nil
}
