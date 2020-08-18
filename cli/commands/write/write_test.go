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

package write

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"flag"
	"github.com/google/fresnel/cli/config"
	"github.com/google/fresnel/cli/console"
	"github.com/google/fresnel/cli/installer"
	"github.com/google/subcommands"
	"github.com/google/winops/storage"
)

func TestName(t *testing.T) {
	name := "testCmd"
	write := &writeCmd{name: name}
	got := write.Name()
	if got != name {
		t.Errorf("Name() got: %q, want: %q", got, name)
	}
}

func TestSynopsis(t *testing.T) {
	write := &writeCmd{name: "testCmd"}
	got := write.Synopsis()
	if got == "" {
		t.Errorf("Synopsis() got: %q, want: not empty", got)
	}
}

func TestUsage(t *testing.T) {
	write := &writeCmd{name: "testCmd"}
	got := write.Usage()
	if got == "" {
		t.Errorf("Usage() got: %q, want: not empty", got)
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		desc    string
		cmd     *writeCmd
		args    []string // Commandline arguments to be passed
		execute func(c *writeCmd, f *flag.FlagSet) error
		logDir  string
		verbose bool // Expected state of console.Verbose
		want    subcommands.ExitStatus
	}{
		{
			desc:   "no devices specified",
			cmd:    &writeCmd{},
			logDir: filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			want:   subcommands.ExitUsageError,
		},
		{
			desc:    "run error",
			cmd:     &writeCmd{},
			args:    []string{"1"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return errors.New("test") },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			want:    subcommands.ExitFailure,
		},
		{
			desc:    "success",
			cmd:     &writeCmd{},
			args:    []string{"1"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return nil },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			verbose: false,
			want:    subcommands.ExitSuccess,
		},
		{
			desc:    "verbose it set with --info",
			cmd:     &writeCmd{},
			args:    []string{"--info", "1"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return nil },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			verbose: true,
			want:    subcommands.ExitSuccess,
		},
		{
			desc:    "verbose it set with --verbose",
			cmd:     &writeCmd{},
			args:    []string{"--verbose", "1"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return nil },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			verbose: true,
			want:    subcommands.ExitSuccess,
		},
		{
			desc:    "verbose it set with --v=2",
			cmd:     &writeCmd{},
			args:    []string{"--v=2", "1"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return nil },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			verbose: true,
			want:    subcommands.ExitSuccess,
		},
		{
			desc:    "no drives specified but --all flag specified",
			cmd:     &writeCmd{},
			args:    []string{"--all"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return nil },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			verbose: false,
			want:    subcommands.ExitSuccess,
		},
		{
			desc:    "both --all and --show_fixed specified",
			cmd:     &writeCmd{},
			args:    []string{"--all", "--show_fixed"},
			execute: func(c *writeCmd, f *flag.FlagSet) error { return nil },
			logDir:  filepath.Dir(filepath.Join(os.TempDir(), binaryName)),
			verbose: false,
			want:    subcommands.ExitFailure,
		},
	}
	for _, tt := range tests {
		// Generate the logDir if specified
		if tt.logDir != "" {
			if err := os.MkdirAll(tt.logDir, 0755); err != nil {
				t.Errorf("%s: os.MkDirAll(%s, 0755) returned %v", tt.desc, tt.logDir, err)
			}
		}
		// Reset console.Verbose and perform substitutions.
		console.Verbose = false
		write := tt.cmd
		execute = tt.execute

		// Generate the flagSet and set Flags
		flagSet := flag.NewFlagSet("test", flag.ContinueOnError)
		write.SetFlags(flagSet)
		if err := flagSet.Parse(tt.args); err != nil {
			t.Errorf("%s: flagSet.Parse(%v) returned %v", tt.desc, tt.args, err)
		}

		// Get results
		got := write.Execute(context.Background(), flagSet, nil)
		if got != tt.want {
			t.Errorf("%s: Execute() got: %d, want: %d", tt.desc, got, tt.want)
		}
		if console.Verbose != tt.verbose {
			t.Errorf("%s: console.Verbose = %t, want: %t", tt.desc, console.Verbose, tt.verbose)
		}
	}
}

// fakeDevice inherits all members of storage.Device through embedding.
// Unimplemented members send a clear signal during tests because they will
// panic if called, allowing us to implement only the minimum set of members
// required.
type fakeDevice struct {
	// storage.Device is embedded, fakeDevice inherits all its members.
	storage.Device

	id string

	dmErr    error
	ejectErr error
	partErr  error
	selErr   error
	wipeErr  error
	writeErr error
}

func (f *fakeDevice) Dismount() error {
	return f.dmErr
}

func (f *fakeDevice) Eject() error {
	return f.ejectErr
}

func (f *fakeDevice) Identifier() string {
	return f.id
}

func (f *fakeDevice) Partition(label string) error {
	return f.partErr
}

func (f *fakeDevice) SelectPartition(uint64, storage.FileSystem) (*storage.Partition, error) {
	return nil, f.partErr
}

func (f *fakeDevice) Wipe() error {
	return f.wipeErr
}

// fakeInstaller inherits all members of installer.Installer through embedding.
// Unimplemented members send a clear signal during tests because they will
// panic if called, allowing us to implement only the minimum set of members
// required.
type fakeInstaller struct {
	// installer.Installer is embedded, fakeInstaller inherits all its members.
	installer.Installer

	prepErr error // Returned when Prepare() is called.
	provErr error // Returned when Provision() is called.
	retErr  error // Returned when Retrieve() is called.
	finErr  error // Returned when Finalize() is called.
}

func (i *fakeInstaller) Prepare(installer.Device) error {
	return i.prepErr
}

func (i *fakeInstaller) Provision(installer.Device) error {
	return i.provErr
}

func (i *fakeInstaller) Retrieve() error {
	return i.retErr
}

func (i *fakeInstaller) Finalize([]installer.Device) error {
	return i.finErr
}

func TestRun(t *testing.T) {
	tests := []struct {
		desc          string
		cmd           *writeCmd
		isElevatedCmd func() (bool, error)
		searchCmd     func(string, uint64, uint64, bool) ([]installer.Device, error)
		newInstCmd    func(config installer.Configuration) (imageInstaller, error)
		args          []string // Commandline arguments to be passed
		want          error
	}{
		{
			desc:          "config.New error",
			cmd:           &writeCmd{},
			isElevatedCmd: func() (bool, error) { return false, nil },
			want:          errConfig,
		},
		{
			desc:          "elevation error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return false, nil },
			want:          errElevation,
		},
		{
			desc:          "search failure",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd:     func(string, uint64, uint64, bool) ([]installer.Device, error) { return nil, errors.New("error") },
			want:          errSearch,
		},
		{
			desc:          "unsuitable device",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd:     func(string, uint64, uint64, bool) ([]installer.Device, error) { return nil, nil },
			args:          []string{"4"},
			want:          errDevice,
		},
		{
			desc:          "new.Installer error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) { return nil, errors.New("") },
			args:       []string{"--confirm=false", "1"},
			want:       errInstaller,
		},
		{
			desc:          "retrieve error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{retErr: errors.New("error")}, nil
			},
			args: []string{"--confirm=false", "1"},
			want: errRetrieve,
		},
		{
			desc:          "prepare error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{prepErr: errors.New("error")}, nil
			},
			args: []string{"--confirm=false", "1"},
			want: errPrepare,
		},
		{
			desc:          "provision error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{provErr: errors.New("error")}, nil
			},
			args: []string{"--confirm=false", "1"},
			want: errProvision,
		},
		{
			desc:          "finalize error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{finErr: errors.New("error")}, nil
			},
			args: []string{"--confirm=false", "1"},
			want: errFinalize,
		},
		{
			desc:          "finalize error",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{finErr: errors.New("error")}, nil
			},
			args: []string{"--confirm=false", "1"},
			want: errFinalize,
		},
		{
			desc:          "success",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{}, nil
			},
			args: []string{"--confirm=false", "1"},
			want: nil,
		},
		{
			desc:          "--all flag provided",
			cmd:           &writeCmd{distro: "windows"},
			isElevatedCmd: func() (bool, error) { return true, nil },
			searchCmd: func(string, uint64, uint64, bool) ([]installer.Device, error) {
				return []installer.Device{&fakeDevice{id: "1"}, &fakeDevice{id: "2"}}, nil
			},
			newInstCmd: func(config installer.Configuration) (imageInstaller, error) {
				return &fakeInstaller{}, nil
			},
			args: []string{"--confirm=false", "--all"},
			want: nil,
		},
	}
	for _, tt := range tests {
		// Perofrm substitutions, generate the flagSet and set Flags
		config.IsElevatedCmd = tt.isElevatedCmd
		search = tt.searchCmd
		newInstaller = tt.newInstCmd

		flagSet := flag.NewFlagSet("test", flag.ContinueOnError)
		write := tt.cmd
		write.SetFlags(flagSet)
		if err := flagSet.Parse(tt.args); err != nil {
			t.Errorf("%s: flagSet.Parse(%v) returned %v", tt.desc, tt.args, err)
		}

		// Get results
		got := run(write, flagSet)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: run() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}
