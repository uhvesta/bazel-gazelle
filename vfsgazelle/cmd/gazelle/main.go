package main

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/uhvesta/bazel-gazelle/config"
	"github.com/uhvesta/bazel-gazelle/resolve"
	"github.com/uhvesta/bazel-gazelle/rule"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
	vfsgazellelanguage "github.com/uhvesta/bazel-gazelle/vfsgazelle/language"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/run"
	vfsgazellewalk "github.com/uhvesta/bazel-gazelle/vfsgazelle/walk"
)

type command int

const (
	runCmd command = iota
	rerunCmd
	helpCmd
)

func main() {
	log.SetPrefix("gazelle-vfsgazelle: ")
	log.SetFlags(0)

	wd := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	if err := runCLI(wd, os.Args[1:], languages); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runCLI(wd string, args []string, langs []vfsgazellelanguage.Language) error {
	cmd, cmdArgs, err := parseCommand(args)
	if err != nil {
		return err
	}
	if cmd == helpCmd {
		return help()
	}

	cfg := config.New()
	cfg.WorkDir = wd

	configurers := makeConfigurers(langs)
	fs := flag.NewFlagSet("gazelle-vfsgazelle", flag.ContinueOnError)
	var timings bool
	var stateFormat string
	fs.BoolVar(&timings, "timings", false, "print per-phase vfsgazelle run timings to stderr")
	fs.StringVar(&stateFormat, "state_format", string(vfs.StateFormatGob), "persisted vfsgazelle state format: gob or json")
	fs.Usage = func() {
		_ = help()
	}
	for _, cext := range configurers {
		cext.RegisterFlags(fs, cmd.String(), cfg)
	}
	if err := fs.Parse(cmdArgs); err != nil {
		return err
	}
	if cmd == runCmd && fs.NArg() > 0 {
		return fmt.Errorf("path-scoped runs are not supported in vfsgazelle yet: %v", fs.Args())
	}
	if cmd == rerunCmd && fs.NArg() == 0 {
		return fmt.Errorf("rerun requires at least one changed path")
	}
	for _, cext := range configurers {
		if err := cext.CheckFlags(fs, cfg); err != nil {
			return err
		}
	}

	var snapshot *vfs.Snapshot
	var timingOffset time.Duration
	emitted := make(map[string][]byte)
	if cmd == rerunCmd {
		registry, err := run.Registry(langs)
		if err != nil {
			return err
		}
		loadStart := time.Now()
		var metadataLoad, cacheLoad time.Duration
		snapshot, metadataLoad, cacheLoad, err = loadSnapshot(stateBasePath(cfg.RepoRoot, vfs.StateFormat(stateFormat)), registry)
		timingOffset = time.Since(loadStart)
		if timings {
			if metadataLoad > 0 {
				log.Printf("timing %-16s %s", "read_vfs_meta", metadataLoad)
			}
			if cacheLoad > 0 {
				log.Printf("timing %-16s %s", "read_vfs_cache", cacheLoad)
			}
			log.Printf("timing %-16s %s", "read_vfs_from_cache", timingOffset)
		}
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			log.Printf("no prior vfsgazelle state found; falling back to full run")
		}
	}

	result, err := run.Run(run.Options{
		Config:       cfg,
		Languages:    langs,
		Configurers:  configurers,
		Prepared:     true,
		Timings:      timings,
		TimingOffset: timingOffset,
		Snapshot:     snapshot,
		Changes:      changesFromArgs(fs.Args()),
		Emit: func(c *config.Config, f *rule.File) error {
			f.Sync()
			formatted := f.Format()
			rel, err := filepath.Rel(cfg.RepoRoot, f.Path)
			if err != nil {
				return err
			}
			emitted[filepath.ToSlash(rel)] = append([]byte(nil), formatted...)
			if existing, err := os.ReadFile(f.Path); err == nil && string(existing) == string(formatted) {
				return nil
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return os.WriteFile(f.Path, formatted, 0o666)
		},
	})
	if err != nil {
		return err
	}
	if len(emitted) > 0 {
		result.Snapshot = result.Snapshot.WithFileContents(emitted)
	}
	return saveSnapshot(stateBasePath(cfg.RepoRoot, vfs.StateFormat(stateFormat)), result.Snapshot, vfs.StateFormat(stateFormat))
}

func makeConfigurers(langs []vfsgazellelanguage.Language) []config.Configurer {
	configurers := []config.Configurer{
		&config.CommonConfigurer{},
		&vfsgazellewalk.Configurer{},
		&resolve.Configurer{},
	}
	for _, lang := range langs {
		configurers = append(configurers, lang)
	}
	return configurers
}

func help() error {
	fmt.Fprint(os.Stderr, `usage: gazelle-vfsgazelle <command> [flags]

Gazelle vfsgazelle runs the snapshot-backed VFS pipeline with the configured vfsgazelle languages.

Commands:
  run     build the VFS snapshot, then run the whole-repo vfsgazelle pipeline
  rerun   load saved vfsgazelle state, patch changed paths, then run the whole-repo vfsgazelle pipeline
  help    show this message

Notes:
  vfsgazelle currently runs on the whole repository.
  Bare invocation is the same as 'run'.
  Use -timings to print per-phase timing information.
  Use -state_format to choose gob or json for the saved vfsgazelle state file.
  run saves VFS state in the OS cache dir for later rerun commands.
  rerun expects changed/new/deleted repo-relative paths.
  Watch mode is not wired into this CLI yet.
`)
	return flag.ErrHelp
}

func parseCommand(args []string) (command, []string, error) {
	if len(args) == 0 {
		return runCmd, nil, nil
	}
	switch args[0] {
	case "run":
		return runCmd, args[1:], nil
	case "rerun":
		return rerunCmd, args[1:], nil
	case "help", "-h", "-help", "--help":
		return helpCmd, args[1:], nil
	default:
		return helpCmd, nil, fmt.Errorf("unknown command %q", args[0])
	}
}

func (cmd command) String() string {
	switch cmd {
	case runCmd:
		return "run"
	case rerunCmd:
		return "rerun"
	case helpCmd:
		return "help"
	default:
		return "run"
	}
}

func stateBasePath(repoRoot string, format vfs.StateFormat) string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(repoRoot, ".gazelle-vfsgazelle-state")
	}
	sum := sha256.Sum256([]byte(repoRoot))
	return filepath.Join(cacheDir, "bazel-gazelle", "vfsgazelle", fmt.Sprintf("%x", sum[:8]))
}

func statePaths(base string, format vfs.StateFormat) (string, string, string) {
	ext := ".gob"
	if format == vfs.StateFormatJSON {
		ext = ".json"
	}
	return base + ".meta" + ext, base + ".cache" + ext, base + ext
}

func parserCachePath(base, parserKey string, format vfs.StateFormat) string {
	ext := ".gob"
	if format == vfs.StateFormatJSON {
		ext = ".json"
	}
	return base + ".cache." + sanitizeParserKey(parserKey) + ext
}

func sanitizeParserKey(key string) string {
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-", "\t", "-").Replace(key)
	if replaced == "" {
		return "unknown"
	}
	return replaced
}

func saveSnapshot(base string, snapshot *vfs.Snapshot, format vfs.StateFormat) error {
	if snapshot == nil {
		return nil
	}
	metaPath, cachePath, legacyPath := statePaths(base, format)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		return err
	}
	metaFile, err := os.Create(metaPath)
	if err != nil {
		return err
	}
	defer metaFile.Close()
	if err := snapshot.SaveMetadata(metaFile, format); err != nil {
		return err
	}
	groupedCaches, err := snapshot.Cache().SnapshotByParser()
	if err != nil {
		return err
	}
	for key, persisted := range groupedCaches {
		cacheFile, err := os.Create(parserCachePath(base, key, format))
		if err != nil {
			return err
		}
		if err := vfs.SaveCachePersisted(cacheFile, format, persisted); err != nil {
			_ = cacheFile.Close()
			return err
		}
		if err := cacheFile.Close(); err != nil {
			return err
		}
	}
	_ = os.Remove(cachePath)
	_ = os.Remove(legacyPath)
	return nil
}

func loadSnapshot(base string, registry *vfs.Registry) (*vfs.Snapshot, time.Duration, time.Duration, error) {
	metaPathGob, cachePathGob, legacyGob := statePaths(base, vfs.StateFormatGob)
	metaPathJSON, cachePathJSON, legacyJSON := statePaths(base, vfs.StateFormatJSON)
	_ = cachePathGob
	_ = cachePathJSON
	metaPath := ""
	switch {
	case fileExists(metaPathGob):
		metaPath = metaPathGob
	case fileExists(metaPathJSON):
		metaPath = metaPathJSON
	default:
		legacyPath := legacyGob
		if !fileExists(legacyPath) {
			legacyPath = legacyJSON
		}
		snapshot, err := loadLegacySnapshot(legacyPath, registry)
		return snapshot, 0, 0, err
	}

	type loadResult struct {
		snapshot *vfs.Snapshot
		duration time.Duration
		err      error
	}
	metaCh := make(chan loadResult, 1)
	go func() {
		start := time.Now()
		snapshot, err := loadMetadataSnapshot(metaPath, registry)
		metaCh <- loadResult{snapshot: snapshot, duration: time.Since(start), err: err}
	}()
	metaResult := <-metaCh
	if metaResult.err != nil {
		return nil, metaResult.duration, 0, metaResult.err
	}
	loaders := make(map[string]func() (*vfs.Cache, error))
	if metaResult.snapshot != nil {
		parserVersions := metaResult.snapshot.ParserVersions()
		keys := make([]string, 0, len(parserVersions))
		for key := range parserVersions {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			version := parserVersions[key]
			parser, ok := registry.Parser(key)
			if !ok || parser.CacheVersion() != version {
				continue
			}
			cacheFilePath := parserCachePath(base, key, detectFormatFromPath(metaPath))
			if !fileExists(cacheFilePath) {
				continue
			}
			parserKey := key
			parserCachePath := cacheFilePath
			loaders[parserKey] = func() (*vfs.Cache, error) {
				return loadCachePayload(parserCachePath)
			}
		}
	}
	snapshot := metaResult.snapshot.AttachParserCacheLoaders(loaders)
	return snapshot, metaResult.duration, 0, nil
}

func detectFormatFromPath(path string) vfs.StateFormat {
	if strings.HasSuffix(path, ".json") {
		return vfs.StateFormatJSON
	}
	return vfs.StateFormatGob
}

func loadLegacySnapshot(path string, registry *vfs.Registry) (*vfs.Snapshot, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	reader, closeFn, err := openStateReader(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return vfs.LoadSnapshot(reader, registry)
}

func loadMetadataSnapshot(path string, registry *vfs.Registry) (*vfs.Snapshot, error) {
	reader, closeFn, err := openStateReader(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return vfs.LoadSnapshotMetadata(reader, registry)
}

func loadCachePayload(path string) (*vfs.Cache, error) {
	reader, closeFn, err := openStateReader(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return vfs.LoadCachePayload(reader)
}

func openStateReader(path string) (io.Reader, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	r := bufio.NewReader(f)
	magic, err := r.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		return nil, nil, err
	}
	var reader io.Reader = r
	closeFn := f.Close
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		reader = zr
		closeFn = func() error {
			if err := zr.Close(); err != nil {
				_ = f.Close()
				return err
			}
			return f.Close()
		}
	}
	return reader, closeFn, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func changesFromArgs(args []string) []vfs.Change {
	changes := make([]vfs.Change, 0, len(args))
	for _, arg := range args {
		changes = append(changes, vfs.Change{
			Path: arg,
			Kind: vfs.ChangeModify,
		})
	}
	return changes
}
