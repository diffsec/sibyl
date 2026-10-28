package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func all(string) bool { return true }

func TestReadKeepsWantedKeys(t *testing.T) {
	data, err := Encode(map[string]any{
		"general.architecture":       "llama",
		"general.sampling.temp":      float32(0.6),
		"llama.context_length":       uint32(131072),
		"llama.attention.head_count": 32,
		"llama.recurrent":            []bool{true, false, true},
		"llama.kv":                   []int{8, 8, 0},
		"tokenizer.ggml.tokens":      []string{"a", "bb", "ccc"},
		"tokenizer.chat_template":    strings.Repeat("x", 100000),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(bytes.NewReader(data), func(k string) bool { return !strings.HasPrefix(k, "tokenizer.") })
	if err != nil {
		t.Fatal(err)
	}
	if got["general.architecture"].Str != "llama" || got["llama.context_length"].Int() != 131072 ||
		got["llama.attention.head_count"].Int() != 32 {
		t.Errorf("scalars = %+v", got)
	}
	if v := got["general.sampling.temp"].Num; v < 0.5999 || v > 0.6001 {
		t.Errorf("temp = %v", v)
	}
	if r := got["llama.recurrent"]; !r.IsArr || len(r.Array) != 3 || r.Array[0] != 1 || r.Array[1] != 0 {
		t.Errorf("bool array = %+v", r)
	}
	if _, ok := got["tokenizer.ggml.tokens"]; ok {
		t.Error("an unwanted key was kept")
	}
}

func TestReadRefusesBadInput(t *testing.T) {
	good, _ := Encode(map[string]any{"general.architecture": "llama"})
	if _, err := Read(strings.NewReader("GGML...."), all); !errors.Is(err, ErrNotGGUF) {
		t.Errorf("bad magic: %v", err)
	}
	if _, err := Read(bytes.NewReader(good[:len(good)-3]), all); err == nil {
		t.Error("a truncated header was accepted")
	}

	// A string that claims to be 1 TiB long must fail before any allocation.
	var b bytes.Buffer
	b.WriteString("GGUF")
	for _, v := range []any{uint32(3), uint64(0), uint64(1), uint64(4)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("evil")
	_ = binary.Write(&b, binary.LittleEndian, typeString)
	_ = binary.Write(&b, binary.LittleEndian, uint64(1<<40))
	if _, err := Read(&b, all); err == nil || !strings.Contains(err.Error(), "1099511627776") {
		t.Errorf("huge string: %v", err)
	}

	// So must a header that claims millions of keys.
	b.Reset()
	b.WriteString("GGUF")
	for _, v := range []any{uint32(3), uint64(0), uint64(1 << 40)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	if _, err := Read(&b, all); err == nil {
		t.Error("a huge key count was accepted")
	}
}
