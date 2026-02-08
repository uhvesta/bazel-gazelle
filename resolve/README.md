# Gazelle Invocation Cache

The invocation cache persists Gazelle's rule index to disk between runs.
Subsequent invocations load cached records and only re-index the directories
being updated, which can dramatically reduce runtime in large repositories where
indexing is the dominant cost.

## Quick start

Pass `-index_cache` with a file path:

```sh
# First run indexes everything and writes the cache
gazelle -index_cache=.gazelle-index.json

# Later runs load the cache, re-index only visited directories, and save back
gazelle -index_cache=.gazelle-index.json some/package/
```

That's it. One flag, one file, load and save happen automatically.

## How it works

1. **Load** -- On startup, Gazelle reads the cache file and appends the
   stored rule records to the index. If the file doesn't exist or the
   fingerprint doesn't match (see below), the cache is silently discarded
   and Gazelle proceeds as if there were no cache.

2. **Invalidate** -- As Gazelle walks directories, every visited package
   has its cached records removed before fresh records from `AddRule` are
   inserted. This means you always get up-to-date results for the
   directories you're updating.

3. **Save** -- After the walk completes (and before dependency resolution),
   the full set of records -- cached + freshly generated -- is written
   back to the cache file atomically (write to temp, rename).

4. **Fingerprint** -- The cache file includes a SHA-256 hash of the
   Gazelle binary. If you recompile Gazelle or change plugins, the
   fingerprint changes and the old cache is automatically discarded.
   This prevents stale data from a different binary version.

## Recommended setup

### File location

Use a repo-relative path so every developer and CI job shares the same
convention:

```sh
gazelle -index_cache=.gazelle-index.json
```

### Gitignore

The cache is a build artifact. Add it to `.gitignore`:

```
# .gitignore
.gazelle-index.json
```

### CI integration

In CI, you can persist the cache between builds using your CI system's
cache mechanism. For example with GitHub Actions:

```yaml
- uses: actions/cache@v4
  with:
    path: .gazelle-index.json
    key: gazelle-index-${{ runner.os }}-${{ hashFiles('go.sum') }}
    restore-keys: |
      gazelle-index-${{ runner.os }}-
- run: gazelle -index_cache=.gazelle-index.json
```

The fingerprint check means a stale cache from a different binary is
harmlessly discarded, so a broad `restore-keys` pattern is safe.

### Incremental updates

The biggest speedup comes from targeted runs that only update specific
directories:

```sh
# Update just the packages you changed
gazelle -index_cache=.gazelle-index.json path/to/changed/package/
```

The cache provides the index for all *other* packages, and only
`path/to/changed/package/` is re-indexed.

## When to use the cache

| Scenario | Benefit |
|---|---|
| Large monorepo, incremental runs on a few dirs | High -- avoids re-indexing thousands of packages |
| CI with cached artifacts between builds | Medium -- saves indexing time on warm runs |
| Small repo (<100 packages) | Low -- indexing is already fast |
| First run (no existing cache file) | None -- the cache is populated for next time |

## Failure behavior

Cache failures are **non-fatal**. If the cache can't be loaded (missing,
corrupt, wrong version, fingerprint mismatch) or can't be saved (disk
full, permission error), Gazelle logs a warning and continues normally.
The worst case is equivalent to running without `-index_cache` at all.

## Cache format

The cache is a JSON file with this structure:

```json
{
  "formatVersion": 1,
  "fingerprint": "<sha256 hex of gazelle binary>",
  "records": [
    {
      "kind": "go_library",
      "label": {"Repo": "", "Pkg": "some/pkg", "Name": "lib", ...},
      "pkg": "some/pkg",
      "importedAs": [{"Lang": "go", "Imp": "example.com/some/pkg"}],
      "embeds": [],
      "lang": "go"
    }
  ]
}
```

The format is intentionally human-readable for debugging. You can inspect
or delete the file at any time with no consequences beyond losing the
cache.
