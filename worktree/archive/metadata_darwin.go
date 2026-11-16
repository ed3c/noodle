//go:build darwin && cgo

package archive

/*
#include <sys/types.h>
#include <sys/stat.h>
#include <sys/xattr.h>
#include <sys/acl.h>
#include <stdlib.h>
#include <errno.h>
static int file_flags(const char *p, unsigned int *flags) {
 struct stat s; if (lstat(p,&s)) return -1; *flags=s.st_flags; return 0;
}
*/
import "C"

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"reflect"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
)

func FileIdentity(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

func ReadMetadata(path string) (Metadata, error) {
	m := Metadata{Xattrs: map[string]string{}}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return m, err
	}
	if stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
		return m, fmt.Errorf("file ownership requires separate restoration: %s", path)
	}
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	var flags C.uint
	if rc, err := C.file_flags(p, &flags); rc != 0 {
		return m, err
	}
	if flags & ^C.uint(0x20) != 0 {
		return m, fmt.Errorf("unsupported file flags on %s: %x", path, uint32(flags))
	}
	n, err := C.listxattr(p, nil, 0, C.XATTR_NOFOLLOW)
	if n < 0 {
		return m, err
	}
	names := make([]byte, int(n))
	if n > 0 {
		got, err := C.listxattr(p, (*C.char)(unsafe.Pointer(&names[0])), C.size_t(n), C.XATTR_NOFOLLOW)
		if got != n {
			return m, errors.Join(err, errors.New("xattr names changed"))
		}
	}
	start := 0
	for i, b := range names {
		if b != 0 {
			continue
		}
		name := string(names[start:i])
		if !utf8.ValidString(name) {
			return m, errors.New("xattr name cannot be represented losslessly")
		}
		start = i + 1
		if flags&0x20 != 0 && name == "com.apple.decmpfs" {
			continue
		}
		key := C.CString(name)
		size, getErr := C.getxattr(p, key, nil, 0, 0, C.XATTR_NOFOLLOW)
		if size < 0 || size > 16*1024*1024 {
			C.free(unsafe.Pointer(key))
			return m, errors.Join(getErr, errors.New("xattr size is unavailable or excessive"))
		}
		value := make([]byte, int(size))
		var ptr unsafe.Pointer
		if size > 0 {
			ptr = unsafe.Pointer(&value[0])
		}
		got, getErr := C.getxattr(p, key, ptr, C.size_t(size), 0, C.XATTR_NOFOLLOW)
		C.free(unsafe.Pointer(key))
		if got != size {
			return m, errors.Join(getErr, errors.New("xattr value changed"))
		}
		m.Xattrs[name] = base64.StdEncoding.EncodeToString(value)
	}
	acl, err := C.acl_get_link_np(p, C.ACL_TYPE_EXTENDED)
	if acl == nil {
		if errors.Is(err, syscall.ENOENT) {
			_, statErr := os.Lstat(path)
			return m, statErr
		}
		return m, err
	}
	defer C.acl_free(unsafe.Pointer(acl))
	var size C.ssize_t
	text, err := C.acl_to_text(acl, &size)
	if text == nil {
		return m, err
	}
	defer C.acl_free(unsafe.Pointer(text))
	m.ACL = C.GoStringN(text, C.int(size))
	return m, nil
}

func CheckIndependentFile(path string) error {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink > 1 {
		return fmt.Errorf("hardlinked file has unresolved external mutation custody: %s", path)
	}
	return nil
}

func AvailableBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func ApplyMetadata(path string, m Metadata, mode *os.FileMode) error {
	old, err := ReadMetadata(path)
	if err != nil {
		return err
	}
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	for key := range old.Xattrs {
		if _, ok := m.Xattrs[key]; ok {
			continue
		}
		k := C.CString(key)
		rc, e := C.removexattr(p, k, C.XATTR_NOFOLLOW)
		C.free(unsafe.Pointer(k))
		if rc != 0 {
			return e
		}
	}
	for key, encoded := range m.Xattrs {
		value, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return err
		}
		k := C.CString(key)
		var ptr unsafe.Pointer
		if len(value) > 0 {
			ptr = unsafe.Pointer(&value[0])
		}
		rc, e := C.setxattr(p, k, ptr, C.size_t(len(value)), 0, C.XATTR_NOFOLLOW)
		C.free(unsafe.Pointer(k))
		if rc != 0 {
			return e
		}
	}
	if mode != nil {
		if err = os.Chmod(path, *mode); err != nil {
			return err
		}
	}
	if old.ACL != m.ACL {
		var acl C.acl_t
		if m.ACL == "" {
			acl = C.acl_init(0)
		} else {
			txt := C.CString(m.ACL)
			acl = C.acl_from_text(txt)
			C.free(unsafe.Pointer(txt))
		}
		if acl == nil {
			return errors.New("saved ACL cannot be parsed")
		}
		rc, e := C.acl_set_link_np(p, C.ACL_TYPE_EXTENDED, acl)
		C.acl_free(unsafe.Pointer(acl))
		if rc != 0 {
			return e
		}
	}
	actual, err := ReadMetadata(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, m) {
		return fmt.Errorf("restored metadata differs: %s", path)
	}
	return nil
}
