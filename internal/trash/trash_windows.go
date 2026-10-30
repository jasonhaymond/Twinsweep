//go:build windows

// Windows Recycle Bin support, via the Windows Shell's SHFileOperationW
// function (the same call Explorer's "Delete" uses).
//
// Big caveat this file guards against: when the shell is asked to "recycle"
// something on a drive that has no Recycle Bin (network shares, most USB
// sticks/SD cards), it silently deletes it PERMANENTLY instead. So before
// calling it we check the drive type and only proceed for fixed (internal)
// drives. As a second line of defence we pass FOF_WANTNUKEWARNING, which makes
// Windows show a warning dialog instead of silently nuking if it still can't
// recycle (e.g. the user turned the Recycle Bin off for that drive).
package trash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NewLazySystemDLL loads shell32.dll only from C:\Windows\System32, never
// from the current folder - this prevents "DLL planting" (a malicious
// shell32.dll placed next to twinsweep.exe being loaded instead).
var procSHFileOperationW = windows.NewLazySystemDLL("shell32.dll").NewProc("SHFileOperationW")

// Constants from the Windows SDK header shellapi.h.
const (
	foDelete           = 0x0003
	fofSilent          = 0x0004 // no progress dialog
	fofNoConfirmation  = 0x0010 // don't ask "are you sure?" (we already verified)
	fofAllowUndo       = 0x0040 // = send to Recycle Bin instead of deleting
	fofNoErrorUI       = 0x0400 // report errors to us, not in a dialog
	fofWantNukeWarning = 0x4000 // warn instead of silently deleting permanently
	maxShellPath       = 260    // SHFileOperation doesn't support longer paths
	driveFixed         = 3      // GetDriveType: internal hard disk / SSD
	// volumeBufLen is the buffer size (in UTF-16 characters) for a volume
	// root path. Typed uint32 so it's passed to Windows with no conversion.
	volumeBufLen uint32 = windows.MAX_PATH + 1
)

// shFileOpStruct mirrors the C struct SHFILEOPSTRUCTW. On 64-bit Windows it
// uses normal alignment, which Go matches; 32-bit Windows packs it
// differently, so MoveToTrash refuses to run there (no 32-bit builds ship).
type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// MoveToTrash sends path (a file or folder) to the Recycle Bin.
//
// Returns ErrNoTrash, having changed nothing, when the path is not on a
// fixed local drive. Returns an error if the shell reports failure or the
// item still exists afterwards.
func MoveToTrash(path string) error {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return errors.New("the Recycle Bin is only supported on 64-bit Windows")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// The shell API rejects \\?\ long-path prefixes and paths over 260 chars.
	if strings.HasPrefix(abs, `\\?\`) || len(abs) >= maxShellPath {
		return fmt.Errorf("path is too long for the Windows Recycle Bin (%d characters); use permanent delete or shorten the path", len(abs))
	}
	if err := requireFixedDrive(abs); err != nil {
		return err
	}

	// pFrom must be a list of paths terminated by TWO null characters.
	from, err := windows.UTF16FromString(abs) // adds one terminating null
	if err != nil {
		return err
	}
	from = append(from, 0) // the second null
	op := shFileOpStruct{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI | fofWantNukeWarning,
	}
	// Call the Windows function. r is its integer return value (0 = success).
	// unsafe.Pointer is how Go hands the address of a struct to a Windows
	// API; it's safe here because op lives until the call returns and its
	// layout matches the C struct exactly (see shFileOpStruct).
	r, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op))) // #nosec G103 -- required to pass SHFILEOPSTRUCTW to the Windows shell; layout verified, 64-bit only
	if r != 0 {
		return fmt.Errorf("the Windows shell could not move it to the Recycle Bin (error 0x%x)", r)
	}
	if op.fAnyOperationsAborted != 0 {
		return errors.New("moving to the Recycle Bin was cancelled")
	}
	// Belt and braces: confirm it's really gone from its original location.
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("the item is still present after moving to the Recycle Bin")
	}
	return nil
}

// requireFixedDrive returns ErrNoTrash unless abs is on an internal fixed
// drive. It finds the volume root first (e.g. "C:\" or a folder-mounted
// volume like "C:\Mounts\Disk2\"), because GetDriveType needs the root.
func requireFixedDrive(abs string) error {
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return err
	}
	buf := make([]uint16, volumeBufLen)
	if err := windows.GetVolumePathName(p, &buf[0], volumeBufLen); err != nil {
		return fmt.Errorf("%w (could not determine the drive: %v)", ErrNoTrash, err)
	}
	if windows.GetDriveType(&buf[0]) != driveFixed {
		return fmt.Errorf("%w: %s is not on an internal fixed drive", ErrNoTrash, abs)
	}
	return nil
}
