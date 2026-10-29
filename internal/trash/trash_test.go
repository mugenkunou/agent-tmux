package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup points the trash at a temp XDG_DATA_HOME so tests never touch the real one.
func setup(t *testing.T) (dataHome, work string) {
	t.Helper()
	dataHome = t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	return dataHome, t.TempDir()
}

func TestRootHonorsXDGDataHome(t *testing.T) {
	dataHome, _ := setup(t)
	got, err := Root()
	if err != nil || got != filepath.Join(dataHome, "Trash") {
		t.Fatalf("Root() = %q, %v", got, err)
	}
}

func TestRootDefaultsToHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := Root()
	if err != nil || got != filepath.Join(home, ".local", "share", "Trash") {
		t.Fatalf("Root() = %q, %v", got, err)
	}
}

func TestMoveDirWritesInfo(t *testing.T) {
	dataHome, work := setup(t)
	dir := filepath.Join(work, "web 1%")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.000001.log"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	dest, err := Move(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dataHome, "Trash", "files", "web 1%")
	if dest != want {
		t.Fatalf("dest = %q, want %q", dest, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("source still exists: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "stdout.000001.log")); err != nil || string(b) != "hi" {
		t.Errorf("content = %q, %v", b, err)
	}
	info, err := os.ReadFile(filepath.Join(dataHome, "Trash", "info", "web 1%.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(info)
	if !strings.HasPrefix(s, "[Trash Info]\nPath=") || !strings.Contains(s, "web%201%25\n") || !strings.Contains(s, "\nDeletionDate=") {
		t.Errorf("trashinfo = %q", s)
	}
}

func TestMoveNameCollision(t *testing.T) {
	dataHome, work := setup(t)
	var dests []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(work, "rec")
		if err := os.WriteFile(p, []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := Move(p)
		if err != nil {
			t.Fatal(err)
		}
		dests = append(dests, d)
	}
	files := filepath.Join(dataHome, "Trash", "files")
	for i, name := range []string{"rec", "rec.2", "rec.3"} {
		if dests[i] != filepath.Join(files, name) {
			t.Errorf("dest[%d] = %q, want %q", i, dests[i], name)
		}
		if b, _ := os.ReadFile(dests[i]); string(b) != string(rune('a'+i)) {
			t.Errorf("%s content = %q", name, b)
		}
		if _, err := os.Stat(filepath.Join(dataHome, "Trash", "info", name+".trashinfo")); err != nil {
			t.Errorf("missing info for %s: %v", name, err)
		}
	}
}

func TestMoveMissingSourceFails(t *testing.T) {
	_, work := setup(t)
	if _, err := Move(filepath.Join(work, "nope")); err == nil {
		t.Fatal("want an error")
	}
}

func TestCopyTree(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/f", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, "dst")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "f")); string(b) != "x" {
		t.Errorf("copied content = %q", b)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "sub/f" {
		t.Errorf("link = %q, %v", target, err)
	}
	if err := copyTree(src, dst); err == nil {
		// Directories merge; an existing file must not be overwritten.
		t.Error("copy over an existing tree should fail")
	}
}
