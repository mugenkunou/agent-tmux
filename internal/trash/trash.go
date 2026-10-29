// Package trash moves files and directories to the user's XDG trash
// ($XDG_DATA_HOME/Trash, default ~/.local/share/Trash) in the freedesktop
// layout (files/ and info/*.trashinfo,
// https://specifications.freedesktop.org/trash-spec), so the desktop's file
// manager sees them and can restore them. Nothing is ever deleted for good.
package trash

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Root returns the user's XDG trash directory: $XDG_DATA_HOME/Trash, defaulting
// to ~/.local/share/Trash.
func Root() (string, error) {
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		return filepath.Join(dataHome, "Trash"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "Trash"), nil
}

func filesDir(root string) string { return filepath.Join(root, "files") }
func infoDir(root string) string  { return filepath.Join(root, "info") }

// Move puts path into the trash and returns where it landed (inside the
// trash's files/ directory). When the trash cannot be located or the move
// fails it returns the error; it never deletes for good as a fallback.
func Move(path string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", fmt.Errorf("find the trash: %w", err)
	}
	return moveToTrash(root, path)
}

// moveToTrash moves path into root's files/ directory and writes a matching
// .trashinfo sidecar into root's info/ directory. It returns the path the item
// was stored at (after collision resolution).
func moveToTrash(root, path string) (string, error) {
	if err := os.MkdirAll(filesDir(root), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(infoDir(root), 0o700); err != nil {
		return "", err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	name := uniqueTrashName(root, filepath.Base(absPath))
	dest := filepath.Join(filesDir(root), name)

	if err := move(absPath, dest); err != nil {
		return "", err
	}
	if err := writeTrashInfo(root, name, absPath, time.Now()); err != nil {
		return dest, err
	}
	return dest, nil
}

// uniqueTrashName returns a name for base that doesn't collide with an existing
// files/ entry or info/ sidecar, appending ".2", ".3", ... as needed: the same
// scheme gio and Nautilus use.
func uniqueTrashName(root, base string) string {
	name := base
	for n := 2; ; n++ {
		_, filesErr := os.Lstat(filepath.Join(filesDir(root), name))
		_, infoErr := os.Lstat(filepath.Join(infoDir(root), name+".trashinfo"))
		if os.IsNotExist(filesErr) && os.IsNotExist(infoErr) {
			return name
		}
		name = fmt.Sprintf("%s.%d", base, n)
	}
}

// writeTrashInfo writes the freedesktop .trashinfo sidecar for a trashed item:
// original path (percent-encoded per the spec) and deletion timestamp.
func writeTrashInfo(root, name, originalPath string, deletedAt time.Time) error {
	encoded := (&url.URL{Path: originalPath}).EscapedPath()
	content := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		encoded, deletedAt.Format("2006-01-02T15:04:05"))
	return os.WriteFile(filepath.Join(infoDir(root), name+".trashinfo"), []byte(content), 0o600)
}

// move renames src to dst. On different filesystems it copies src and then
// removes it, and a failed copy leaves src untouched. A symlink is moved as
// the link itself, like mv. dst must not exist.
func move(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		if err := copyTree(src, dst); err != nil {
			_ = os.RemoveAll(dst) // do not leave a half copy behind
			return err
		}
		return os.RemoveAll(src)
	}
	return err
}

// copyTree copies a file, directory tree or symlink to dst, keeping modes.
// dst must not exist: it never overwrites.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}

	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyTree(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
