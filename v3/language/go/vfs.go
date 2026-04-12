package golang

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"path/filepath"

	"github.com/uhvesta/bazel-gazelle/v3/internal/vfs"
)

var activeRepo *vfs.Snapshot

func withRepo(repo *vfs.Snapshot, fn func()) {
	prev := activeRepo
	activeRepo = repo
	defer func() { activeRepo = prev }()
	fn()
}

func repoRelFromAbs(path string) (string, bool) {
	if activeRepo == nil {
		return "", false
	}
	rel, err := filepath.Rel(activeRepo.Root, path)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func readFile(path string) ([]byte, error) {
	if rel, ok := repoRelFromAbs(path); ok {
		return activeRepo.ReadFile(rel)
	}
	return nil, fmt.Errorf("no active v3 repo snapshot for %s", path)
}

func openReader(path string) (io.ReadCloser, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func repoFileExists(rel string) bool {
	if activeRepo == nil {
		return false
	}
	_, ok := activeRepo.File(path.Clean(rel))
	return ok
}

func repoListDir(rel string) ([]string, error) {
	if activeRepo == nil {
		return nil, fmt.Errorf("no active v3 repo snapshot for dir %s", rel)
	}
	return activeRepo.ListDir(path.Clean(rel))
}
