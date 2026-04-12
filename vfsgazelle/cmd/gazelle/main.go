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
	if cmd == rerunCmd {
		registry, err := run.Registry(langs)
		if err != nil {
			return err
		}
		loadStart := time.Now()
		snapshot, err = loadSnapshot(statePath(cfg.RepoRoot, vfs.StateFormat(stateFormat)), registry)
		timingOffset = time.Since(loadStart)
		if timings {
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
	return saveSnapshot(statePath(cfg.RepoRoot, vfs.StateFormat(stateFormat)), result.Snapshot, vfs.StateFormat(stateFormat))
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

func statePath(repoRoot string, format vfs.StateFormat) string {
	cacheDir, err := os.UserCacheDir()
	ext := ".gob"
	if format == vfs.StateFormatJSON {
		ext = ".json"
	}
	if err != nil {
		return filepath.Join(repoRoot, ".gazelle-vfsgazelle-state"+ext)
	}
	sum := sha256.Sum256([]byte(repoRoot))
	return filepath.Join(cacheDir, "bazel-gazelle", "vfsgazelle", fmt.Sprintf("%x%s", sum[:8], ext))
}

func saveSnapshot(path string, snapshot *vfs.Snapshot, format vfs.StateFormat) error {
	if snapshot == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return snapshot.Save(f, format)
}

func loadSnapshot(path string, registry *vfs.Registry) (*vfs.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	magic, err := r.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var reader io.Reader = r
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	}
	return vfs.LoadSnapshot(reader, registry)
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
