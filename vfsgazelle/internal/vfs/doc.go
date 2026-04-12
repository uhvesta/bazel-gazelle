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
//
// Persisted state is split into:
//   - one metadata file for the directory tree, file hashes, and selected raw content
//   - one parser-cache file per parser key
//
// On rerun, metadata blocks startup, while parser caches are attached
// independently and only block when a caller actually accesses a model for that
// parser through GetModel.
package vfs
