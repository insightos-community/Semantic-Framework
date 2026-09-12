//go:build darwin && cgo

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package bootstrap

/*
#include <libproc.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

func managedProcessExecutable(pid int) (string, error) {
	var path [C.PROC_PIDPATHINFO_MAXSIZE]C.char
	n, err := C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.uint32_t(len(path)))
	if n <= 0 {
		if errors.Is(err, syscall.ESRCH) {
			return "", os.ErrNotExist
		}
		if err == nil {
			err = fmt.Errorf("proc_pidpath returned no executable for PID %d", pid)
		}
		return "", err
	}
	return C.GoString(&path[0]), nil
}
