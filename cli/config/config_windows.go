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
	"regexp"

	"github.com/google/glazier/go/registry"
	"github.com/google/winops/powershell"
)

var (
	// Dependency injection for testing.
	powershellCmd = powershell.Command

	// IsElevatedCmd injects the command to determine the elevation state of the
	// user context.
	IsElevatedCmd      = isAdmin
	funcUSBPermissions = HasWritePermissions

	// Regex for powershell handling.
	regExAdmin = regexp.MustCompile(`S-1-5-32-544`)

	denyWriteRegKey = `SOFTWARE\Policies\Microsoft\Windows\RemovableStorageDevices\{53f5630d-b6bf-11d0-94f2-00a0c91efb8b}`
)

// isAdmin determines if the current user is running the binary with elevated
// permissions on Windows.
func isAdmin() (bool, error) {
	out, err := powershellCmd(fmt.Sprintf("powershell.exe -File '%s'", adminScriptPath), nil, &powershell.PSConfig{ErrAction: powershell.Stop})
	if err != nil {
		return false, fmt.Errorf("%w: %v", errElevation, err)
	}
	if regExAdmin.Match(out) {
		return true, nil
	}
	return false, nil
}

// HasWritePermissions determines if the local machine is blocked from writing to removable media via policy.
func HasWritePermissions() error {
	v, err := registry.GetInteger(denyWriteRegKey, "Deny_Write")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	if v == 1 {
		return fmt.Errorf("removable media write prevented by policy")
	}
	return nil
}
