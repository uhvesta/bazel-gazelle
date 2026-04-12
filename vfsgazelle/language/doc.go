// Package language defines the vfsgazelle plugin interfaces.
//
// The API mirrors classic Gazelle at a high level, but replaces direct
// filesystem access with VFS-backed package and file handles. Languages can
// register parsers up front, consume cached semantic models from the frozen
// snapshot, and optionally access the snapshot during Configure.
package language
