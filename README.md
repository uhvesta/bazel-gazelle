# Gazelle

Gazelle generates and updates Bazel `BUILD.bazel` files. The traditional Gazelle pipeline and the new `vfsgazelle` pipeline both preserve the same high-level rule-generation model:

1. discover packages and configuration
2. generate/update rules for each package
3. build a repo-wide index of importable rules
4. resolve dependencies against that index
5. write updated BUILD files

`vfsgazelle` is intended to match normal Gazelle behavior as closely as possible. The main difference is the execution substrate: `vfsgazelle` builds and reuses a snapshot-backed VFS, but it still tries to preserve Gazelle's walk, generate, index, resolve, and emit semantics wherever that is practical.

The older, general project README was moved to [docs/README.md](docs/README.md). This root README is now focused on how Gazelle works, how `vfsgazelle` differs, and how to use incremental reruns safely.

**Contents**

- [How Gazelle Works](#how-gazelle-works)
- [How vfsgazelle Works](#how-vfsgazelle-works)
- [Why Plugins Should Usually Stay Single-Threaded](#why-plugins-should-usually-stay-single-threaded)
- [Plugin Porting](#plugin-porting)
- [Incremental Reruns](#incremental-reruns)
- [Watchman Setup](#watchman-setup)
- [Why vfsgazelle Exists](#why-vfsgazelle-exists)
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

## How vfsgazelle Works

`vfsgazelle` keeps the same logical algorithm, but changes how inputs are prepared.

Instead of having languages read directly from the OS while Gazelle is walking, `vfsgazelle` does this:

1. build a repo snapshot / VFS
2. parse and cache file models in that VFS
3. freeze the snapshot into an immutable read-only view
4. run the normal Gazelle-style walk/generate/index/resolve algorithm against the frozen snapshot

So the big shift is:

- classic Gazelle: IO and rule generation are interleaved
- `vfsgazelle`: IO is front-loaded into a snapshot, then the algorithm runs in memory

The goal is not to invent a different rule-generation model. The goal is to keep Gazelle's behavior as intact as possible while changing how filesystem state is prepared, cached, and reused.

The current `vfsgazelle` ordering is:

1. build VFS
2. prime parser cache
3. freeze snapshot
4. DFS walk with config propagation
5. generate package results
6. build rule index
7. resolve
8. emit

That lets languages use:

- `Repo.ReadFile(...)`
- `Repo.GetModel(path, parserKey)`
- `PackageDir.RegularFiles()`
- `PackageDir.Subdirs()`

instead of direct `os.ReadFile` style access.

### Why Plugins Should Usually Stay Single-Threaded

TL;DR: `vfsgazelle` already applies concurrency where it materially improves wall-clock time: VFS file IO, parser priming, background parser-cache loading, and rerun short-circuiting. Plugins are meant to be small deterministic interjection points inside that pipeline, not their own schedulers. In practice, plugin-managed goroutines usually duplicate framework work, add contention, and make runs slower or harder to reason about.

`vfsgazelle` already applies most of the useful concurrency and IO optimization in the framework itself, before plugin logic becomes the bottleneck.

There is one important framework-owned concurrency layer today:

1. VFS build concurrency
   - worker goroutines read files and parse models
   - one coordinator owns snapshot membership and cache mutation

There are also a couple of important IO and caching optimizations around that:

- parser-backed files are turned into cached semantic models up front through `Repo.GetModel(...)`
- frozen snapshots are reused across `rerun`
- persisted snapshot metadata stores only the directory tree plus the small set of control files whose bytes must survive reload
- snapshot metadata loads synchronously at startup, while parser cache loads are started immediately in the background per parser key
- `Repo.GetModel(path, parserKey)` does not block on unrelated parser caches; it only waits if that specific parser cache is needed and has not finished loading yet
- parser-backed file changes are reparsed and a rerun is avoided in incremental/rerun mode if the model is semantically unchanged.
- control-file changes are ignored if their content hash is unchanged (i.e. if a BUILD.bazel is unchanged content wise then rerun is essentially no-op)
- if all incoming rerun changes are semantic no-ops, `vfsgazelle` skips the full walk / generate / resolve pipeline entirely
- emitted `BUILD` files are only rewritten if the formatted bytes actually changed

So the execution contract is intentionally asymmetric:

- parent-before-child configuration remains deterministic
- package-local generation currently stays single-threaded in the framework
- global indexing and resolution stay framework-owned

For plugin authors, the practical implication is that extra goroutines are usually the wrong optimization:

- they compete with framework-owned file IO and parser work instead of replacing it
- they add scheduling and synchronization overhead in code that is often already memory-resident by the time plugins run
- they encourage shared mutable state on language objects, which is harder to reason about and easier to get wrong
- they often duplicate work that should instead be moved into parser registration and `Repo.GetModel(...)`
- they can make profiling noisier, which makes it harder to tell whether time is really going to parsing, generation, resolution, or emit

In most plugins, the better optimization path is:

- move repeated parsing into a registered parser
- read from `Repo.GetModel(...)` instead of reparsing bytes in plugin code
- use `PackageDir.RegularFiles()` / `Subdirs()` instead of rescanning the filesystem
- keep `Fix`, `GenerateRules`, and `Resolve` mostly stateless
- let snapshot reuse, parser-cache reuse, and rerun no-op filtering eliminate unnecessary work

So plugin authors should usually assume this:

- do not assume `Fix`, `GenerateRules`, or `Resolve` are framework-parallelized today
- avoid mutable shared state on the language object unless you synchronize it yourself
- strongly prefer stateless logic plus VFS/parser-cache APIs over plugin-managed worker pools

In particular, plugin authors should generally not start their own goroutines for routine file or parser work. `vfsgazelle` already parallelizes the expensive parts of VFS file IO and parser priming, then uses cached semantic models and no-op rerun detection to avoid repeating work on later runs. For most plugins, a simple single-threaded implementation that leans on those APIs will be both faster and easier to maintain than a plugin-managed concurrent design.

### What The VFS Stores

The `vfsgazelle` VFS is a repo snapshot plus a parsed-model cache.

At a high level it stores:

- directory membership
- file presence through the snapshot tree
- raw file content only for control files that must survive snapshot reload
- parsed models keyed by `(path, parser key, parser cache version, content hash)`
- a parser-version manifest used to reject stale persisted parser caches up front

The important design choice is that the semantic cache is the main cache. For parser-backed files like `.go`, `.proto`, and `go.mod`, the useful thing is usually the parsed model, not the raw bytes.

Each parser also declares a manual cache version string. Parser authors are expected to bump that version whenever old cached parser results should be invalidated.

The snapshot has two phases:

1. mutable build phase
   - one coordinator owns the VFS maps and cache builder
   - worker goroutines can read files and parse models
   - workers send results back to the coordinator
2. frozen run phase
   - the snapshot is immutable
   - walk, generate, imports, and resolve read from that frozen state

That split is why `vfsgazelle` can stay deterministic without putting mutexes all over the read path.

Metadata and parser caches are also loaded differently on `rerun`:

- snapshot metadata blocks startup
- parser cache loads are started immediately in the background, one future per parser key
- `Repo.GetModel(path, parserKey)` only waits if that specific parser cache is still loading

That behavior is intentionally transparent to plugins. A plugin still just calls normal synchronous VFS methods.

### Full Build vs Rerun

`run` builds the snapshot from scratch:

1. enumerate the repo tree
2. read file content
3. compute content hashes
4. prime parser-backed models
5. freeze the snapshot
6. run Gazelle against the frozen view

`rerun` starts from the previously saved frozen snapshot:

1. load the saved snapshot metadata
2. start parser-cache loads in the background
3. normalize and filter the changed path list
4. patch only the changed/new/deleted files or directories
5. if patching is a semantic no-op, return the previous frozen snapshot
6. otherwise prime parsers for changed work, freeze the updated snapshot, and rerun the whole Gazelle algorithm

So `rerun` is still a whole-repo Gazelle run, but it is not a whole-repo filesystem rebuild.

### Dirtying And Rebuild Rules

`vfsgazelle` currently uses a simple, explicit invalidation model.

- ordinary file edits
  - patch just those files
  - parser-backed files are reparsed and ignored if their semantic model is unchanged
  - control files are ignored if their content hash is unchanged
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
- if that happens at the repo root package, `vfsgazelle` falls back to a full VFS rebuild

That rule exists because those directives change which descendants should even exist in the logical walk, so a local file patch is not enough.

Repo-level traversal policy is treated the same way:

- if `.bazelignore` changes the effective ignored path set, `vfsgazelle` falls back to a full VFS rebuild
- if `REPO.bazel` changes the effective `ignore_directories(...)` set, `vfsgazelle` falls back to a full VFS rebuild

### Persistence

After a successful run, `vfsgazelle` writes snapshot state to the OS cache directory.

That persisted state is intentionally compact:

- it always keeps enough information to reconstruct the directory tree
- it stores parsed models for parser-backed files in per-parser cache files
- it only keeps explicit file metadata for control files whose bytes must survive reload, especially:
  - `BUILD`
  - `BUILD.bazel`
  - `MODULE.bazel`
  - `WORKSPACE`
  - `WORKSPACE.bazel`
  - `REPO.bazel`
  - `.bazelignore`
- ordinary non-control files and parser-backed files are reconstructed from the saved directory tree instead of per-file metadata rows

The persisted layout is:

- one metadata file
- one parser cache file per parser key that actually has persisted entries

For example:

- `<hash>.meta.gob`
- `<hash>.cache.go-fileinfo.gob`
- `<hash>.cache.go-modfile.gob`
- `<hash>.cache.proto-fileinfo.gob`

The metadata file records the active parser cache versions. On `rerun`, if a parser's current `CacheVersion()` does not match the saved manifest entry, that parser cache file is rejected before load and that parser starts cold.

### External Metadata And Lockfiles

`vfsgazelle` is designed to support language-specific external metadata without relying on generic remote discovery.

A common example is a lockfile or metadata file that maps language-level symbols to external dependencies, for example:

- a module lockfile
- a package manifest lockfile
- a Rust crate metadata file
- a language-specific dependency manifest that maps import paths or symbols to external Bazel labels

The intended `vfsgazelle` pattern is:

1. register a parser for that metadata file
2. parse it into a cached semantic model in the VFS
3. load that model through `Repo.GetModel(...)`
4. use it during `Configure`, `GenerateRules`, `Imports`, or `Resolve`

If the parser changes in a way that makes old cached entries unsafe, the parser should bump its cache version so those entries are ignored.

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

That lets `vfsgazelle` support external resolution as a local semantic data problem instead of a remote lookup problem.

## Bazel Rule Support

`vfsgazelle` now has a first-class Bazel rule surface for both binary composition and snapshot testing.

Preferred public rules:

- `vfsgazelle_binary`
- `vfsgazelle_generation_test`

Compatibility aliases still exported from `//:def.bzl`:

- `gazelle_vfsgazelle_binary`
- `gazelle_vfsgazelle_generation_test`

### Composing a Custom vfsgazelle Binary

Use `vfsgazelle_binary` the same way classic Gazelle users compose `gazelle_binary`: provide an ordered list of vfsgazelle language libraries exporting `NewLanguage()`.

```starlark
load("@bazel_gazelle//:def.bzl", "DEFAULT_VFSGAZELLE_LANGUAGES", "vfsgazelle_binary")

vfsgazelle_binary(
    name = "my_vfsgazelle",
    languages = DEFAULT_VFSGAZELLE_LANGUAGES + [
        "//tools/vfsgazelle/foo",
    ],
)
```

Language ordering still matters. If one plugin depends on metadata generated by another, keep the producer earlier in the list.

### Composing Snapshot Tests

Use `vfsgazelle_generation_test` to run a `vfsgazelle` binary against golden test workspaces.

```starlark
load("@bazel_gazelle//:def.bzl", "vfsgazelle_binary", "vfsgazelle_generation_test")

vfsgazelle_binary(
    name = "my_vfsgazelle",
    languages = [
        "//vfsgazelle/language/proto",
        "//vfsgazelle/language/go",
        "//tools/vfsgazelle/foo",
    ],
)

vfsgazelle_generation_test(
    name = "my_vfsgazelle_snapshot_test",
    gazelle_binary = ":my_vfsgazelle",
    test_data = glob(["testdata/vfsgazelle/**"]),
)
```

The fixture layout matches `gazelle_generation_test`:

- `BUILD.in` is copied to `BUILD.bazel` before the run
- `BUILD.out` is the expected generated file
- `arguments.txt` contains CLI flags passed after the implicit `run` command
- `expectedStdout.txt`, `expectedStderr.txt`, and `expectedExitCode.txt` are optional

`vfsgazelle_generation_test` intentionally runs `vfsgazelle run ...`; it does not add an `update` subcommand.

If you want snapshot tests to use a deterministic persisted-state location, add
`-state_dir` to `arguments.txt`, for example:

```text
-state_dir
.vfsgazelle-cache
```

Relative paths are resolved from the repo root of the test workspace.

## Plugin Porting

At a high level, a normal Gazelle plugin and a `vfsgazelle` plugin are trying to do the same job:

- interpret directives and config
- inspect package-local files
- generate rules
- describe imports for indexing
- resolve dependencies against the repo-wide index

The main difference is where filesystem state comes from.

### Classic Gazelle Plugin Shape

Classic Gazelle plugins usually work with:

- `Configure(c, rel, f)`
- `GenerateRules(args)`
- `Imports(args)`
- `Resolve(args)`

and `GenerateRules` receives package-local slices like:

- `Subdirs []string`
- `RegularFiles []string`
- `GenFiles []string`

Classic plugins often do extra IO with direct filesystem calls like:

- `os.ReadFile`
- `os.Stat`
- `os.ReadDir`

or helper APIs layered on top of the real filesystem.

### vfsgazelle Plugin Shape

`vfsgazelle` keeps the same broad lifecycle, but the interfaces are VFS-aware.

Important differences:

- `GenerateRules` receives:
  - `Repo *vfs.Snapshot`
  - `PackageDir *vfs.Dir`
  - `GenFiles []string`
- configurers can implement:
  - `ConfigureRepo(c, repo, rel, f)`
    instead of only `Configure(c, rel, f)`
- plugins can register parsers up front and consume cached models through:
  - `Repo.GetModel(path, parserKey)`

For most non-trivial `vfsgazelle` ports, parser registration is not optional in practice. It is the normal way to move repeated parsing work into the VFS cache.

So instead of relying on direct OS IO, a `vfsgazelle` plugin is expected to use:

- `Repo.ReadFile(...)`
- `Repo.ListDir(...)`
- `Repo.File(...)`
- `Repo.GetModel(...)`
- `PackageDir.RegularFiles()`
- `PackageDir.Subdirs()`

### Practical Porting Strategy

The easiest way to port an existing plugin is usually:

1. copy the existing plugin into a `vfsgazelle` package
2. keep the high-level generation and resolve logic intact
3. replace direct file IO with VFS calls
4. add parser registration for expensive or repeated parsing work
5. use `ConfigureRepo` where config-time repo inspection is needed

In practice, the most common code changes are:

- replace raw package file lists with `PackageDir.RegularFiles()`
- replace path concatenation plus `os.ReadFile` with `Repo.ReadFile`
- replace repeated parsing helpers with `Repo.GetModel`
- replace config-time repo probing with snapshot queries in `ConfigureRepo`
- add a parser with a manual `CacheVersion()` string and bump it when parser semantics change

### Concrete Example

Here is a simplified example of a classic Gazelle plugin that reads `.foo`
files directly from disk and generates one rule per file.

Classic shape:

```go
type fooLang struct{}

func (*fooLang) Name() string { return "foo" }

func (*fooLang) KnownDirectives() []string { return []string{"foo_mode"} }

func (*fooLang) Configure(c *config.Config, rel string, f *rule.File) {
    // Read directives from f and update c.Exts as needed.
}

func (*fooLang) GenerateRules(args language.GenerateArgs) language.GenerateResult {
    var gen []*rule.Rule
    var imports []interface{}

    for _, name := range args.RegularFiles {
        if !strings.HasSuffix(name, ".foo") {
            continue
        }
        path := filepath.Join(args.Dir, name)
        data, err := os.ReadFile(path)
        if err != nil {
            continue
        }

        model := parseFoo(data)
        r := rule.NewRule("foo_library", strings.TrimSuffix(name, ".foo"))
        r.SetAttr("srcs", []string{name})
        r.SetAttr("deps", model.Deps)

        gen = append(gen, r)
        imports = append(imports, model.Imports)
    }

    return language.GenerateResult{
        Gen:     gen,
        Imports: imports,
    }
}

func (*fooLang) Imports(args language.ImportsArgs) []resolve.ImportSpec {
    return nil
}

func (*fooLang) Resolve(args language.ResolveArgs) {}
```

The equivalent `vfsgazelle` plugin keeps the same broad logic, but file access moves
through the snapshot and the parser is registered up front.

`vfsgazelle` shape:

```go
type fooLang struct{}

func (*fooLang) Name() string { return "foo" }

func (*fooLang) KnownDirectives() []string { return []string{"foo_mode"} }

func (*fooLang) RegisterParsers(reg *vfs.Registry) error {
    return reg.Register(fooParser{}, vfs.MatchExtension(".foo"))
}

func (fooParser) CacheVersion() string { return "v1" }

func (*fooLang) ConfigureRepo(c *config.Config, repo *vfs.Snapshot, rel string, f *rule.File) {
    // Read directives from f and update c.Exts as needed.
}

func (*fooLang) GenerateRules(args vlang.GenerateArgs) vlang.GenerateResult {
    var gen []*rule.Rule
    var imports []interface{}

    for _, file := range args.PackageDir.RegularFiles() {
        if !strings.HasSuffix(file.Name(), ".foo") {
            continue
        }

        result, err := file.GetModel("foo/file")
        if err != nil {
            continue
        }
        model := result.Model.(fooModel)

        r := rule.NewRule("foo_library", strings.TrimSuffix(file.Name(), ".foo"))
        r.SetAttr("srcs", []string{file.Name()})
        r.SetAttr("deps", model.Deps)

        gen = append(gen, r)
        imports = append(imports, model.Imports)
    }

    return vlang.GenerateResult{
        Gen:     gen,
        Imports: imports,
    }
}

func (*fooLang) Imports(args vlang.ImportsArgs) []resolve.ImportSpec {
    return nil
}

func (*fooLang) Resolve(args vlang.ResolveArgs) {}
```

The semantic change is small:

- classic plugin:
  - loops over `RegularFiles []string`
  - reads file bytes directly with `os.ReadFile`
- `vfsgazelle` plugin:
  - loops over `PackageDir.RegularFiles()`
  - reads parsed state through `GetModel(...)`

That is the most common migration pattern: keep the rule logic, replace the filesystem plumbing, and register parsers for the file types the plugin repeatedly inspects.

### File IO Differences

Classic plugins often mix semantic logic with filesystem calls.

`vfsgazelle` tries to separate those concerns:

- the VFS build phase does repo IO up front
- parser registration turns expensive parsing into cached semantic models
- the run phase reads from the frozen snapshot

That means a good `vfsgazelle` plugin usually:

- reads repo-local files through the VFS
- parses structured files through registered parsers
- avoids direct `os.*` access for repo-local data

Direct OS access in a `vfsgazelle` plugin should generally only remain for things that are not workspace snapshot data, such as:

- environment-dependent behavior
- user cache dir selection
- external tools or subprocesses
- other data outside the repo snapshot

### Resolve Differences

The resolve model is intentionally close to classic Gazelle.

In both systems:

- generated rules contribute imports to the index
- existing rules can still be indexed if their resolver returns imports
- dependency resolution happens after the full index exists

The practical `vfsgazelle` difference is that plugins are expected to resolve from:

- the frozen `RuleIndex`
- the frozen VFS
- language-specific cached metadata

instead of falling back to ad hoc filesystem or remote discovery during resolve.

## Incremental Reruns

`vfsgazelle` now supports two CLI modes:

```text
gazelle-vfsgazelle run
gazelle-vfsgazelle rerun <changed paths...>
```

Useful flags:

- `-timings`
  - print per-phase timing information to stderr
  - on `rerun`, this includes metadata load and patching before the normal Gazelle phases
- `-state_format`
  - choose `gob` or `json` for the persisted `vfsgazelle` snapshot state
  - `gob` is the default
- `-state_compression`
  - choose `none`, `gzip`, or `zstd` for persisted `vfsgazelle` state files
  - `none` is the default

`run` does a full snapshot build and saves state in the OS cache directory.

`rerun` does this:

1. load the saved frozen snapshot metadata
2. start per-parser cache loads in the background
3. patch only the changed/new/deleted paths
4. if the patch is a semantic no-op, reuse the previous frozen snapshot
5. otherwise rerun the whole Gazelle algorithm against the patched snapshot
6. save the resulting snapshot back to disk

This means `rerun` is already useful even without a built-in watcher.

### No-op Protection

Two safeguards are important for stable incremental use:

1. unchanged patched files are ignored

   - if a changed path is passed to `rerun` but it is semantically unchanged for the relevant snapshot input, the patch is a no-op
   - if all incoming changes are no-ops, `vfsgazelle` skips the full walk/generate/resolve algorithm entirely

2. unchanged BUILD output is not rewritten
   - before writing a `BUILD.bazel` file, `vfsgazelle` compares the formatted output to the existing file bytes
   - if the content is identical, it does not rewrite the file

Those two behaviors are what make external watch tooling practical without falling into self-triggered loops.

## Watchman Setup

There is no built-in Watchman daemon in `vfsgazelle` yet. The intended model is:

- let Watchman or another tool detect changed files
- pass the changed repo-relative paths to `gazelle-vfsgazelle rerun`

The important part is that the watcher batches and coalesces file changes before invoking Gazelle.

A practical shape is:

1. do one initial cold run

```sh
bazel run //vfsgazelle/cmd/gazelle -- run
```

1. configure Watchman to invoke a small wrapper script
2. the wrapper script passes the changed file list to:

```sh
bazel run //vfsgazelle/cmd/gazelle -- rerun path/to/file1.go path/to/file2.proto
```

Recommended filters:

- ignore `.git`
- ignore `bazel-*`
- ignore editor temp files
- ignore generated files you know Gazelle should not inspect

Because `rerun` skips unchanged content and emit skips identical BUILD rewrites, a Watchman-driven loop can be stable without first building watcher logic into Gazelle itself.

A simple Watchman trigger example looks like this:

```sh
watchman watch ~/src/myrepo
watchman -- trigger ~/src/myrepo gazelle-vfsgazelle \
  '**/*.go' '**/*.proto' '**/BUILD' '**/BUILD.bazel' \
  '**/WORKSPACE' '**/WORKSPACE.bazel' '**/REPO.bazel' '.bazelignore' \
  -- ./tools/run-gazelle-vfsgazelle-rerun.sh
```

In practice most teams should use a small wrapper script instead of relying on a one-line trigger:

1. collect the changed repo-relative paths from Watchman
2. drop paths under `.git`, `bazel-*`, editor temp files, and other known junk
3. invoke:

```sh
bazel run //vfsgazelle/cmd/gazelle -- rerun <paths...>
```

These examples may not work as-is in every environment. Check Watchman syntax and trigger behavior here:

- <https://facebook.github.io/watchman/>

Or build your own file system service that collects changed repo-relative paths and invokes:

```sh
bazel run //vfsgazelle/cmd/gazelle -- rerun <paths...>
```

The important point is not the exact Watchman syntax. The important point is that `vfsgazelle` expects a coalesced list of changed repo-relative paths, and it is safe for that list to include some noise because the rerun path does semantic no-op filtering for parser-backed files and content checks for control files.

## Why vfsgazelle Exists

`vfsgazelle` is not about changing the semantic rule-generation model. It is about changing the execution model.

Benefits:

- parser results are cached instead of recomputed every run
- repo IO is centralized behind a VFS
- the run phase works against immutable in-memory state
- incremental reruns can patch a prior snapshot instead of rebuilding everything
- no-op reruns can short-circuit almost completely

This is different from classic lazy indexing.

### vfsgazelle vs Lazy Indexing

Lazy indexing optimizes one part of the classic algorithm:

- it avoids indexing the whole repo eagerly
- it is still fundamentally tied to the classic filesystem-driven traversal model

`vfsgazelle` is broader:

- it snapshots the repo
- it caches parsed models
- it supports saved state across process restarts
- it supports path-based patching for reruns

Lazy indexing also depends on reasonably strong path conventions. In practice, it works best when import paths, package roots, and directory layout line up in a predictable way. Some monorepos and mixed-language codebases do not have those conventions, which makes lazy indexing less effective or harder to configure correctly.

So lazy indexing is mainly an indexing optimization inside the classic design, while `vfsgazelle` is a different execution substrate for the whole algorithm and does not depend as heavily on those path-structure assumptions.

### Why vfsgazelle Can Be Faster

For a cold run on a repo, `vfsgazelle` still pays snapshot cost. The real performance payoff comes from reruns:

- unchanged files are not reparsed
- unchanged snapshots can skip the whole algorithm
- changed-file reruns avoid rebuilding the whole VFS

That is why `vfsgazelle` is most interesting for:

- editor-triggered reruns
- watcher-driven workflows
- repeated local development runs

## Reference

- Old project README: [docs/README.md](docs/README.md)
- Extension guide: [extend.md](extend.md)
- Rule reference: [reference.md](reference.md)
- Command/config reference: [gazelle-reference.md](gazelle-reference.md)
- How classic Gazelle works: [how-gazelle-works.md](how-gazelle-works.md)
