package gguf

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Encode writes a GGUF version 3 header with no tensors. Tests use it to
// make model files. A value is a string, a bool, an int, a uint32, a
// float32, a float64, or a slice of bools, ints or strings. Keys are
// written in sorted order, except general.architecture, which comes first,
// as in the files that llama.cpp writes.
func Encode(kv map[string]any) ([]byte, error) {
	return EncodeTensors(kv, nil)
}

// EncodeTensors writes a GGUF file with the tensors, each an int8 vector of
// its Size, followed by zeroed data at the default alignment.
func EncodeTensors(kv map[string]any, tensors []Tensor) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("GGUF")
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { le(uint64(len(s))); b.WriteString(s) }
	le(uint32(3))
	le(uint64(len(tensors)))
	le(uint64(len(kv)))
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == "general.architecture") != (keys[j] == "general.architecture") {
			return keys[i] == "general.architecture"
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		str(k)
		switch v := kv[k].(type) {
		case string:
			le(typeString)
			str(v)
		case bool:
			le(typeBool)
			le(v)
		case int:
			le(typeInt32)
			le(int32(v))
		case uint32:
			le(typeUint32)
			le(v)
		case float32:
			le(typeFloat32)
			le(math.Float32bits(v))
		case float64:
			le(typeFloat64)
			le(math.Float64bits(v))
		case []bool:
			le(typeArray)
			le(typeBool)
			le(uint64(len(v)))
			for _, x := range v {
				le(x)
			}
		case []int:
			le(typeArray)
			le(typeInt32)
			le(uint64(len(v)))
			for _, x := range v {
				le(int32(x))
			}
		case []string:
			le(typeArray)
			le(typeString)
			le(uint64(len(v)))
			for _, x := range v {
				str(x)
			}
		default:
			return nil, fmt.Errorf("key %q: unsupported type %T", k, v)
		}
	}
	const typeI8 = 24
	align := func(n int64) int64 { return (n + defaultAlignment - 1) / defaultAlignment * defaultAlignment }
	var off int64
	for _, t := range tensors {
		str(t.Name)
		le(uint32(1))
		le(uint64(t.Size))
		le(uint32(typeI8))
		le(uint64(off))
		off = align(off + t.Size)
	}
	if len(tensors) > 0 {
		b.Write(make([]byte, align(int64(b.Len()))-int64(b.Len())))
		last := tensors[len(tensors)-1]
		b.Write(make([]byte, off-align(last.Size)+last.Size))
	}
	return b.Bytes(), nil
}
