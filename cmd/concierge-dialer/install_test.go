package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/enetx/g"
)

func TestInstallBothCopiesSkillAndBinary(t *testing.T) {
	source, binary := installFixture(t)
	home := g.String(t.TempDir())
	result := install("both", source, home, binary)
	if result.IsErr() {
		t.Fatal(result.Err())
	}
	if result.Unwrap().Len() != 2 {
		t.Fatalf("expected two targets: %v", result.Unwrap())
	}
	for destination := range result.Unwrap().Iter() {
		for path, want := range (g.Map[g.String, g.String]{"SKILL.md": "---\nname: concierge-dialer\ndescription: Call businesses\n---\n", "references/setup.md": "Setup\n", "bin/" + binaryName(): "test binary\n"}).Iter() {
			data, err := os.ReadFile(filepath.Join(destination.Std(), path.Std()))
			if err != nil || g.String(data) != want {
				t.Fatalf("%s: %q %v", path, data, err)
			}
		}
		info, err := os.Stat(filepath.Join(destination.Std(), "bin", binaryName().Std()))
		if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0700) {
			t.Fatalf("binary permissions: %v %v", info, err)
		}
	}
}

func TestInstallBothPreflightsEveryDestination(t *testing.T) {
	source, binary := installFixture(t)
	home := g.String(t.TempDir())
	existing := filepath.Join(home.Std(), ".claude", "skills", "concierge-dialer")
	if err := os.MkdirAll(existing, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "keep"), g.String("untouched").Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if result := install("both", source, home, binary); result.IsOk() {
		t.Fatal("overwrote existing destination")
	}
	if _, err := os.Stat(filepath.Join(home.Std(), ".agents", "skills", "concierge-dialer")); !os.IsNotExist(err) {
		t.Fatalf("first target was changed despite existing second target: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(existing, "keep"))
	if err != nil || g.String(data) != "untouched" {
		t.Fatalf("existing target changed: %q %v", data, err)
	}
}

func TestInstallRejectsSymlinksAndMissingSource(t *testing.T) {
	source, binary := installFixture(t)
	home := g.String(t.TempDir())
	if err := os.Symlink(binary.Std(), filepath.Join(source.Std(), "linked-secret")); err != nil {
		t.Fatal(err)
	}
	if result := install("codex", source, home, binary); result.IsOk() {
		t.Fatal("copied symlink")
	}
	if _, err := os.Stat(filepath.Join(home.Std(), ".agents")); !os.IsNotExist(err) {
		t.Fatalf("failed source validation changed home: %v", err)
	}
	if install("codex", "", home, binary).IsOk() || install("unknown", source, home, binary).IsOk() {
		t.Fatal("accepted invalid install arguments")
	}
}

func TestInstallRejectsDestinationInsideSource(t *testing.T) {
	source, binary := installFixture(t)
	if result := install("both", source, source, binary); result.IsOk() {
		t.Fatal("accepted recursive installation")
	}
	if _, err := os.Stat(filepath.Join(source.Std(), ".agents")); !os.IsNotExist(err) {
		t.Fatalf("recursive installation changed source: %v", err)
	}
}

func TestInstallRollsBackOnlyNewDestinationsOnCopyFailure(t *testing.T) {
	source, binary := installFixture(t)
	home := g.String(t.TempDir())
	if err := os.WriteFile(filepath.Join(source.Std(), "bin"), g.String("not a directory").Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if result := install("both", source, home, binary); result.IsOk() {
		t.Fatal("expected bin directory collision to fail")
	}
	for client := range g.SliceOf[g.String](".agents", ".claude").Iter() {
		if _, err := os.Stat(filepath.Join(home.Std(), client.Std(), "skills", "concierge-dialer")); !os.IsNotExist(err) {
			t.Fatalf("failed installation left a destination: %v", err)
		}
	}
}

func TestInstallFromPackagedSkillUsesRunningBinary(t *testing.T) {
	source, binary := installFixture(t)
	if err := os.Mkdir(filepath.Join(source.Std(), "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Std(), "bin", binaryName().Std()), []byte("old binary"), 0700); err != nil {
		t.Fatal(err)
	}
	result := install("both", source, g.String(t.TempDir()), binary)
	if result.IsErr() {
		t.Fatal(result.Err())
	}
	for dest := range result.Unwrap().Iter() {
		b, err := os.ReadFile(filepath.Join(dest.Std(), "bin", binaryName().Std()))
		if err != nil || string(b) != "test binary\n" {
			t.Fatal("packaged executable was duplicated or replaced incorrectly")
		}
	}
}

func installFixture(t *testing.T) (g.String, g.String) {
	t.Helper()
	source := g.String(t.TempDir())
	if err := os.Mkdir(filepath.Join(source.Std(), "references"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range (g.Map[g.String, g.String]{"SKILL.md": "---\nname: concierge-dialer\ndescription: Call businesses\n---\n", "references/setup.md": "Setup\n"}).Iter() {
		if err := os.WriteFile(filepath.Join(source.Std(), name.Std()), data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return source, privateFile(t, "concierge-dialer", "test binary\n")
}
