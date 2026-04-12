# Gazelle

Gazelle generates and updates Bazel `BUILD.bazel` files. The traditional Gazelle pipeline and the new `v3` pipeline both preserve the same high-level rule-generation model:

1. discover packages and configuration
2. generate/update rules for each package
3. build a repo-wide index of importable rules
4. resolve dependencies against that index
5. write updated BUILD files

`v3` is intended to match normal Gazelle behavior as closely as possible. The main difference is the execution substrate: `v3` builds and reuses a snapshot-backed VFS, but it still tries to preserve Gazelle's walk, generate, index, resolve, and emit semantics wherever that is practical.

The older, general project README was moved to [docs/README.md](docs/README.md). This root README is now focused on how Gazelle works, how `v3` differs, and how to use incremental reruns safely.

**Contents**

- [How Gazelle Works](#how-gazelle-works)
- [How V3 Works](#how-v3-works)
- [Incremental Reruns](#incremental-reruns)
- [Watchman Setup](#watchman-setup)
- [Why V3 Exists](#why-v3-exists)
- [Reference](#reference)

## How Gazelle Works

At a high level, classic Gazelle walks the repository in a depth-first manner, propagating configuration from parent directories to children, then generating package-level rules and finally resolving dependencies across the whole repo.

The important ordering is:

1. read repo-level configuration like `WORKSPACE`, `REPO.bazel`, `.bazelignore`, directives, and language flags
2. walk directories depth-first
3. for each directory:
   - load that directory's `BUILD` file if one exists
   - apply `Configure` logic for all languages/configurers
   - recurse into child directories
   - run package-level rule generation/update
4. after rule generation across the repo, build the global rule index
5. run `Resolve` against that full index
6. fix loads and write files

The walk is easier to understand with a concrete example. Suppose the repo looks like this:

```text
.
├── BUILD.bazel
├── a
│   ├── BUILD.bazel
│   └── x.go
└── b
    ├── BUILD.bazel
    └── y.go
```

Gazelle conceptually does:

1. load `//:BUILD.bazel`
2. run root `Configure`
3. recurse into `//a`
4. load `//a:BUILD.bazel`
5. run `Configure` for `a`
6. generate/update package `a`
7. recurse back
8. recurse into `//b`
9. load `//b:BUILD.bazel`
10. run `Configure` for `b`
11. generate/update package `b`
12. finish the walk
13. build the rule index
14. resolve imports for generated rules in `a` and `b`
15. write files

The language hooks line up with that ordering:

- `Configure`: parent-before-child, while walking
- `GenerateRules`: package-local, during the walk
- `Imports`: used for index construction after generation
- `Resolve`: after the whole index exists
- `FixLoads`: just before emit

That model is simple and deterministic, but it mixes filesystem IO and semantic work together during the run.

## How V3 Works

`v3` keeps the same logical algorithm, but changes how inputs are prepared.

Instead of having languages read directly from the OS while Gazelle is walking, `v3` does this:

1. build a repo snapshot / VFS
2. parse and cache file models in that VFS
3. freeze the snapshot into an immutable read-only view
4. run the normal Gazelle-style walk/generate/index/resolve algorithm against the frozen snapshot

So the big shift is:

- classic Gazelle: IO and rule generation are interleaved
- `v3`: IO is front-loaded into a snapshot, then the algorithm runs in memory

The goal is not to invent a different rule-generation model. The goal is to keep Gazelle's behavior as intact as possible while changing how filesystem state is prepared, cached, and reused.

The current `v3` ordering is:

1. build VFS
2. prime parser cache
3. freeze snapshot
4. DFS walk with config propagation
5. generate rules
6. build rule index
7. resolve
8. emit

That lets languages use:

- `Repo.ReadFile(...)`
- `Repo.GetModel(path, parserKey)`
- `PackageDir.RegularFiles()`
- `PackageDir.Subdirs()`

instead of direct `os.ReadFile` style access.

### What The VFS Stores

The `v3` VFS is a repo snapshot plus a parsed-model cache.

At a high level it stores:

- directory membership
- per-file path and content hash
- raw file content when the runner still needs direct bytes
- parsed models keyed by `(path, parser key, parser version, content hash)`

The important design choice is that the semantic cache is the main cache. For parser-backed files like `.go`, `.proto`, and `go.mod`, the useful thing is usually the parsed model, not the raw bytes.

The snapshot has two phases:

1. mutable build phase
   - one coordinator owns the VFS maps and cache builder
   - worker goroutines can read files and parse models
   - workers send results back to the coordinator
2. frozen run phase
   - the snapshot is immutable
   - walk, generate, imports, and resolve read from that frozen state

That split is why `v3` can stay deterministic without putting mutexes all over the read path.

### Full Build vs Rerun

`run` builds the snapshot from scratch:

1. enumerate the repo tree
2. read file content
3. compute content hashes
4. prime parser-backed models
5. freeze the snapshot
6. run Gazelle against the frozen view

`rerun` starts from the previously saved frozen snapshot:

1. load the saved snapshot and parsed-model cache
2. normalize the changed path list
3. filter out changed paths that should not affect the walk
4. patch only the changed/new/deleted files or directories
5. reuse cached parsed models for unchanged content
6. freeze the updated snapshot
7. rerun the whole Gazelle algorithm

So `rerun` is still a whole-repo Gazelle run, but it is not a whole-repo filesystem rebuild.

### Dirtying And Rebuild Rules

`v3` currently uses a simple, explicit invalidation model.

- ordinary file edits
  - patch just those files
  - if the content hash is unchanged, the patch is ignored
- deleted files
  - remove the file from the snapshot and cache
- newly created files
  - add the file to the snapshot and parse any matching models
- newly created directories
  - rescan that subtree into the snapshot

Before patching, `rerun` filters the incoming changed path list against the previous frozen snapshot and the effective walk rules. That means changes under these are ignored when they should be ignored:

- `.bazelignore`
- `REPO.bazel` `ignore_directories(...)`
- `# gazelle:exclude`
- `# gazelle:ignore`
- `# gazelle:directive_file`

There is one important special case for BUILD files:

- if a `BUILD` or `BUILD.bazel` file changes its `exclude` or `ignore` directives, that package subtree is rebuilt instead of doing a single-file patch
- if that happens at the repo root package, `v3` falls back to a full VFS rebuild

That rule exists because those directives change which descendants should even exist in the logical walk, so a local file patch is not enough.

### Persistence

After a successful run, `v3` writes snapshot state to the OS cache directory.

That persisted state is intentionally compact:

- it always keeps enough information to reconstruct the directory tree
- it stores file hashes for change detection
- it stores parsed models for parser-backed files
- it only keeps raw file content when direct bytes are still needed, especially for Bazel control files like:
  - `BUILD`
  - `BUILD.bazel`
  - `WORKSPACE`
  - `WORKSPACE.bazel`
  - `REPO.bazel`
  - `.bazelignore`

### External Metadata And Lockfiles

`v3` is designed to support language-specific external metadata without relying on generic remote discovery.

A common example is a lockfile or metadata file that maps language-level symbols to external dependencies, for example:

- a module lockfile
- a package manifest lockfile
- a Rust crate metadata file
- a language-specific dependency manifest that maps import paths or symbols to external Bazel labels

The intended `v3` pattern is:

1. register a parser for that metadata file
2. parse it into a cached semantic model in the VFS
3. load that model through `Repo.GetModel(...)`
4. use it during `Configure`, `GenerateRules`, `Imports`, or `Resolve`

In practice that means external dependency resolution can be:

- language-specific
- deterministic
- snapshot-backed
- incremental

instead of relying on a shared “remote cache” abstraction or on-demand subprocess/network discovery.

For example, if a language has a metadata file, a plugin can:

1. parse that metadata file into:
   - package or symbol -> external dependency identifier
   - external dependency identifier -> Bazel label
2. cache that parsed model in the VFS
3. during `Resolve`, check:
   - in-repo `RuleIndex` first
   - lockfile-derived external index second

That lets `v3` support external resolution as a local semantic data problem instead of a remote lookup problem.

## Incremental Reruns

`v3` now supports two CLI modes:

```text
gazelle-v3 run
gazelle-v3 rerun <changed paths...>
```

Useful flags:

- `-timings`
  - print per-phase timing information to stderr
  - on `rerun`, this includes `read_vfs_from_cache` and `patch_vfs`
- `-state_format`
  - choose `gob` or `json` for the persisted `v3` snapshot state
  - `gob` is the default

`run` does a full snapshot build and saves state in the OS cache directory.

`rerun` does this:

1. load the saved frozen snapshot
2. patch only the changed/new/deleted paths
3. reuse cached parsed models for unchanged files
4. rerun the whole Gazelle algorithm against the patched snapshot
5. save the updated snapshot back to disk

This means `rerun` is already useful even without a built-in watcher.

### No-op Protection

Two safeguards are important for stable incremental use:

1. unchanged patched files are ignored
   - if a changed path is passed to `rerun` but the file content hash is unchanged, the patch is a no-op
   - if all incoming changes are no-ops, `v3` skips the full walk/generate/resolve algorithm entirely

2. unchanged BUILD output is not rewritten
   - before writing a `BUILD.bazel` file, `v3` compares the formatted output to the existing file bytes
   - if the content is identical, it does not rewrite the file

Those two behaviors are what make external watch tooling practical without falling into self-triggered loops.

## Watchman Setup

There is no built-in Watchman daemon in `v3` yet. The intended model is:

- let Watchman or another tool detect changed files
- pass the changed repo-relative paths to `gazelle-v3 rerun`

The important part is that the watcher batches and coalesces file changes before invoking Gazelle.

A practical shape is:

1. do one initial cold run

```sh
bazel run //v3/cmd/gazelle -- run
```

2. configure Watchman to invoke a small wrapper script
3. the wrapper script passes the changed file list to:

```sh
bazel run //v3/cmd/gazelle -- rerun path/to/file1.go path/to/file2.proto
```

Recommended filters:

- ignore `.git`
- ignore `bazel-*`
- ignore editor temp files
- ignore generated files you know Gazelle should not inspect

Because `rerun` skips unchanged content and emit skips identical BUILD rewrites, a Watchman-driven loop can be stable without first building watcher logic into Gazelle itself.

A concrete Watchman example looks like this:

```sh
watchman watch ~/src/myrepo
watchman -- trigger ~/src/myrepo gazelle-v3 \
  '**/*.go' '**/*.proto' '**/BUILD' '**/BUILD.bazel' \
  '**/WORKSPACE' '**/WORKSPACE.bazel' '**/REPO.bazel' '.bazelignore' \
  -- ./tools/run-gazelle-v3-rerun.sh
```

In practice most teams should use a small wrapper script instead of inlining everything in one command:

1. collect the changed repo-relative paths from Watchman
2. drop paths under `.git`, `bazel-*`, editor temp files, and other known junk
3. invoke:

```sh
bazel run //v3/cmd/gazelle -- rerun <paths...>
```

These examples may not work as-is in every environment. Check the Watchman syntax and trigger model here:

- https://facebook.github.io/watchman/

Or build your own file system service that collects changed repo-relative paths and invokes:

```sh
bazel run //v3/cmd/gazelle -- rerun <paths...>
```

The important point is not the exact Watchman syntax. The important point is that `v3` expects a coalesced list of changed repo-relative paths, and it is safe for that list to include some noise because the rerun path does hash checks and no-op filtering.

## Why V3 Exists

`v3` is not about changing the semantic rule-generation model. It is about changing the execution model.

Benefits:

- parser results are cached instead of recomputed every run
- repo IO is centralized behind a VFS
- the run phase works against immutable in-memory state
- incremental reruns can patch a prior snapshot instead of rebuilding everything
- no-op reruns can short-circuit almost completely

This is different from classic lazy indexing.

### V3 vs Lazy Indexing

Lazy indexing optimizes one part of the classic algorithm:

- it avoids indexing the whole repo eagerly
- it is still fundamentally tied to the classic filesystem-driven traversal model

`v3` is broader:

- it snapshots the repo
- it caches parsed models
- it supports saved state across process restarts
- it supports path-based patching for reruns

Lazy indexing also depends on reasonably strong path conventions. In practice, it works best when import paths, package roots, and directory layout line up in a predictable way. Some monorepos and mixed-language codebases do not have those conventions, which makes lazy indexing less effective or harder to configure correctly.

So lazy indexing is mainly an indexing optimization inside the classic design, while `v3` is a different execution substrate for the whole algorithm and does not depend as heavily on those path-structure assumptions.

### Why V3 Can Be Faster

For a cold run on a repo, `v3` still pays snapshot cost. The real performance payoff comes from reruns:

- unchanged files are not reparsed
- unchanged snapshots can skip the whole algorithm
- changed-file reruns avoid rebuilding the whole VFS

That is why `v3` is most interesting for:

- editor-triggered reruns
- watcher-driven workflows
- repeated local development runs

## Reference

- Old project README: [docs/README.md](docs/README.md)
- Extension guide: [extend.md](extend.md)
- Rule reference: [reference.md](reference.md)
- Command/config reference: [gazelle-reference.md](gazelle-reference.md)
- How classic Gazelle works: [how-gazelle-works.md](how-gazelle-works.md)
