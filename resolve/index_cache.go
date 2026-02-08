/* Copyright 2026 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resolve

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const cacheFormatVersion = 1

// indexCache is the on-disk representation of the rule index cache.
type indexCache struct {
	FormatVersion int           `json:"formatVersion"`
	Fingerprint   string        `json:"fingerprint"`
	Records       []*ruleRecord `json:"records"`
}

// SaveCache serializes the current rule records to a JSON file at path.
// The fingerprint is embedded in the file so that LoadCache can detect
// stale caches (e.g., after a binary rebuild). Writes are atomic: data
// is written to a temporary file first and then renamed.
func (ix *RuleIndex) SaveCache(path, fingerprint string) error {
	c := indexCache{
		FormatVersion: cacheFormatVersion,
		Fingerprint:   fingerprint,
		Records:       ix.rules,
	}
	data, err := json.Marshal(&c)
	if err != nil {
		return fmt.Errorf("marshaling index cache: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gazelle-index-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for index cache: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing index cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing index cache temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming index cache: %w", err)
	}
	return nil
}

// LoadCache reads a previously saved index cache from path and appends
// the cached records to ix.rules. LoadCache must be called before Finish.
//
// If the file does not exist or the fingerprint does not match, LoadCache
// silently returns nil (the index simply starts empty). A corrupt or
// unreadable file returns an error.
func (ix *RuleIndex) LoadCache(path, fingerprint string) error {
	if ix.indexed {
		return fmt.Errorf("LoadCache called after Finish")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading index cache: %w", err)
	}

	var c indexCache
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("unmarshaling index cache: %w", err)
	}

	if c.FormatVersion != cacheFormatVersion {
		return nil
	}
	if c.Fingerprint != fingerprint {
		return nil
	}

	ix.rules = append(ix.rules, c.Records...)
	return nil
}

// InvalidatePackage removes all cached records whose Pkg matches pkg.
// This should be called for every directory visited during the walk,
// before fresh records are added via AddRule.
func (ix *RuleIndex) InvalidatePackage(pkg string) {
	n := 0
	for _, r := range ix.rules {
		if r.Pkg != pkg {
			ix.rules[n] = r
			n++
		}
	}
	// Clear trailing pointers to allow GC.
	for i := n; i < len(ix.rules); i++ {
		ix.rules[i] = nil
	}
	ix.rules = ix.rules[:n]
}

// BinaryFingerprint computes a SHA-256 hex digest of the file at
// executablePath. Since Go plugins are statically linked, the binary
// changes whenever any plugin or Gazelle itself is recompiled.
func BinaryFingerprint(executablePath string) (string, error) {
	f, err := os.Open(executablePath)
	if err != nil {
		return "", fmt.Errorf("opening binary for fingerprint: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing binary for fingerprint: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
