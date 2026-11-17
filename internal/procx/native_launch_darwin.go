//go:build darwin && cgo

package procx

/*
#include <errno.h>
#include <libproc.h>
#include <sys/sysctl.h>

static int native_launch_image(int pid, void *data, int capacity, int *word_size) {
    struct proc_bsdinfo info;
    if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, sizeof(info)) != sizeof(info)) return errno ? errno : EIO;
    *word_size = (info.pbi_flags & PROC_FLAG_LP64) ? 8 : 4;
    if (proc_pidpath(pid, data, capacity) <= 0) return errno ? errno : EIO;
    return 0;
}

static int native_launch_capacity(int *capacity) {
    int mib[2] = {CTL_KERN, KERN_ARGMAX};
    size_t size = sizeof(*capacity);
    if (sysctl(mib, 2, capacity, &size, NULL, 0) != 0) return errno;
    return size == sizeof(*capacity) ? 0 : EIO;
}
static int native_launch_args(int pid, void *data, size_t *size) {
    int mib[3] = {CTL_KERN, KERN_PROCARGS2, pid};
    if (sysctl(mib, 3, data, size, NULL, 0) != 0) return errno;
    return 0;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ObserveNativeImage reads only the kernel image, without requiring mutable
// argv to be readable. It does not establish a namespace or compute absence.
func ObserveNativeImage(original NativeIdentity) (string, error) {
	before, err := ObserveNativeIdentity(original.PID)
	if err != nil {
		return "", fmt.Errorf("original image generation unavailable: %w", err)
	}
	if !original.SameGeneration(before) || before.Zombie {
		return "", fmt.Errorf("original image generation unavailable")
	}
	image, _, err := readNativeImage(original.PID)
	if err != nil {
		return "", err
	}
	after, err := ObserveNativeIdentity(original.PID)
	if err != nil {
		return "", fmt.Errorf("original image generation changed during readback: %w", err)
	}
	if !original.SameGeneration(after) || after.Zombie {
		return "", fmt.Errorf("original image generation changed during readback")
	}
	return image, nil
}

func readNativeImage(pid int) (string, int, error) {
	image := make([]byte, int(C.PROC_PIDPATHINFO_MAXSIZE))
	var wordSize C.int
	if code := C.native_launch_image(C.int(pid), unsafe.Pointer(&image[0]), C.int(len(image)), &wordSize); code != 0 {
		return "", 0, fmt.Errorf("kernel launch image: %w", syscall.Errno(code))
	}
	imageEnd := 0
	for imageEnd < len(image) && image[imageEnd] != 0 {
		imageEnd++
	}
	if imageEnd == 0 || imageEnd == len(image) {
		return "", 0, fmt.Errorf("kernel launch image path incomplete")
	}
	return string(image[:imageEnd]), int(wordSize), nil
}

// ObserveNativeLaunch binds argv to the same kernel generation on both sides
// of its read. It refuses ambiguity and never returns the environment suffix.
func ObserveNativeLaunch(original NativeIdentity) (NativeLaunch, error) {
	before, err := ObserveNativeIdentity(original.PID)
	if err != nil {
		return NativeLaunch{}, fmt.Errorf("read original launch generation: %w", err)
	}
	if !original.SameGeneration(before) || before.Zombie {
		return NativeLaunch{}, fmt.Errorf("original launch generation unavailable")
	}
	image, wordSize, err := readNativeImage(original.PID)
	if err != nil {
		return NativeLaunch{}, err
	}
	var capacity C.int
	if code := C.native_launch_capacity(&capacity); code != 0 {
		return NativeLaunch{}, fmt.Errorf("kernel launch capacity: %w", syscall.Errno(code))
	}
	if capacity < 5 || capacity > 4<<20 {
		return NativeLaunch{}, fmt.Errorf("kernel launch capacity outside supported bound")
	}
	data := make([]byte, int(capacity))
	size := C.size_t(len(data))
	if code := C.native_launch_args(C.int(original.PID), unsafe.Pointer(&data[0]), &size); code != 0 {
		return NativeLaunch{}, fmt.Errorf("kernel launch read: %w", syscall.Errno(code))
	}
	if int(size) > len(data) || size < 5 {
		return NativeLaunch{}, fmt.Errorf("kernel launch size invalid")
	}
	data = data[:int(size)]
	result, err := parseNativeLaunch(data, image, wordSize)
	if err != nil {
		return NativeLaunch{}, err
	}
	after, err := ObserveNativeIdentity(original.PID)
	if err != nil {
		return NativeLaunch{}, fmt.Errorf("recheck original launch generation: %w", err)
	}
	if !original.SameGeneration(after) || after.Zombie {
		return NativeLaunch{}, fmt.Errorf("original launch generation changed during readback")
	}
	return result, nil
}
