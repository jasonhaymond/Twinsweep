//go:build unix && !darwin

// Linux / BSD Trash support, implementing the freedesktop.org Trash spec
// (https://specifications.freedesktop.org/trash-spec/) that GNOME Files,
// KDE Dolphin, Thunar and friends all use - so items Twinsweep trashes appear
// in the normal Trash and can be restored from there.
//
// How the spec works, briefly:
//   - The "home trash" is $XDG_DATA_HOME/Trash (default ~/.local/share/Trash)
//     with two subfolders: files/ (the trashed items) and info/ (one small
//     .trashinfo text file per item recording its original path and when it
//     was deleted).
//   - Moving across filesystems would mean copying (slow, and not atomic), so
//     items on another drive go to a trash folder at the top of THAT drive:
//     either a shared $topdir/.Trash/$UID (only if an admin created .Trash
//     with the sticky bit) or a private $topdir/.Trash-$UID.
//
// If neither works (e.g. no permission to create .Trash-$UID at the top of a
// drive), we return ErrNoTrash and change nothing.
package trash

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// MoveToTrash sends path (file or folder) to the appropriate freedesktop
// trash folder. Side effects: writes one .trashinfo file and renames the
// item into the trash. If the rename fails, the .trashinfo is removed again.
func MoveToTrash(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	itemDev, err := deviceOf(abs, true)
	if err != nil {
		return err
	}

	// 1. Home trash, if it's on the same filesystem as the item.
	home, err := homeTrash()
	if err == nil {
		if err := ensureTrashDirs(home); err == nil {
			if dev, err := deviceOf(home, false); err == nil && dev == itemDev {
				return moveInto(home, abs, abs)
			}
		}
	}

	// 2. Per-drive trash at the top of the item's filesystem.
	top, err := mountTop(abs, itemDev)
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrNoTrash, err)
	}
	uid := strconv.Itoa(os.Getuid())
	// Paths in a per-drive trash are recorded relative to the drive top, so
	// the trash keeps working if the drive is mounted somewhere else later.
	rel, err := filepath.Rel(top, abs)
	if err != nil {
		return err
	}
	if dir, ok := sharedTopTrash(top, uid); ok {
		return moveInto(dir, abs, rel)
	}
	private := filepath.Join(top, ".Trash-"+uid)
	if err := ensurePrivateTopTrash(private); err != nil {
		return fmt.Errorf("%w (%v)", ErrNoTrash, err)
	}
	return moveInto(private, abs, rel)
}

// homeTrash returns $XDG_DATA_HOME/Trash, or ~/.local/share/Trash.
func homeTrash() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "Trash"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".local", "share", "Trash"), nil
}

// ensureTrashDirs creates trash/files and trash/info (owner-only, 0700) if
// they don't exist yet.
func ensureTrashDirs(trash string) error {
	for _, sub := range []string{"files", "info"} {
		if err := os.MkdirAll(filepath.Join(trash, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// deviceOf returns the filesystem (device) number a path lives on. lstat
// means "don't follow a symlink at path itself".
func deviceOf(path string, lstat bool) (uint64, error) {
	var info fs.FileInfo
	var err error
	if lstat {
		info, err = os.Lstat(path)
	} else {
		info, err = os.Stat(path)
	}
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("cannot determine filesystem")
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev's type differs between Linux and the BSDs
}

// mountTop walks up from path until the parent is on a different device;
// that folder is the top of the filesystem ("mount point").
func mountTop(path string, dev uint64) (string, error) {
	dir := filepath.Dir(path)
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir, nil // reached "/"
		}
		pd, err := deviceOf(parent, false)
		if err != nil {
			return "", err
		}
		if pd != dev {
			return dir, nil
		}
		dir = parent
	}
}

// sharedTopTrash returns $top/.Trash/$uid if an administrator set up a
// shared .Trash correctly. The spec requires .Trash to be a real folder (not
// a symlink - a symlink could redirect our files anywhere) with the sticky
// bit set (so users can't delete each other's trashed files).
func sharedTopTrash(top, uid string) (string, bool) {
	base := filepath.Join(top, ".Trash")
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || info.Mode()&fs.ModeSticky == 0 {
		return "", false
	}
	dir := filepath.Join(base, uid)
	if err := ensureTrashDirs(dir); err != nil {
		return "", false
	}
	return dir, true
}

// ensurePrivateTopTrash creates (or validates) $top/.Trash-$uid. If it
// already exists it must be a real folder owned by us - otherwise another
// user could have pre-created it to capture our deleted files.
func ensurePrivateTopTrash(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s exists but is not a folder owned by you", dir)
	}
	return ensureTrashDirs(dir)
}

// moveInto performs the spec's two-step trash operation inside one trash
// folder: reserve a unique name by creating its .trashinfo file (O_EXCL =
// fail if it already exists, so two programs trashing "photo.jpg" at the
// same moment can't collide), then rename the item into files/.
// recorded is the path written into the .trashinfo (absolute for the home
// trash, drive-relative for a per-drive trash).
func moveInto(trash, abs, recorded string) error {
	base := filepath.Base(abs)
	for n := 1; n < 10000; n++ {
		name := base
		if n > 1 {
			name = base + "." + strconv.Itoa(n)
		}
		infoPath := filepath.Join(trash, "info", name+".trashinfo")
		f, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue // name taken; try photo.jpg.2, photo.jpg.3, ...
		}
		if err != nil {
			return err
		}
		// The spec stores the path URL-encoded ("My File.txt" -> "My%20File.txt").
		content := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
			(&url.URL{Path: recorded}).EscapedPath(), time.Now().Format("2006-01-02T15:04:05"))
		_, werr := f.WriteString(content)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(infoPath)
			return errors.Join(werr, cerr)
		}
		dest := filepath.Join(trash, "files", name)
		if _, err := os.Lstat(dest); err == nil {
			_ = os.Remove(infoPath) // stray file without info; skip this name
			continue
		}
		if err := os.Rename(abs, dest); err != nil {
			_ = os.Remove(infoPath)
			if errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("%w (trash is on a different drive)", ErrNoTrash)
			}
			return err
		}
		return nil
	}
	return errors.New("could not find a free name in the trash")
}
