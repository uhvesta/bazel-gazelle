package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
	"github.com/bazelbuild/bazel-gazelle/v3/run"
	v3walk "github.com/bazelbuild/bazel-gazelle/v3/walk"
)

type command int

const (
	runCmd command = iota
	helpCmd
)

func main() {
	log.SetPrefix("gazelle-v3: ")
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

func runCLI(wd string, args []string, langs []v3language.Language) error {
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
	fs := flag.NewFlagSet("gazelle-v3", flag.ContinueOnError)
	fs.Usage = func() {
		_ = help()
	}
	for _, cext := range configurers {
		cext.RegisterFlags(fs, cmd.String(), cfg)
	}
	if err := fs.Parse(cmdArgs); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("path-scoped runs are not supported in v3 yet: %v", fs.Args())
	}
	for _, cext := range configurers {
		if err := cext.CheckFlags(fs, cfg); err != nil {
			return err
		}
	}

	_, err = run.Run(run.Options{
		Config:      cfg,
		Languages:   langs,
		Configurers: configurers,
		Prepared:    true,
		Emit: func(c *config.Config, f *rule.File) error {
			f.Sync()
			return os.WriteFile(f.Path, f.Format(), 0o666)
		},
	})
	return err
}

func makeConfigurers(langs []v3language.Language) []config.Configurer {
	configurers := []config.Configurer{
		&config.CommonConfigurer{},
		&v3walk.Configurer{},
		&resolve.Configurer{},
	}
	for _, lang := range langs {
		configurers = append(configurers, lang)
	}
	return configurers
}

func help() error {
	fmt.Fprint(os.Stderr, `usage: gazelle-v3 <command> [flags]

Gazelle v3 runs the snapshot-backed VFS pipeline with the configured v3 languages.

Commands:
  run     build the VFS snapshot, then run the whole-repo v3 pipeline
  help    show this message

Notes:
  v3 currently runs on the whole repository.
  Bare invocation is the same as 'run'.
  Path-scoped runs, rerun-with-changes, and watch mode are not wired into this CLI yet.
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
	case "help", "-h", "-help", "--help":
		return helpCmd, args[1:], nil
	default:
		return runCmd, args, nil
	}
}

func (cmd command) String() string {
	switch cmd {
	case runCmd:
		return "run"
	case helpCmd:
		return "help"
	default:
		return "run"
	}
}
