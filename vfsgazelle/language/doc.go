// Package language defines the vfsgazelle plugin interfaces.
//
// The API mirrors classic Gazelle at a high level, but replaces direct
// filesystem access with VFS-backed package and file handles. Languages can
// register parsers up front, consume cached semantic models from the frozen
// snapshot, and optionally access the snapshot during Configure.
//
// Parser registration is the normal way for a vfsgazelle language to move
// repeated parsing work into the framework cache. Each parser declares a
// manual CacheVersion, and vfsgazelle uses that version to reject stale
// persisted parser caches before they are loaded on rerun.
package language
