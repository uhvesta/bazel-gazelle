// Package run executes the vfsgazelle pipeline.
//
// A run builds or patches a VFS snapshot, primes parser-backed semantic models,
// freezes the snapshot, walks the repo in Gazelle order, builds a global rule
// index, resolves dependencies, and emits updated BUILD files.
//
// The runner is intentionally whole-repo oriented. Incremental behavior comes
// from snapshot patching and parser-cache reuse, not from partial package-level
// execution of the algorithm. Real rerun speedups come from avoiding full
// filesystem reconstruction and reusing parser-backed semantic models, while
// keeping the Gazelle walk/generate/index/resolve model intact.
package run
