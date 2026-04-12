// Package walk applies Gazelle-style traversal and configuration propagation to
// a frozen vfsgazelle snapshot.
//
// The walk preserves the classic parent-before-child Configure ordering and
// visits packages in depth-first post-order so that languages can reuse the
// same high-level generation model while reading from a VFS instead of the OS.
//
// The walk package also owns traversal-sensitive invalidation rules for rerun,
// such as subtree rebuilds for BUILD-file exclude/ignore changes and full
// rebuilds when repo-level ignore sources like .bazelignore or REPO.bazel
// change their effective ignored sets.
package walk
