package privatefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/enetx/g"
)

func TestPrivateCreateAndReplace(t *testing.T) {
	dir := g.String(filepath.Join(t.TempDir(), "private"))
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	path := g.String(filepath.Join(dir.Std(), "receipt.json"))
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("original"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := Create(path); err == nil {
		duplicate.Close()
		t.Fatal("replaced existing private file")
	}
	replacement, err := os.CreateTemp(dir.Std(), ".replacement-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(replacement); err != nil {
		t.Fatal("temporary receipt did not inherit private access:", err)
	}
	if _, err := replacement.WriteString("updated"); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Replace(g.String(replacement.Name()), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path.Std())
	if err != nil || string(data) != "updated" {
		t.Fatalf("replacement not persisted: %q %v", data, err)
	}
}

func TestRejectsSharedFileAndDirectory(t *testing.T) {
	dir := g.String(filepath.Join(t.TempDir(), "private"))
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	path := g.String(filepath.Join(dir.Std(), "shared.json"))
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for target := range g.SliceOf(path, dir).Iter() {
		if runtime.GOOS == "windows" {
			if output, err := exec.Command("icacls", target.Std(), "/grant", "*S-1-1-0:(R)").CombinedOutput(); err != nil {
				t.Fatalf("grant test access: %v %s", err, output)
			}
		} else if err := os.Chmod(target.Std(), 0755); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(target.Std())
		if err != nil {
			t.Fatal(err)
		}
		err = Check(file)
		file.Close()
		if err == nil {
			t.Fatal("accepted shared access:", target)
		}
	}
	if err := MkdirAll(dir); err == nil {
		t.Fatal("silently changed or accepted an existing shared directory")
	}
}
