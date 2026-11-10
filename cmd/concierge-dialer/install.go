package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/enetx/g"
)

func installCommand(args g.Slice[g.String], out io.Writer) g.Result[g.Int] {
	set := flags("install")
	target := set.String("target", "", "codex, claude or both")
	source := set.String("source", "", "path to the skill directory containing SKILL.md")
	home := set.String("home", "", "installation home (defaults to your home directory)")
	if err := set.Parse(stdArgs(args)); err != nil || set.NArg() != 0 {
		return g.Err[g.Int](errors.New("invalid install flags; use help"))
	}
	if *home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return g.Err[g.Int](err)
		}
		*home = resolved
	}
	executable, err := os.Executable()
	if err != nil {
		return g.Err[g.Int](err)
	}
	installed := install(g.String(*target), g.String(*source), g.String(*home), g.String(executable))
	if installed.IsErr() {
		return g.Err[g.Int](installed.Err())
	}
	return writeJSON(out, g.Map[g.String, any]{"installed": installed.Unwrap()})
}

func install(target, source, home, executable g.String) g.Result[g.Slice[g.String]] {
	if source.IsEmpty() {
		return g.Err[g.Slice[g.String]](errors.New("--source is required; pass the skill directory containing SKILL.md"))
	}
	if home.IsEmpty() {
		return g.Err[g.Slice[g.String]](errors.New("installation home is required"))
	}
	absoluteSource, err := filepath.Abs(source.Std())
	if err != nil {
		return g.Err[g.Slice[g.String]](err)
	}
	source = g.String(absoluteSource)
	var destinations g.Slice[g.String]
	switch target {
	case "codex":
		destinations.Push(g.String(filepath.Join(home.Std(), ".agents", "skills", "concierge-dialer")))
	case "claude":
		destinations.Push(g.String(filepath.Join(home.Std(), ".claude", "skills", "concierge-dialer")))
	case "both":
		destinations.Push(g.String(filepath.Join(home.Std(), ".agents", "skills", "concierge-dialer")))
		destinations.Push(g.String(filepath.Join(home.Std(), ".claude", "skills", "concierge-dialer")))
	default:
		return g.Err[g.Slice[g.String]](errors.New("--target must be codex, claude or both"))
	}
	if info, err := os.Lstat(filepath.Join(source.Std(), "SKILL.md")); err != nil || !info.Mode().IsRegular() {
		return g.Err[g.Slice[g.String]](errors.New("--source must contain a regular SKILL.md file"))
	}
	if err := filepath.WalkDir(source.Std(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return errors.New("skill source must contain only directories and regular files")
		}
		return nil
	}); err != nil {
		return g.Err[g.Slice[g.String]](err)
	}
	if info, err := os.Stat(executable.Std()); err != nil || !info.Mode().IsRegular() {
		return g.Err[g.Slice[g.String]](errors.New("running executable must be a regular file"))
	}
	for destination := range destinations.Iter() {
		absoluteDestination, err := filepath.Abs(destination.Std())
		if err != nil {
			return g.Err[g.Slice[g.String]](err)
		}
		relative, err := filepath.Rel(source.Std(), absoluteDestination)
		if err != nil || relative == "." || (relative != ".." && !g.String(relative).StartsWith(g.String(".."+string(filepath.Separator)))) {
			return g.Err[g.Slice[g.String]](errors.New("installation destination must be outside the skill source"))
		}
		if _, err := os.Lstat(destination.Std()); err == nil {
			return g.Err[g.Slice[g.String]](fmt.Errorf("destination already exists; preserve it before updating: %s", destination))
		} else if !os.IsNotExist(err) {
			return g.Err[g.Slice[g.String]](err)
		}
	}
	created := g.Slice[g.String]{}
	complete := false
	defer func() {
		if !complete {
			created.Iter().ForEach(func(path g.String) { _ = os.RemoveAll(path.Std()) })
		}
	}()
	for destination := range destinations.Iter() {
		if err := os.MkdirAll(filepath.Dir(destination.Std()), 0700); err != nil {
			return g.Err[g.Slice[g.String]](err)
		}
		if err := os.Mkdir(destination.Std(), 0700); err != nil {
			return g.Err[g.Slice[g.String]](err)
		}
		created.Push(destination)
		if err := filepath.WalkDir(source.Std(), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(source.Std(), path)
			if err != nil {
				return err
			}
			if relative == "." {
				return nil
			}
			if relative == filepath.Join("bin", "concierge-dialer") {
				return nil // Install the running binary once, including when copying an installed skill.
			}
			dest := filepath.Join(destination.Std(), relative)
			if entry.IsDir() {
				return os.Mkdir(dest, 0700)
			}
			if !entry.Type().IsRegular() {
				return errors.New("skill source changed during installation")
			}
			return copyFile(g.String(path), g.String(dest), 0600).Err()
		}); err != nil {
			return g.Err[g.Slice[g.String]](err)
		}
		bin := g.String(filepath.Join(destination.Std(), "bin"))
		if err := os.MkdirAll(bin.Std(), 0700); err != nil {
			return g.Err[g.Slice[g.String]](err)
		}
		if copied := copyFile(executable, g.String(filepath.Join(bin.Std(), "concierge-dialer")), 0700); copied.IsErr() {
			return g.Err[g.Slice[g.String]](copied.Err())
		}
	}
	complete = true
	return g.Ok(destinations)
}

func copyFile(source, destination g.String, mode fs.FileMode) g.Result[bool] {
	input, err := os.Open(source.Std())
	if err != nil {
		return g.Err[bool](err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination.Std(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return g.Err[bool](err)
	}
	_, err = io.Copy(output, input)
	closed := output.Close()
	if err != nil {
		return g.Err[bool](err)
	}
	if closed != nil {
		return g.Err[bool](closed)
	}
	return g.Ok(true)
}
