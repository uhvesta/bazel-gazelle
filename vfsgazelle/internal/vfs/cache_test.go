package vfs

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

type testModel struct {
	Path   string   `json:"path"`
	Length int      `json:"length"`
	Lines  []string `json:"lines"`
}

type countingParser struct {
	key     string
	version string
	parses  int
}

func (p *countingParser) Key() string          { return p.key }
func (p *countingParser) CacheVersion() string { return p.version }

func (p *countingParser) Parse(path string, data []byte) (any, error) {
	p.parses++
	return testModel{
		Path:   path,
		Length: len(data),
		Lines:  splitLines(data),
	}, nil
}

func (p *countingParser) Encode(model any) ([]byte, error) {
	return json.Marshal(model)
}

func (p *countingParser) Decode(data []byte) (any, error) {
	var model testModel
	err := json.Unmarshal(data, &model)
	return model, err
}

func splitLines(data []byte) []string {
	parts := bytes.Split(data, []byte("\n"))
	out := make([]string, len(parts))
	for i, line := range parts {
		out[i] = string(line)
	}
	return out
}

func TestCacheBuilderReusesParsedModelForSameContent(t *testing.T) {
	builder := NewCacheBuilder(nil)
	parser := &countingParser{key: "test/model", version: "v1"}

	first, err := builder.Parse("foo.txt", []byte("a\nb"), parser)
	if err != nil {
		t.Fatal(err)
	}
	second, err := builder.Parse("foo.txt", []byte("a\nb"), parser)
	if err != nil {
		t.Fatal(err)
	}

	if parser.parses != 1 {
		t.Fatalf("parser called %d times, want 1", parser.parses)
	}
	if first.CacheHit {
		t.Fatal("first parse should not be a cache hit")
	}
	if !second.CacheHit {
		t.Fatal("second parse should be a cache hit")
	}
	if !reflect.DeepEqual(first.Model, second.Model) {
		t.Fatalf("models differ (-want +got): %#v %#v", first.Model, second.Model)
	}
	if first.ModelHash != second.ModelHash {
		t.Fatalf("model hash mismatch: %q != %q", first.ModelHash, second.ModelHash)
	}
}

func TestCacheBuilderInvalidatesOnContentChange(t *testing.T) {
	builder := NewCacheBuilder(nil)
	parser := &countingParser{key: "test/model", version: "v1"}

	_, err := builder.Parse("foo.txt", []byte("old"), parser)
	if err != nil {
		t.Fatal(err)
	}
	got, err := builder.Parse("foo.txt", []byte("new"), parser)
	if err != nil {
		t.Fatal(err)
	}

	if parser.parses != 2 {
		t.Fatalf("parser called %d times, want 2", parser.parses)
	}
	if got.CacheHit {
		t.Fatal("changed content should not be a cache hit")
	}
}

func TestCacheBuilderInvalidatesOnParserVersionChange(t *testing.T) {
	builder := NewCacheBuilder(nil)
	parserV1 := &countingParser{key: "test/model", version: "v1"}
	parserV2 := &countingParser{key: "test/model", version: "v2"}

	_, err := builder.Parse("foo.txt", []byte("same"), parserV1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := builder.Parse("foo.txt", []byte("same"), parserV2)
	if err != nil {
		t.Fatal(err)
	}

	if got.CacheHit {
		t.Fatal("version change should force a reparse")
	}
	if parserV1.parses != 1 || parserV2.parses != 1 {
		t.Fatalf("unexpected parse counts: v1=%d v2=%d", parserV1.parses, parserV2.parses)
	}
}

func TestCacheBuilderSupportsMultipleParserKeysPerPath(t *testing.T) {
	builder := NewCacheBuilder(nil)
	parserA := &countingParser{key: "test/a", version: "v1"}
	parserB := &countingParser{key: "test/b", version: "v1"}

	_, err := builder.Parse("foo.txt", []byte("same"), parserA)
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.Parse("foo.txt", []byte("same"), parserB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.Parse("foo.txt", []byte("same"), parserA)
	if err != nil {
		t.Fatal(err)
	}

	if parserA.parses != 1 {
		t.Fatalf("parserA called %d times, want 1", parserA.parses)
	}
	if parserB.parses != 1 {
		t.Fatalf("parserB called %d times, want 1", parserB.parses)
	}
}

func TestFrozenCacheReturnsOnlyExistingModels(t *testing.T) {
	builder := NewCacheBuilder(nil)
	parser := &countingParser{key: "test/model", version: "v1"}

	first, err := builder.Parse("foo.txt", []byte("persist"), parser)
	if err != nil {
		t.Fatal(err)
	}
	cache := builder.Freeze()

	second, hit, err := cache.Get("foo.txt", []byte("persist"), parser)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("frozen cache should hit for parsed entry")
	}
	if !reflect.DeepEqual(first.Model, second.Model) {
		t.Fatalf("models differ after freeze (-want +got): %#v %#v", first.Model, second.Model)
	}

	_, hit, err = cache.Get("foo.txt", []byte("other"), parser)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("frozen cache should miss for changed content")
	}
}

func TestCacheRoundTripPersistence(t *testing.T) {
	builder := NewCacheBuilder(nil)
	parser := &countingParser{key: "test/model", version: "v1"}

	first, err := builder.Parse("foo.txt", []byte("persist"), parser)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := builder.Freeze().Save(&buf); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(&buf)
	if err != nil {
		t.Fatal(err)
	}

	second, hit, err := loaded.Get("foo.txt", []byte("persist"), parser)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("loaded cache should hit")
	}
	if !reflect.DeepEqual(first.Model, second.Model) {
		t.Fatalf("models differ after load (-want +got): %#v %#v", first.Model, second.Model)
	}
}
