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
	"os/exec"
	"regexp"
)

var (
	// Dependency injection for testing.
	powershellCmd = powershell

	// IsElevatedCmd injects the command to determine the elevation state of the
	// user context.
	IsElevatedCmd = isAdmin

	// Regex for powershell handling.
	regExAdmin = regexp.MustCompile(`S-1-5-32-544`)
)

// isAdmin determins if the current user is running the binary with elevated
// permissions on Windows.
func isAdmin() (bool, error) {
	psBlock := `(([System.Security.Principal.WindowsIdentity]::GetCurrent()).groups -match 'S-1-5-32-544')`
	out, err := powershellCmd(psBlock)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errElevation, err)
	}
	if regExAdmin.Match(out) {
		return true, nil
	}
	return false, nil
}

// Powershell represents the OS command used to run a powershell cmdlet on
// Windows.
func powershell(psBlock string) ([]byte, error) {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", psBlock).CombinedOutput()
	if err != nil {
		return []byte{}, fmt.Errorf(`exec.Command("powershell.exe", "-NoProfile", "-Command", %s) command returned: %q: %v`, psBlock, out, err)
	}
	return out, nil
}
