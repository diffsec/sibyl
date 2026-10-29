// Package gguf reads the metadata of a GGUF model file. It reads only the
// header, never the tensor data, and it keeps only the keys that a caller
// asks for. It can also list the tensors with their sizes. Every length in the file is checked against a limit, so a
// damaged or hostile file cannot make the reader allocate without bound.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// Limits for one file. A real header holds a tokenizer of a few hundred
// thousand strings and a chat template of tens of kilobytes.
const (
	maxKeys      = 1 << 16
	maxString    = 16 << 20
	maxArray     = 1 << 24
	maxHeaderLen = 512 << 20 // bytes read before the reader gives up
	maxDepth     = 4         // nested arrays
	maxTensors   = 1 << 20
	maxDims      = 8
)

// defaultAlignment is the alignment of the tensor data when the file does
// not set general.alignment.
const defaultAlignment = 32

// Value types in the GGUF format.
const (
	typeUint8 uint32 = iota
	typeInt8
	typeUint16
	typeInt16
	typeUint32
	typeInt32
	typeFloat32
	typeBool
	typeString
	typeArray
	typeUint64
	typeInt64
	typeFloat64
)

// Value is one metadata value. Numbers are kept as float64, which holds
// every integer in a model header exactly. An array of numbers or booleans
// fills Array. Strings fill Str. Arrays of strings are skipped.
type Value struct {
	Num   float64
	Str   string
	Array []float64
	IsArr bool
}

// Int returns the value as an int.
func (v Value) Int() int { return int(v.Num) }

// ErrNotGGUF means the file does not start with the GGUF magic.
var ErrNotGGUF = errors.New("not a GGUF file")

// ReadFile reads the metadata of path and returns the values of the keys
// for which want returns true.
func ReadFile(path string, want func(key string) bool) (map[string]Value, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Read(f, want)
}

// Read reads GGUF metadata from r.
func Read(r io.Reader, want func(key string) bool) (map[string]Value, error) {
	d, n, err := open(r)
	if err != nil {
		return nil, err
	}
	return d.keys(n, want)
}

// Tensor is one tensor of a model file. Size is its share of the data
// section, alignment padding included.
type Tensor struct {
	Name string
	Size int64
}

// ReadTensors lists the tensors of path.
func ReadTensors(path string) ([]Tensor, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return Tensors(f, fi.Size())
}

// Tensors lists the tensors of a model file of fileSize bytes read from r.
// The sizes come from the data offsets, so the reader needs no table of
// the quantization types.
func Tensors(r io.Reader, fileSize int64) ([]Tensor, error) {
	d, n, err := open(r)
	if err != nil {
		return nil, err
	}
	count := d.tensors
	if count > maxTensors {
		return nil, fmt.Errorf("GGUF header lists %d tensors", count)
	}
	vals, err := d.keys(n, func(k string) bool { return k == "general.alignment" })
	if err != nil {
		return nil, err
	}
	align := uint64(defaultAlignment)
	if v, ok := vals["general.alignment"]; ok && v.Num > 0 {
		align = uint64(v.Num)
	}
	type info struct {
		name   string
		offset uint64
	}
	infos := make([]info, 0, count)
	for i := uint64(0); i < count; i++ {
		name := d.str()
		dims := d.u32()
		if d.err == nil && dims > maxDims {
			return nil, fmt.Errorf("tensor %q has %d dimensions", name, dims)
		}
		for j := uint32(0); j < dims; j++ {
			_ = d.u64()
		}
		_ = d.u32() // type
		off := d.u64()
		if d.err != nil {
			return nil, d.err
		}
		infos = append(infos, info{name, off})
	}
	start := (d.pos + align - 1) / align * align
	if fileSize < 0 || start > uint64(fileSize) {
		return nil, errors.New("the tensor data starts after the end of the file")
	}
	end := uint64(fileSize) - start
	sorted := make([]int, len(infos))
	for i := range sorted {
		sorted[i] = i
	}
	sort.Slice(sorted, func(a, b int) bool { return infos[sorted[a]].offset < infos[sorted[b]].offset })
	out := make([]Tensor, len(infos))
	for k, i := range sorted {
		next := end
		if k+1 < len(sorted) {
			next = infos[sorted[k+1]].offset
		}
		if infos[i].offset > next {
			return nil, fmt.Errorf("tensor %q starts after the end of the file", infos[i].name)
		}
		out[i] = Tensor{Name: infos[i].name, Size: int64(next - infos[i].offset)}
	}
	return out, nil
}

// open checks the magic and the version and returns the number of keys.
func open(r io.Reader) (*decoder, uint64, error) {
	d := &decoder{r: bufio.NewReaderSize(io.LimitReader(r, maxHeaderLen), 1<<20)}
	var magic [4]byte
	if _, err := io.ReadFull(d.r, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return nil, 0, ErrNotGGUF
	}
	d.pos = 4
	version := d.u32()
	if d.err == nil && (version < 2 || version > 3) {
		return nil, 0, fmt.Errorf("GGUF version %d is not supported", version)
	}
	d.tensors = d.u64()
	n := d.u64()
	if d.err != nil {
		return nil, 0, d.err
	}
	if n > maxKeys {
		return nil, 0, fmt.Errorf("GGUF header lists %d keys", n)
	}
	return d, n, nil
}

// keys reads n key-value pairs and keeps the wanted ones.
func (d *decoder) keys(n uint64, want func(key string) bool) (map[string]Value, error) {
	out := map[string]Value{}
	for i := uint64(0); i < n; i++ {
		key := d.str()
		t := d.u32()
		if d.err != nil {
			return nil, d.err
		}
		keep := want(key)
		v := d.value(t, keep, 0)
		if d.err != nil {
			return nil, fmt.Errorf("key %q: %w", key, d.err)
		}
		if keep {
			out[key] = v
		}
	}
	return out, nil
}

type decoder struct {
	r       *bufio.Reader
	err     error
	buf     [8]byte
	pos     uint64 // bytes read so far
	tensors uint64 // the tensor count in the header
}

func (d *decoder) read(n int) []byte {
	if d.err != nil {
		return d.buf[:n]
	}
	if _, err := io.ReadFull(d.r, d.buf[:n]); err != nil {
		d.err = fmt.Errorf("truncated header: %w", err)
	}
	d.pos += uint64(n)
	return d.buf[:n]
}

func (d *decoder) u32() uint32 { return binary.LittleEndian.Uint32(d.read(4)) }
func (d *decoder) u64() uint64 { return binary.LittleEndian.Uint64(d.read(8)) }

func (d *decoder) str() string {
	n := d.u64()
	if d.err != nil {
		return ""
	}
	if n > maxString {
		d.err = fmt.Errorf("string of %d bytes", n)
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(d.r, b); err != nil {
		d.err = fmt.Errorf("truncated header: %w", err)
	}
	d.pos += n
	return string(b)
}

func (d *decoder) skip(n uint64) {
	if d.err != nil {
		return
	}
	if _, err := d.r.Discard(int(n)); err != nil {
		d.err = fmt.Errorf("truncated header: %w", err)
	}
	d.pos += n
}

// size is the byte size of a fixed-size type, or 0.
func size(t uint32) uint64 {
	switch t {
	case typeUint8, typeInt8, typeBool:
		return 1
	case typeUint16, typeInt16:
		return 2
	case typeUint32, typeInt32, typeFloat32:
		return 4
	case typeUint64, typeInt64, typeFloat64:
		return 8
	}
	return 0
}

func (d *decoder) num(t uint32) float64 {
	switch t {
	case typeUint8:
		return float64(d.read(1)[0])
	case typeInt8:
		return float64(int8(d.read(1)[0]))
	case typeBool:
		if d.read(1)[0] != 0 {
			return 1
		}
		return 0
	case typeUint16:
		return float64(binary.LittleEndian.Uint16(d.read(2)))
	case typeInt16:
		return float64(int16(binary.LittleEndian.Uint16(d.read(2))))
	case typeUint32:
		return float64(d.u32())
	case typeInt32:
		return float64(int32(d.u32()))
	case typeFloat32:
		return float64(math.Float32frombits(d.u32()))
	case typeUint64:
		return float64(d.u64())
	case typeInt64:
		return float64(int64(d.u64()))
	case typeFloat64:
		return math.Float64frombits(d.u64())
	}
	d.err = fmt.Errorf("unknown value type %d", t)
	return 0
}

// value reads one value of type t. With keep false it only skips it.
func (d *decoder) value(t uint32, keep bool, depth int) Value {
	switch t {
	case typeString:
		if !keep {
			d.skip(d.u64Checked(maxString))
			return Value{}
		}
		return Value{Str: d.str()}
	case typeArray:
		if depth >= maxDepth {
			d.err = errors.New("arrays nested too deep")
			return Value{}
		}
		et := d.u32()
		n := d.u64Checked(maxArray)
		if d.err != nil {
			return Value{}
		}
		if sz := size(et); sz > 0 {
			if !keep {
				d.skip(n * sz)
				return Value{IsArr: true}
			}
			v := Value{IsArr: true, Array: make([]float64, 0, n)}
			for i := uint64(0); i < n && d.err == nil; i++ {
				v.Array = append(v.Array, d.num(et))
			}
			return v
		}
		for i := uint64(0); i < n && d.err == nil; i++ {
			d.value(et, false, depth+1)
		}
		return Value{IsArr: true}
	default:
		if sz := size(t); sz == 0 {
			d.err = fmt.Errorf("unknown value type %d", t)
			return Value{}
		}
		return Value{Num: d.num(t)}
	}
}

func (d *decoder) u64Checked(limit uint64) uint64 {
	n := d.u64()
	if d.err == nil && n > limit {
		d.err = fmt.Errorf("length %d is over the limit %d", n, limit)
		return 0
	}
	return n
}
