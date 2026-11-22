package feishu

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Feishu persistent WebSocket pbbp2 frame method values.
const (
	frameMethodControl = 0
	frameMethodData    = 1
)

// pbbp2Header mirrors pbbp2.Header (1: key, 2: value).
type pbbp2Header struct {
	Key   string
	Value string
}

// pbbp2Frame mirrors Feishu's pbbp2.Frame protobuf schema.
type pbbp2Frame struct {
	SeqID           uint64
	LogID           uint64
	Service         int32
	Method          int32
	Headers         []pbbp2Header
	PayloadEncoding string
	PayloadType     string
	Payload         []byte
	LogIDNew        string
}

func (f *pbbp2Frame) header(key string) string {
	for _, h := range f.Headers {
		if h.Key == key {
			return h.Value
		}
	}
	return ""
}

func (f pbbp2Frame) marshal() []byte {
	var b []byte
	if f.SeqID != 0 {
		b = appendVarintField(b, 1, f.SeqID)
	}
	if f.LogID != 0 {
		b = appendVarintField(b, 2, f.LogID)
	}
	if f.Service != 0 {
		b = appendVarintField(b, 3, uint64(uint32(f.Service)))
	}
	if f.Method != 0 {
		b = appendVarintField(b, 4, uint64(uint32(f.Method)))
	}
	for _, h := range f.Headers {
		var hb []byte
		if h.Key != "" {
			hb = appendBytesField(hb, 1, []byte(h.Key))
		}
		if h.Value != "" {
			hb = appendBytesField(hb, 2, []byte(h.Value))
		}
		b = appendBytesField(b, 5, hb)
	}
	if f.PayloadEncoding != "" {
		b = appendBytesField(b, 6, []byte(f.PayloadEncoding))
	}
	if f.PayloadType != "" {
		b = appendBytesField(b, 7, []byte(f.PayloadType))
	}
	if len(f.Payload) > 0 {
		b = appendBytesField(b, 8, f.Payload)
	}
	if f.LogIDNew != "" {
		b = appendBytesField(b, 9, []byte(f.LogIDNew))
	}
	return b
}

func unmarshalFrame(data []byte) (pbbp2Frame, error) {
	var f pbbp2Frame
	i := 0
	for i < len(data) {
		tag, n := binary.Uvarint(data[i:])
		if n <= 0 {
			return f, errors.New("pbbp2: invalid field tag")
		}
		i += n
		field := tag >> 3
		wire := tag & 0x7
		switch field {
		case 1:
			if wire != 0 {
				return f, fmt.Errorf("pbbp2: field 1 bad wire type %d", wire)
			}
			v, m := binary.Uvarint(data[i:])
			if m <= 0 {
				return f, errors.New("pbbp2: invalid varint")
			}
			f.SeqID = v
			i += m
		case 2:
			if wire != 0 {
				return f, fmt.Errorf("pbbp2: field 2 bad wire type %d", wire)
			}
			v, m := binary.Uvarint(data[i:])
			if m <= 0 {
				return f, errors.New("pbbp2: invalid varint")
			}
			f.LogID = v
			i += m
		case 3:
			if wire != 0 {
				return f, fmt.Errorf("pbbp2: field 3 bad wire type %d", wire)
			}
			v, m := binary.Uvarint(data[i:])
			if m <= 0 {
				return f, errors.New("pbbp2: invalid varint")
			}
			f.Service = int32(v)
			i += m
		case 4:
			if wire != 0 {
				return f, fmt.Errorf("pbbp2: field 4 bad wire type %d", wire)
			}
			v, m := binary.Uvarint(data[i:])
			if m <= 0 {
				return f, errors.New("pbbp2: invalid varint")
			}
			f.Method = int32(v)
			i += m
		case 5:
			if wire != 2 {
				return f, fmt.Errorf("pbbp2: field 5 bad wire type %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return f, err
			}
			i = next
			h, err := unmarshalHeader(raw)
			if err != nil {
				return f, err
			}
			f.Headers = append(f.Headers, h)
		case 6:
			if wire != 2 {
				return f, fmt.Errorf("pbbp2: field 6 bad wire type %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return f, err
			}
			i = next
			f.PayloadEncoding = string(raw)
		case 7:
			if wire != 2 {
				return f, fmt.Errorf("pbbp2: field 7 bad wire type %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return f, err
			}
			i = next
			f.PayloadType = string(raw)
		case 8:
			if wire != 2 {
				return f, fmt.Errorf("pbbp2: field 8 bad wire type %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return f, err
			}
			i = next
			f.Payload = append([]byte(nil), raw...)
		case 9:
			if wire != 2 {
				return f, fmt.Errorf("pbbp2: field 9 bad wire type %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return f, err
			}
			i = next
			f.LogIDNew = string(raw)
		default:
			next, err := skipWireField(data, i, wire)
			if err != nil {
				return f, err
			}
			i = next
		}
	}
	return f, nil
}

func unmarshalHeader(data []byte) (pbbp2Header, error) {
	var h pbbp2Header
	i := 0
	for i < len(data) {
		tag, n := binary.Uvarint(data[i:])
		if n <= 0 {
			return h, errors.New("pbbp2 header: invalid tag")
		}
		i += n
		field := tag >> 3
		wire := tag & 0x7
		switch field {
		case 1:
			if wire != 2 {
				return h, fmt.Errorf("pbbp2 header: field 1 bad wire %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return h, err
			}
			i = next
			h.Key = string(raw)
		case 2:
			if wire != 2 {
				return h, fmt.Errorf("pbbp2 header: field 2 bad wire %d", wire)
			}
			raw, next, err := consumeBytes(data, i)
			if err != nil {
				return h, err
			}
			i = next
			h.Value = string(raw)
		default:
			next, err := skipWireField(data, i, wire)
			if err != nil {
				return h, err
			}
			i = next
		}
	}
	return h, nil
}

func appendVarintField(b []byte, field uint64, v uint64) []byte {
	b = binary.AppendUvarint(b, field<<3)
	return binary.AppendUvarint(b, v)
}

func appendBytesField(b []byte, field uint64, payload []byte) []byte {
	b = binary.AppendUvarint(b, (field<<3)|2)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	return append(b, payload...)
}

func consumeBytes(data []byte, i int) ([]byte, int, error) {
	l, n := binary.Uvarint(data[i:])
	if n <= 0 {
		return nil, i, errors.New("pbbp2: invalid length varint")
	}
	i += n
	if l > uint64(len(data)-i) {
		return nil, i, errors.New("pbbp2: truncated bytes field")
	}
	return data[i : i+int(l)], i + int(l), nil
}

func skipWireField(data []byte, i int, wire uint64) (int, error) {
	switch wire {
	case 0:
		_, n := binary.Uvarint(data[i:])
		if n <= 0 {
			return i, errors.New("pbbp2: truncated varint")
		}
		return i + n, nil
	case 1:
		if len(data)-i < 8 {
			return i, errors.New("pbbp2: truncated fixed64")
		}
		return i + 8, nil
	case 2:
		_, next, err := consumeBytes(data, i)
		return next, err
	case 5:
		if len(data)-i < 4 {
			return i, errors.New("pbbp2: truncated fixed32")
		}
		return i + 4, nil
	default:
		return i, fmt.Errorf("pbbp2: unsupported wire type %d", wire)
	}
}
