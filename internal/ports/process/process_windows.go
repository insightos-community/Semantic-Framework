//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// No KILL_ON_JOB_CLOSE: an unconfirmed robot stop must leave the processes
// available for reconciliation, even if their supervisor exits unexpectedly.
type Tree struct {
	mu  sync.Mutex
	job windows.Handle
}

func Start(command *exec.Cmd) (_ *Tree, result error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			windows.CloseHandle(job)
		}
	}()
	// Never execute application code before the process belongs to our job.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
	if err = command.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	pid := uint32(command.Process.Pid)
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)
	if err = windows.AssignProcessToJobObject(job, handle); err != nil {
		return nil, fmt.Errorf("assign child to job: %w", err)
	}
	// Go's exec.Cmd owns/closes the primary thread handle. While the process is
	// suspended it has one primary thread; locate it using the documented Toolhelp
	// API and resume it only after assignment succeeds.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return nil, openErr
		}
		_, resumeErr := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if resumeErr != nil {
			return nil, resumeErr
		}
		return &Tree{job: job}, nil
	}
	return nil, fmt.Errorf("primary thread unavailable for child %d: %w", pid, err)
}

func (tree *Tree) Close() error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return nil
	}
	err := windows.CloseHandle(tree.job)
	tree.job = 0
	return err
}

// Terminate is forced retirement, only permitted after application stop evidence.
// Windows does not emulate SIGTERM using TerminateProcess.
func (tree *Tree) Terminate() error { return tree.Kill() }
func (tree *Tree) Kill() error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return errors.New("process tree is closed")
	}
	return windows.TerminateJobObject(tree.job, 1)
}
func (tree *Tree) Alive() (bool, error) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return false, errors.New("process tree is closed")
	}
	// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION (winnt.h), not exposed by x/sys.
	var accounting struct {
		TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
		TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
	}
	err := windows.QueryInformationJobObject(tree.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil)
	return accounting.ActiveProcesses > 0, err
}
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

func (tree *Tree) Interrupt() error { return tree.Terminate() }
