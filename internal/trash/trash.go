// Package trash moves a file or folder to the operating system's Recycle Bin
// / Trash, so a removal can be undone from Explorer, Finder or the Linux
// file manager the usual way.
//
// Where it sits: called only by internal/clean, after that package has
// verified the item is a genuine duplicate. The per-OS implementations are in
// trash_windows.go, trash_darwin.go and trash_unix.go (Linux/BSD); Go picks
// the right one at build time from the `//go:build` line at the top of each.
//
// Safety rule shared by every implementation: if a real, recoverable trash
// isn't available for the item's drive, return ErrNoTrash and do NOTHING.
// Never quietly fall back to permanent deletion - the user asked for an
// undoable action and must explicitly choose permanent delete instead.
package trash

import "errors"

// ErrNoTrash means the item's location has no usable Recycle Bin/Trash
// (network share, USB stick on Windows, a Linux mount without a writable
// .Trash folder...). Nothing was changed.
var ErrNoTrash = errors.New("no Recycle Bin/Trash is available for this location; nothing was removed (use permanent delete if you really mean it)")
