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

package list

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/subcommands"
	"github.com/google/winops/storage"
)

func TestName(t *testing.T) {
	list := &listCmd{}
	got := list.Name()
	if got == "" {
		t.Errorf("Name() got: %q, want: not empty, got", got)
	}
}

func TestSynopsis(t *testing.T) {
	list := &listCmd{}
	got := list.Synopsis()
	if got == "" {
		t.Errorf("Synopsis() got: %q, want: not empty, got", got)
	}
}

func TestUsage(t *testing.T) {
	list := &listCmd{}
	got := list.Usage()
	if got == "" {
		t.Errorf("Usage() got: %q, want: not empty, got", got)
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		desc       string
		fakeSearch func(string, uint64, uint64, bool) ([]*storage.Device, error)
		want       subcommands.ExitStatus
	}{
		{
			desc:       "search error",
			fakeSearch: func(string, uint64, uint64, bool) ([]*storage.Device, error) { return nil, fmt.Errorf("error") },
			want:       subcommands.ExitFailure,
		},
		{
			desc: "success",
			fakeSearch: func(string, uint64, uint64, bool) ([]*storage.Device, error) {
				return []*storage.Device{&storage.Device{}}, nil
			},
			want: subcommands.ExitSuccess,
		},
	}
	for _, tt := range tests {
		search = tt.fakeSearch
		list := &listCmd{}
		got := list.Execute(context.Background(), nil, nil)
		if got != tt.want {
			t.Errorf("%s: Execute() got: %d, want: %d", tt.desc, got, tt.want)
		}
	}
}
