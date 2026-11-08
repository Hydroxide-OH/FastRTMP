package rtmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// AMF0 type markers.
const (
	amfNumber      = 0x00
	amfBoolean     = 0x01
	amfString      = 0x02
	amfObject      = 0x03
	amfMovieClip   = 0x04
	amfNull        = 0x05
	amfUndefined   = 0x06
	amfReference   = 0x07
	amfEcmaArray   = 0x08
	amfObjectEnd   = 0x09
	amfStrictArray = 0x0A
	amfDate        = 0x0B
	amfLongString  = 0x0C
	amfUnsupported = 0x0D
	amfRecordSet   = 0x0E
	amfXmlDoc      = 0x0F
	amfTypedObject = 0x10
	amfAvmPlus     = 0x11
)

// Prop is an ordered key/value pair used to build AMF0 objects with a
// deterministic field order (nicer on the wire and easier to debug).
type Prop struct {
	Key string
	Val any
}

// Object is an ordered AMF0 object.
type Object []Prop

// ECMAArray is the 0x08 "associative array" flavour of an AMF0 object.
type ECMAArray []Prop

var errAMFShort = errors.New("amf0: unexpected end of buffer")

// ---------- decode ----------

// DecodeAMF0 decodes every value contained in b. Trailing garbage is ignored,
// which mirrors how RTMP peers treat extra command arguments.
func DecodeAMF0(b []byte) ([]any, error) {
	d := &amfDecoder{b: b}
	var out []any
	for d.pos < len(d.b) {
		v, err := d.value()
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, nil
}

type amfDecoder struct {
	b   []byte
	pos int
}

func (d *amfDecoder) read(n int) ([]byte, error) {
	if d.pos+n > len(d.b) {
		return nil, errAMFShort
	}
	v := d.b[d.pos : d.pos+n]
	d.pos += n
	return v, nil
}

func (d *amfDecoder) u8() (uint8, error) {
	v, err := d.read(1)
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

func (d *amfDecoder) u16() (uint16, error) {
	v, err := d.read(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(v), nil
}

func (d *amfDecoder) u32() (uint32, error) {
	v, err := d.read(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(v), nil
}

func (d *amfDecoder) value() (any, error) {
	marker, err := d.u8()
	if err != nil {
		return nil, err
	}
	switch marker {
	case amfNumber:
		v, err := d.read(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(v)), nil
	case amfBoolean:
		v, err := d.u8()
		if err != nil {
			return nil, err
		}
		return v != 0, nil
	case amfString:
		n, err := d.u16()
		if err != nil {
			return nil, err
		}
		v, err := d.read(int(n))
		if err != nil {
			return nil, err
		}
		return string(v), nil
	case amfLongString, amfXmlDoc:
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		v, err := d.read(int(n))
		if err != nil {
			return nil, err
		}
		return string(v), nil
	case amfObject, amfTypedObject:
		if marker == amfTypedObject {
			if _, err := d.string16(); err != nil {
				return nil, err
			}
		}
		return d.props(false)
	case amfEcmaArray:
		// count is advisory; the object end marker terminates the array.
		if _, err := d.u32(); err != nil {
			return nil, err
		}
		return d.props(false)
	case amfStrictArray:
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		if n > 1<<20 {
			return nil, fmt.Errorf("amf0: strict array too long (%d)", n)
		}
		out := make([]any, 0, n)
		for i := uint32(0); i < n; i++ {
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case amfNull, amfUndefined, amfUnsupported:
		return nil, nil
	case amfDate:
		v, err := d.read(8)
		if err != nil {
			return nil, err
		}
		if _, err := d.read(2); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(v)), nil
	case amfReference:
		if _, err := d.u16(); err != nil {
			return nil, err
		}
		return nil, nil
	case amfAvmPlus:
		// AMF3 payloads are rare on RTMP control channels; skip gracefully.
		return nil, nil
	default:
		return nil, fmt.Errorf("amf0: unsupported marker 0x%02x", marker)
	}
}

func (d *amfDecoder) string16() (string, error) {
	n, err := d.u16()
	if err != nil {
		return "", err
	}
	v, err := d.read(int(n))
	if err != nil {
		return "", err
	}
	return string(v), nil
}

func (d *amfDecoder) props(strict bool) (map[string]any, error) {
	out := make(map[string]any, 8)
	for {
		if d.pos+3 > len(d.b) {
			return nil, errAMFShort
		}
		// Object end: 0x000009
		if d.b[d.pos] == 0 && d.b[d.pos+1] == 0 && d.b[d.pos+2] == amfObjectEnd {
			d.pos += 3
			return out, nil
		}
		key, err := d.string16()
		if err != nil {
			return nil, err
		}
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		out[key] = v
		if d.pos >= len(d.b) {
			return nil, errAMFShort
		}
	}
}

func float64FromBits(u uint64) float64 {
	return math.Float64frombits(u)
}

// ---------- encode ----------

// EncodeAMF0 encodes all values back to back.
func EncodeAMF0(vals ...any) ([]byte, error) {
	buf := make([]byte, 0, 256)
	for _, v := range vals {
		var err error
		buf, err = appendAMF0(buf, v)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func appendAMF0(b []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(b, amfNull), nil
	case bool:
		val := byte(0)
		if t {
			val = 1
		}
		return append(b, amfBoolean, val), nil
	case string:
		return appendAMF0String(b, t), nil
	case []byte:
		return appendAMF0String(b, string(t)), nil
	case int:
		return appendAMF0Number(b, float64(t)), nil
	case int32:
		return appendAMF0Number(b, float64(t)), nil
	case int64:
		return appendAMF0Number(b, float64(t)), nil
	case uint32:
		return appendAMF0Number(b, float64(t)), nil
	case uint64:
		return appendAMF0Number(b, float64(t)), nil
	case float32:
		return appendAMF0Number(b, float64(t)), nil
	case float64:
		return appendAMF0Number(b, t), nil
	case Object:
		b = append(b, amfObject)
		return appendProps(b, t)
	case ECMAArray:
		b = append(b, amfEcmaArray)
		b = appendU32(b, uint32(len(t)))
		return appendProps(b, Object(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		props := make(Object, 0, len(keys))
		for _, k := range keys {
			props = append(props, Prop{Key: k, Val: t[k]})
		}
		b = append(b, amfObject)
		return appendProps(b, props)
	case []any:
		b = append(b, amfStrictArray)
		b = appendU32(b, uint32(len(t)))
		for _, item := range t {
			var err error
			b, err = appendAMF0(b, item)
			if err != nil {
				return nil, err
			}
		}
		return b, nil
	default:
		return nil, fmt.Errorf("amf0: cannot encode %T", v)
	}
}

func appendProps(b []byte, props Object) ([]byte, error) {
	for _, p := range props {
		// Object property names are length-prefixed *without* a type marker.
		if len(p.Key) > 0xFFFF {
			return nil, fmt.Errorf("amf0: property name too long (%d)", len(p.Key))
		}
		b = appendU16(b, uint16(len(p.Key)))
		b = append(b, p.Key...)
		if p.Val == nil {
			b = append(b, amfNull)
			continue
		}
		var err error
		b, err = appendAMF0(b, p.Val)
		if err != nil {
			return nil, err
		}
	}
	return append(b, 0, 0, amfObjectEnd), nil
}

func appendAMF0String(b []byte, s string) []byte {
	if len(s) > 0xFFFF {
		b = append(b, amfLongString)
		b = appendU32(b, uint32(len(s)))
		return append(b, s...)
	}
	b = append(b, amfString)
	b = appendU16(b, uint16(len(s)))
	return append(b, s...)
}

func appendAMF0Number(b []byte, f float64) []byte {
	b = append(b, amfNumber)
	return appendU64(b, math.Float64bits(f))
}

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendU24(b []byte, v uint32) []byte {
	return append(b, byte(v>>16), byte(v>>8), byte(v))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendU64(b []byte, v uint64) []byte {
	return append(b, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
