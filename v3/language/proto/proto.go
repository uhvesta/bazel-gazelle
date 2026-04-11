package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
)

const protoName = "proto"

type protoLang struct{}

func (*protoLang) Name() string { return protoName }

func NewLanguage() v3language.Language {
	return &protoLang{}
}

var activeRepo *vfs.Snapshot

func (*protoLang) RegisterParsers(reg *vfs.Registry) error {
	return reg.Register(protoFileParser{}, vfs.MatchExtension(".proto"))
}

type protoFileParser struct{}

func (protoFileParser) Key() string     { return "proto/fileinfo" }
func (protoFileParser) Version() string { return "v1" }
func (protoFileParser) Parse(path string, data []byte) (any, error) {
	return parseProtoFileInfo(path, data), nil
}
func (protoFileParser) Encode(model any) ([]byte, error) { return json.Marshal(model) }
func (protoFileParser) Decode(data []byte) (any, error) {
	var info FileInfo
	err := json.Unmarshal(data, &info)
	return info, err
}

// FileInfo contains metadata extracted from a .proto file.
type FileInfo struct {
	Path, Name string

	PackageName string
	Options     []Option
	Imports     []string
	HasServices bool
	Services    []string
	Messages    []string
	Enums       []string
}

type Option struct {
	Key, Value string
}

var protoRe = buildProtoRegexp()

func ProtoFileInfo(dir, name string) FileInfo {
	fullPath := filepath.Join(dir, name)
	if activeRepo != nil {
		rel, err := filepath.Rel(activeRepo.Root, fullPath)
		if err == nil {
			rel = filepath.ToSlash(rel)
			if result, err := activeRepo.GetModel(rel, "proto/fileinfo"); err == nil {
				return result.Model.(FileInfo)
			}
			if data, err := activeRepo.ReadFile(rel); err == nil {
				return parseProtoFileInfo(rel, data)
			}
		}
	}
	return FileInfo{Path: fullPath, Name: name}
}

func parseProtoFileInfo(path string, content []byte) FileInfo {
	info := FileInfo{
		Path: path,
		Name: pathBase(path),
	}
	for _, match := range protoRe.FindAllSubmatch(content, -1) {
		switch {
		case match[1] != nil:
			info.Imports = append(info.Imports, unquoteProtoString(match[1]))
		case match[2] != nil:
			pkg := string(match[2])
			if info.PackageName == "" {
				info.PackageName = pkg
			}
		case match[3] != nil:
			info.Options = append(info.Options, Option{
				Key:   string(match[3]),
				Value: unquoteProtoString(match[4]),
			})
		case match[5] != nil:
			info.HasServices = true
			if serviceName, ok := extractObjectName(string(match[5])); ok {
				info.Services = append(info.Services, serviceName)
			}
		case match[6] != nil:
			if messageName, ok := extractObjectName(string(match[6])); ok {
				info.Messages = append(info.Messages, messageName)
			}
		case match[7] != nil:
			if enumName, ok := extractObjectName(string(match[7])); ok {
				info.Enums = append(info.Enums, enumName)
			}
		}
	}
	sort.Strings(info.Imports)
	return info
}

func buildProtoRegexp() *regexp.Regexp {
	hexEscape := `\\[xX][0-9a-fA-f]{2}`
	octEscape := `\\[0-7]{3}`
	charEscape := `\\[abfnrtv'"\\]`
	charValue := strings.Join([]string{hexEscape, octEscape, charEscape, "[^\x00\\'\\\"\\\\]"}, "|")
	strLit := `'(?:` + charValue + `|")*'|"(?:` + charValue + `|')*"`
	ident := `[A-Za-z][A-Za-z0-9_]*`
	fullIdent := ident + `(?:\.` + ident + `)*`
	importStmt := `\bimport\s*(?:public|weak)?\s*(` + strLit + `)\s*;`
	packageStmt := `\bpackage\s*(` + fullIdent + `)\s*;`
	optionStmt := `\boption\s*(` + fullIdent + `)\s*=\s*(` + strLit + `)\s*;`
	serviceStmt := `(service\s+` + ident + `\s*{)`
	messageStmt := `(message\s+` + ident + `\s*{)`
	enumStmt := `(enum\s+` + ident + `\s*{)`
	comment := `//[^\n]*`
	return regexp.MustCompile(strings.Join([]string{importStmt, packageStmt, optionStmt, serviceStmt, messageStmt, enumStmt, comment}, "|"))
}

func unquoteProtoString(q []byte) string {
	noQuotes := bytes.Split(q[1:len(q)-1], []byte{'"'})
	if len(noQuotes) != 1 {
		for i := 0; i < len(noQuotes)-1; i++ {
			if len(noQuotes[i]) == 0 || noQuotes[i][len(noQuotes[i])-1] != '\\' {
				noQuotes[i] = append(noQuotes[i], '\\')
			}
		}
		q = append([]byte{'"'}, bytes.Join(noQuotes, []byte{'"'})...)
		q = append(q, '"')
	}
	if q[0] == '\'' {
		q[0] = '"'
		q[len(q)-1] = '"'
	}
	s, err := strconv.Unquote(string(q))
	if err != nil {
		panic(fmt.Sprintf("unquoting string literal %s from proto: %v", q, err))
	}
	return s
}

func extractObjectName(fullMatch string) (string, bool) {
	fields := strings.Fields(fullMatch)
	if len(fields) < 2 {
		return "", false
	}
	return strings.TrimSuffix(fields[1], "{"), true
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func withRepo(repo *vfs.Snapshot, fn func()) {
	prev := activeRepo
	activeRepo = repo
	defer func() { activeRepo = prev }()
	fn()
}
