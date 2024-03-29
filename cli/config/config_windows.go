// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows
// +build windows

package config

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	win "golang.org/x/sys/windows"
	"github.com/google/glazier/go/registry"
)

var (

	// IsElevatedCmd injects the command to determine the elevation state of the
	// user context.
	IsElevatedCmd      = isAdmin
	funcUSBPermissions = HasWritePermissions

	denyWriteRegKey = `SOFTWARE\Policies\Microsoft\Windows\RemovableStorageDevices\{53f5630d-b6bf-11d0-94f2-00a0c91efb8b}`
)

// isAdmin determines if the current user is running the binary with elevated
// permissions on Windows.
func isAdmin() (bool, error) {

	var sid *win.SID

	// https://docs.microsoft.com/en-us/windows/win32/api/securitybaseapi/nf-securitybaseapi-checktokenmembership
	err := win.AllocateAndInitializeSid(
		&win.SECURITY_NT_AUTHORITY,
		2,
		win.SECURITY_BUILTIN_DOMAIN_RID,
		win.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid)
	if err != nil {
		return false, fmt.Errorf("sid error: %v", err)
	}

	token := win.Token(0)
	defer token.Close()

	member, err := token.IsMember(sid)
	if err != nil {
		return false, fmt.Errorf("Token Membership Error: %v", err)
	}

	// user is currently an admin
	if member {
		return true, nil
	}

	if err := runAsAdmin(); err != nil {
		return false, fmt.Errorf("runAsAdmin Error: %v", err)
	}

	return false, errElevation
}

// If not run in an Admin session, try to re-open in one.
func runAsAdmin() error {
	verb := "runas"
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	args := strings.Join(os.Args[1:], " ")

	verbPtr, _ := syscall.UTF16PtrFromString(verb)
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	argPtr, _ := syscall.UTF16PtrFromString(args)

	var showCmd int32 = 1 //SW_NORMAL

	if err := win.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd); err != nil {
		return (err)
	}
	return nil
}

// HasWritePermissions determines if the local machine is blocked from writing to removable media via policy.
func HasWritePermissions() error {
	v, err := registry.GetInteger(denyWriteRegKey, "Deny_Write")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	if v == 1 {
		return ErrWritePerms
	}
	return nil
}
