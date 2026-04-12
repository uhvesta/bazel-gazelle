// Package walk applies Gazelle-style traversal and configuration propagation to
// a frozen vfsgazelle snapshot.
//
// The walk preserves the classic parent-before-child Configure ordering and
// visits packages in depth-first post-order so that languages can reuse the
// same high-level generation model while reading from a VFS instead of the OS.
package walk
