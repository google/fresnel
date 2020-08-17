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

// +build windows

package config

import (
	"errors"
	"testing"
)

func TestIsAdmin(t *testing.T) {
	// These represent the expected of the PowerShell command.
	outIsAdmin := []byte(
		"BinaryLength AccountDomainSid Value\n" +
			"------------ ---------------- -----\n" +
			"     16                  S-1-5-32-544")
	outNotAdmin := []byte("")

	tests := []struct {
		desc      string
		fakePSCmd func(string) ([]byte, error)
		want      bool
		err       error
	}{
		{
			desc:      "powershell error",
			fakePSCmd: func(string) ([]byte, error) { return nil, errElevation },
			want:      false,
			err:       errElevation,
		},
		{
			desc:      "is not admin",
			fakePSCmd: func(string) ([]byte, error) { return outNotAdmin, nil },
			want:      false,
			err:       nil,
		},
		{
			desc:      "is admin",
			fakePSCmd: func(string) ([]byte, error) { return outIsAdmin, nil },
			want:      true,
			err:       nil,
		},
	}
	for _, tt := range tests {
		powershellCmd = tt.fakePSCmd
		got, err := isAdmin()
		if !errors.Is(err, tt.err) {
			t.Errorf("%s: isAdmin() err: %v, want err: %v", tt.desc, err, tt.err)
		}
		if got != tt.want {
			t.Errorf("%s: isAdmin() got: %t, want: %t", tt.desc, got, tt.want)
		}
	}
}
