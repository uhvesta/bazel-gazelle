// Package vfs implements the snapshot-backed filesystem used by vfsgazelle.
//
// The package is built around two phases:
//   - a mutable build phase, owned by a single coordinator goroutine
//   - a frozen read-only phase used by walk, generation, indexing, and resolve
//
// During the build phase, worker goroutines may read files and parse models,
// but the coordinator alone mutates snapshot membership and the parsed-model
// cache. Once the snapshot is frozen, callers can safely share it without
// additional synchronization.
//
// The VFS stores both raw file metadata and parser-backed semantic models. For
// parser-backed files, the semantic model is the primary cache artifact. Cache
// entries are invalidated by file content hash and by each parser's declared
// CacheVersion.
package vfs
