//go:build windows

// Physical-file identity on Windows (NTFS/ReFS). See fileid_unix.go for why
// the scanner needs this: hardlinked paths share one copy of the data and are
// not reported as duplicates.
package scan

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// fileKey returns "volumeSerial:fileIndex", which Windows guarantees is
// unique per physical file while it is open. ok=false means it could not be
// determined (permission denied, path too long, FAT32 without stable IDs);
// the caller then treats the path as a distinct file, the safe default.
//
// The handle is opened with no read/write access (0) - we only ask for
// metadata - and with full sharing flags so we never block another program
// that has the file open. FILE_FLAG_OPEN_REPARSE_POINT means "if this is a
// link, look at the link itself, don't follow it"; FILE_FLAG_BACKUP_SEMANTICS
// is required by Windows to open folders and is harmless for files.
func fileKey(path string) (key string, ok bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &d); err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d", d.VolumeSerialNumber, d.FileIndexHigh, d.FileIndexLow), true
}
