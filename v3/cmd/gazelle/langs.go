package main

import (
	v3language "github.com/uhvesta/bazel-gazelle/v3/language"
	golang "github.com/uhvesta/bazel-gazelle/v3/language/go"
	"github.com/uhvesta/bazel-gazelle/v3/language/proto"
)

var languages = []v3language.Language{
	proto.NewLanguage(),
	golang.NewLanguage(),
}
