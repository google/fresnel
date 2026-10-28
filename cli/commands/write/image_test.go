// Copyright 2026 Google LLC
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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"flag"
	"github.com/google/fresnel/cli/config"
	"github.com/google/fresnel/cli/console"
	"github.com/google/fresnel/cli/installer"
	"github.com/google/deck"
	"github.com/google/subcommands"
	"github.com/google/winops/storage"
)

const (
	testLoopID         = "loop7"
	testIno     uint64 = 4242
	testISOSize        = 10 << 20
)

var testTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeClock replaces imageNow and imageSleep so that polling loops finish
// without real delays.
type fakeClock struct {
	now    time.Time
	sleeps int
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.now = c.now.Add(d)
	c.sleeps++
}

// stubImageGlobals saves every dependency injection used by image mode and
// restores them when the test completes. Dependencies that change the system
// are replaced with fakes that fail the test when they are called without
// being stubbed, so that no test can run losetup or sudo for real.
func stubImageGlobals(t *testing.T) *fakeClock {
	t.Helper()
	oldExecuteImage := executeImage
	oldExecute := execute
	oldNow := imageNow
	oldSleep := imageSleep
	oldGetenv := imageGetenv
	oldGeteuid := imageGeteuid
	oldUserHomeDir := imageUserHomeDir
	oldLookupHome := imageLookupHome
	oldLookPath := imageLookPath
	oldStat := imageStat
	oldRunCmd := imageRunCmd
	oldFreeBytes := imageFreeBytes
	oldFileInode := imageFileInode
	oldNewDevice := imageNewDevice
	oldPartitionID := imagePartitionID
	oldPartitionTimeout := imagePartitionTimeout
	oldDetachTimeout := imageDetachTimeout
	oldPollInterval := imagePollInterval
	oldToolFolders := imageToolSearchFolders
	oldNewInstaller := newInstaller
	oldUSB := funcUSBPermissions
	oldIsElevated := config.IsElevatedCmd
	oldVerbose := console.Verbose
	t.Cleanup(func() {
		executeImage = oldExecuteImage
		execute = oldExecute
		imageNow = oldNow
		imageSleep = oldSleep
		imageGetenv = oldGetenv
		imageGeteuid = oldGeteuid
		imageUserHomeDir = oldUserHomeDir
		imageLookupHome = oldLookupHome
		imageLookPath = oldLookPath
		imageStat = oldStat
		imageRunCmd = oldRunCmd
		imageFreeBytes = oldFreeBytes
		imageFileInode = oldFileInode
		imageNewDevice = oldNewDevice
		imagePartitionID = oldPartitionID
		imagePartitionTimeout = oldPartitionTimeout
		imageDetachTimeout = oldDetachTimeout
		imagePollInterval = oldPollInterval
		imageToolSearchFolders = oldToolFolders
		newInstaller = oldNewInstaller
		funcUSBPermissions = oldUSB
		config.IsElevatedCmd = oldIsElevated
		console.Verbose = oldVerbose
	})

	clock := &fakeClock{now: testTime}
	imageNow = clock.Now
	imageSleep = clock.Sleep
	imageGetenv = func(string) string { return "" }
	imageGeteuid = func() int { return 1000 }
	imageUserHomeDir = func() (string, error) { return "", errors.New("imageUserHomeDir is not stubbed") }
	imageLookupHome = func(uid string) (string, error) {
		return "", fmt.Errorf("imageLookupHome(%q) is not stubbed", uid)
	}
	imageRunCmd = func(name string, args ...string) (string, error) {
		t.Errorf("imageRunCmd(%q, %q) was called without a stub", name, args)
		return "", errors.New("imageRunCmd is not stubbed")
	}
	imageNewDevice = func(id string) (installer.Device, error) {
		t.Errorf("imageNewDevice(%q) was called without a stub", id)
		return nil, errors.New("imageNewDevice is not stubbed")
	}
	newInstaller = func(installer.Configuration) (imageInstaller, error) {
		t.Error("newInstaller was called without a stub")
		return nil, errors.New("newInstaller is not stubbed")
	}
	funcUSBPermissions = func() error { return nil }
	config.IsElevatedCmd = func() (bool, error) { return true, nil }
	return clock
}

// requireImageMode skips tests of behavior that only exists on Linux.
func requireImageMode(t *testing.T) {
	t.Helper()
	if !imageModeSupported {
		t.Skipf("--image_file is not supported on %s", runtime.GOOS)
	}
}

// newFlagSet registers the write flags on c and parses args.
func newFlagSet(t *testing.T, c *writeCmd, args ...string) (*flag.FlagSet, error) {
	t.Helper()
	f := flag.NewFlagSet("test", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	c.SetFlags(f)
	return f, f.Parse(args)
}

// loopJSON returns 'losetup --list --json' output for the given rows.
func loopJSON(rows ...map[string]any) string {
	b, err := json.Marshal(map[string]any{"loopdevices": rows})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// blockJSON returns 'lsblk --json' output for the given rows.
func blockJSON(rows ...map[string]any) string {
	b, err := json.Marshal(map[string]any{"blockdevices": rows})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// with returns a copy of m with key set to v.
func with(m map[string]any, key string, v any) map[string]any {
	c := maps.Clone(m)
	c[key] = v
	return c
}

func TestImageFileFlag(t *testing.T) {
	tests := []struct {
		desc        string
		args        []string
		wantEnabled bool
		wantPath    string
		wantString  string
		wantArgs    []string
		wantErr     bool
	}{
		{
			desc: "unset",
		},
		{
			desc:        "bare",
			args:        []string{"--image_file"},
			wantEnabled: true,
			wantString:  "true",
		},
		{
			desc:        "path",
			args:        []string{"--image_file=/tmp/out.bin"},
			wantEnabled: true,
			wantPath:    "/tmp/out.bin",
			wantString:  "/tmp/out.bin",
		},
		{
			desc:        "explicit true",
			args:        []string{"--image_file=true"},
			wantEnabled: true,
			wantString:  "true",
		},
		{
			desc: "explicit false",
			args: []string{"--image_file=false"},
		},
		{
			desc: "later false wins",
			args: []string{"--image_file=/tmp/out.bin", "--image_file=false"},
		},
		{
			desc:        "boolean-like path with a prefix",
			args:        []string{"--image_file=./true"},
			wantEnabled: true,
			wantPath:    "./true",
			wantString:  "./true",
		},
		{
			desc:        "bare does not consume the next argument",
			args:        []string{"--image_file", "/tmp/out.bin"},
			wantEnabled: true,
			wantString:  "true",
			wantArgs:    []string{"/tmp/out.bin"},
		},
		{
			desc:    "empty value",
			args:    []string{"--image_file="},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			c := &writeCmd{}
			f, err := newFlagSet(t, c, tt.args...)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = nil, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tt.args, err)
			}
			if c.image.enabled != tt.wantEnabled || c.image.path != tt.wantPath {
				t.Errorf("Parse(%q) set (enabled: %t, path: %q), want (%t, %q)", tt.args, c.image.enabled, c.image.path, tt.wantEnabled, tt.wantPath)
			}
			if got := c.image.String(); got != tt.wantString {
				t.Errorf("String() = %q, want %q", got, tt.wantString)
			}
			if got := f.Args(); !slices.Equal(got, tt.wantArgs) {
				t.Errorf("Args() = %q, want %q", got, tt.wantArgs)
			}
		})
	}
}

func TestImageFileFlagValue(t *testing.T) {
	var nilFlag *imageFileFlag
	if got := nilFlag.String(); got != "" {
		t.Errorf("String() on a nil flag = %q, want empty", got)
	}
	v := &imageFileFlag{}
	if !v.IsBoolFlag() {
		t.Error("IsBoolFlag() = false, want true")
	}
	if got := v.Get(); got != "" {
		t.Errorf("Get() on a zero flag = %v, want empty", got)
	}
	if err := v.Set("/tmp/out.bin"); err != nil {
		t.Fatalf("Set(%q) = %v, want nil", "/tmp/out.bin", err)
	}
	if got := v.Get(); got != "/tmp/out.bin" {
		t.Errorf("Get() = %v, want %q", got, "/tmp/out.bin")
	}
	if err := v.Set(""); err == nil {
		t.Error("Set(\"\") = nil, want error")
	}
}

func TestValidateImageMode(t *testing.T) {
	tests := []struct {
		desc     string
		cmd      *writeCmd
		args     []string
		wantPath string
		wantErr  bool
	}{
		{
			desc: "bare",
			args: []string{"--image_file"},
		},
		{
			desc:     "bare with one positional path",
			args:     []string{"--image_file", "/tmp/out.bin"},
			wantPath: "/tmp/out.bin",
		},
		{
			desc:     "flag path",
			args:     []string{"--image_file=/tmp/out.bin"},
			wantPath: "/tmp/out.bin",
		},
		{
			desc:    "two positionals",
			args:    []string{"--image_file", "/tmp/a.bin", "/tmp/b.bin"},
			wantErr: true,
		},
		{
			desc:    "flag path and a positional",
			args:    []string{"--image_file=/tmp/a.bin", "sdb"},
			wantErr: true,
		},
		{
			desc:    "flag after the positional path",
			args:    []string{"--image_file", "/tmp/a.bin", "--warning=false"},
			wantErr: true,
		},
		{
			desc:    "update",
			cmd:     &writeCmd{update: true},
			args:    []string{"--image_file"},
			wantErr: true,
		},
		{
			desc:    "all",
			args:    []string{"--all", "--image_file"},
			wantErr: true,
		},
		{
			desc:    "all shorthand",
			args:    []string{"--a", "--image_file"},
			wantErr: true,
		},
		{
			desc:    "show fixed",
			args:    []string{"--show_fixed", "--image_file"},
			wantErr: true,
		},
		{
			desc:    "eject",
			args:    []string{"--eject", "--image_file"},
			wantErr: true,
		},
		{
			desc:    "no dismount",
			args:    []string{"--dismount=false", "--image_file"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			c := tt.cmd
			if c == nil {
				c = &writeCmd{}
			}
			f, err := newFlagSet(t, c, tt.args...)
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tt.args, err)
			}
			err = validateImageMode(c, f)
			if tt.wantErr || !imageModeSupported {
				if !errors.Is(err, errImageUsage) {
					t.Errorf("validateImageMode(%q) = %v, want %v", tt.args, err, errImageUsage)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateImageMode(%q) = %v, want nil", tt.args, err)
			}
			if c.image.path != tt.wantPath {
				t.Errorf("validateImageMode(%q) set path %q, want %q", tt.args, c.image.path, tt.wantPath)
			}
		})
	}
}

func TestExecuteImageMode(t *testing.T) {
	tests := []struct {
		desc      string
		cmd       *writeCmd
		args      []string
		imageErr  error
		want      subcommands.ExitStatus
		wantImage bool
		wantPath  string
	}{
		{
			desc:      "bare flag without devices",
			args:      []string{"--image_file"},
			want:      subcommands.ExitSuccess,
			wantImage: true,
		},
		{
			desc:      "positional path",
			args:      []string{"--image_file", "/tmp/out.bin"},
			want:      subcommands.ExitSuccess,
			wantImage: true,
			wantPath:  "/tmp/out.bin",
		},
		{
			desc:      "flag path",
			args:      []string{"--image_file=/tmp/out.bin"},
			want:      subcommands.ExitSuccess,
			wantImage: true,
			wantPath:  "/tmp/out.bin",
		},
		{
			desc:      "image error",
			args:      []string{"--image_file"},
			imageErr:  errors.New("injected"),
			want:      subcommands.ExitFailure,
			wantImage: true,
		},
		{
			desc: "two positionals",
			args: []string{"--image_file", "/tmp/a.bin", "/tmp/b.bin"},
			want: subcommands.ExitUsageError,
		},
		{
			desc: "all",
			args: []string{"--all", "--image_file"},
			want: subcommands.ExitUsageError,
		},
		{
			desc: "update",
			cmd:  &writeCmd{name: "update", distro: "windows", track: "stable", update: true},
			args: []string{"--image_file"},
			want: subcommands.ExitUsageError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			console.Verbose = false
			execute = func(*writeCmd, *flag.FlagSet) error {
				t.Error("execute was called in image mode")
				return nil
			}
			var gotImage bool
			var gotPath string
			executeImage = func(_ context.Context, c *writeCmd, _ *flag.FlagSet) error {
				gotImage = true
				gotPath = c.image.path
				return tt.imageErr
			}
			c := tt.cmd
			if c == nil {
				c = &writeCmd{name: "windows", distro: "windows", track: "stable"}
			}
			f, err := newFlagSet(t, c, tt.args...)
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tt.args, err)
			}
			want, wantImage := tt.want, tt.wantImage
			if !imageModeSupported {
				want, wantImage = subcommands.ExitUsageError, false
			}
			if got := c.Execute(context.Background(), f); got != want {
				t.Errorf("Execute(%q) = %v, want %v", tt.args, got, want)
			}
			if gotImage != wantImage {
				t.Errorf("Execute(%q) called executeImage: %t, want %t", tt.args, gotImage, wantImage)
			}
			if wantImage && gotPath != tt.wantPath {
				t.Errorf("Execute(%q) passed path %q, want %q", tt.args, gotPath, tt.wantPath)
			}
		})
	}
}

func TestExecuteDeviceModeSkipsImage(t *testing.T) {
	stubImageGlobals(t)
	console.Verbose = false
	executeImage = func(context.Context, *writeCmd, *flag.FlagSet) error {
		t.Error("executeImage was called without --image_file")
		return nil
	}
	var calls int
	execute = func(*writeCmd, *flag.FlagSet) error {
		calls++
		return nil
	}

	c := &writeCmd{name: "windows", distro: "windows", track: "stable"}
	f, err := newFlagSet(t, c, "1")
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if got := c.Execute(context.Background(), f); got != subcommands.ExitSuccess {
		t.Errorf("Execute(1) = %v, want %v", got, subcommands.ExitSuccess)
	}

	c = &writeCmd{name: "windows", distro: "windows", track: "stable"}
	f, err = newFlagSet(t, c)
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if got := c.Execute(context.Background(), f); got != subcommands.ExitUsageError {
		t.Errorf("Execute() without devices = %v, want %v", got, subcommands.ExitUsageError)
	}
	if calls != 1 {
		t.Errorf("execute was called %d times, want 1", calls)
	}
}

func TestExecuteLastLine(t *testing.T) {
	requireImageMode(t)
	tests := []struct {
		desc string
		args []string
		want string
	}{
		{desc: "image mode", args: []string{"--image_file=/tmp/out.bin"}, want: "Image file: /tmp/out.bin"},
		{desc: "device mode", args: []string{"1"}, want: binaryName + " completed successfully."},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			console.Verbose = false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var gotCtx context.Context
			executeImage = func(ctx context.Context, c *writeCmd, _ *flag.FlagSet) error {
				gotCtx = ctx
				console.Printf("\nImage file: %s\n", c.image.path)
				return nil
			}
			execute = func(*writeCmd, *flag.FlagSet) error { return nil }
			c := &writeCmd{name: "windows", distro: "windows", track: "stable"}
			f, err := newFlagSet(t, c, tt.args...)
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tt.args, err)
			}
			var got subcommands.ExitStatus
			out := captureStdout(t, func() { got = c.Execute(ctx, f) })
			if got != subcommands.ExitSuccess {
				t.Fatalf("Execute(%q) = %v, want %v", tt.args, got, subcommands.ExitSuccess)
			}
			if last := lastLine(out); last != tt.want {
				t.Errorf("Execute(%q) last output line = %q, want %q; output:\n%s", tt.args, last, tt.want, out)
			}
			if c.image.enabled && gotCtx != ctx {
				t.Error("Execute() did not pass its context to executeImage")
			}
		})
	}
}

func TestUsageDocumentsImageFile(t *testing.T) {
	got := (&writeCmd{name: "windows"}).Usage()
	for _, want := range []string{"--image_file[=PATH]", "windows --image_file"} {
		if !strings.Contains(got, want) {
			t.Errorf("Usage() does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "%!") {
		t.Errorf("Usage() contains a formatting error:\n%s", got)
	}
}

func TestComputeImageSize(t *testing.T) {
	const gib = uint64(1) << 30
	usable := map[int]uint64{}
	for _, d := range usbDriveClasses {
		usable[d.nominalGB] = d.usable
	}
	tests := []struct {
		desc     string
		payload  uint64
		wantGB   int
		wantSize uint64 // Zero means that only the invariants are checked.
	}{
		{desc: "empty payload uses the floor", payload: 0, wantGB: 4, wantSize: 1548 * oneMiB},
		{desc: "small payload uses the floor", payload: 100 * oneMiB, wantGB: 4, wantSize: 1548 * oneMiB},
		{desc: "one byte under the floor", payload: gib - 1, wantGB: 4, wantSize: 1548 * oneMiB},
		{desc: "floor", payload: gib, wantGB: 4, wantSize: 1548 * oneMiB},
		{desc: "one byte over the floor", payload: gib + 1, wantGB: 4},
		{desc: "5.5GiB fits an 8GB drive", payload: 11 * gib / 2, wantGB: 8},
		{desc: "headroom is capped at the 8GB class", payload: 68 * gib / 10, wantGB: 8, wantSize: 7_400_000_000 / oneMiB * oneMiB},
		{desc: "7.2GiB needs a 16GB drive", payload: 72 * gib / 10, wantGB: 16},
		{desc: "largest class", payload: 110_000_000_000, wantGB: 128, wantSize: 118_400_000_000 / oneMiB * oneMiB},
		{desc: "larger than every class", payload: 118_400_000_001, wantGB: 0},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			size, gb := computeImageSize(tt.payload)
			if gb != tt.wantGB {
				t.Errorf("computeImageSize(%d) class = %dGB, want %dGB", tt.payload, gb, tt.wantGB)
			}
			if tt.wantSize != 0 && size != tt.wantSize {
				t.Errorf("computeImageSize(%d) size = %d, want %d", tt.payload, size, tt.wantSize)
			}
			floor := max(tt.payload, gib)
			hardMin := roundUpMiB(floor + floor/256 + imageLayoutOverhead)
			if size < hardMin {
				t.Errorf("computeImageSize(%d) size = %d, want at least %d", tt.payload, size, hardMin)
			}
			if size%oneMiB != 0 {
				t.Errorf("computeImageSize(%d) size = %d, want a multiple of 1MiB", tt.payload, size)
			}
			if gb > 0 && size > usable[gb] {
				t.Errorf("computeImageSize(%d) size = %d, want at most %d for a %dGB drive", tt.payload, size, usable[gb], gb)
			}
		})
	}
}

func TestDefaultImageName(t *testing.T) {
	want := binaryName + "-windows-stable-20260102-030405.bin"
	if got := defaultImageName("windows", "stable", testTime); got != want {
		t.Errorf("defaultImageName() = %q, want %q", got, want)
	}
}

func TestResolveImagePath(t *testing.T) {
	requireImageMode(t)
	name := defaultImageName("windows", "stable", testTime)
	tests := []struct {
		desc      string
		arg       string
		euid      int
		env       map[string]string
		homeErr   error
		lookupErr error
		want      string
		wantErr   bool
	}{
		{
			desc: "default for a normal user",
			euid: 1000,
			want: "<home>/" + name,
		},
		{
			desc: "SUDO_UID is ignored for a normal user",
			euid: 1000,
			env:  map[string]string{"SUDO_UID": "1234"},
			want: "<home>/" + name,
		},
		{
			desc: "default under sudo",
			env:  map[string]string{"SUDO_UID": "1234"},
			want: "<sudohome>/" + name,
		},
		{
			desc:    "root without sudo",
			wantErr: true,
		},
		{
			desc:    "root with SUDO_UID of root",
			env:     map[string]string{"SUDO_UID": "0"},
			wantErr: true,
		},
		{
			desc:      "sudo home lookup failure",
			env:       map[string]string{"SUDO_UID": "1234"},
			lookupErr: errors.New("no such user"),
			wantErr:   true,
		},
		{
			desc:    "home folder failure",
			euid:    1000,
			homeErr: errors.New("no HOME"),
			wantErr: true,
		},
		{
			desc: "existing folder",
			arg:  "<dir>",
			euid: 1000,
			want: "<dir>/" + name,
		},
		{
			desc: "file in an existing folder",
			arg:  "<dir>/out.bin",
			euid: 1000,
			want: "<dir>/out.bin",
		},
		{
			desc: "path is cleaned",
			arg:  "<dir>/sub/../out.bin",
			euid: 1000,
			want: "<dir>/out.bin",
		},
		{
			desc:    "missing parent folder",
			arg:     "<dir>/missing/out.bin",
			euid:    1000,
			wantErr: true,
		},
		{
			desc:    "parent is a file",
			arg:     "<dir>/file.txt/out.bin",
			euid:    1000,
			wantErr: true,
		},
		{
			desc: "tilde",
			arg:  "~",
			euid: 1000,
			want: "<home>/" + name,
		},
		{
			desc: "tilde path",
			arg:  "~/out.bin",
			euid: 1000,
			want: "<home>/out.bin",
		},
		{
			desc: "tilde path under sudo",
			arg:  "~/out.bin",
			env:  map[string]string{"SUDO_UID": "1234"},
			want: "<sudohome>/out.bin",
		},
		{
			desc:    "tilde path as root without sudo",
			arg:     "~/out.bin",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			dir, home, sudoHome := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "file.txt"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			expand := func(s string) string {
				return strings.NewReplacer("<dir>", dir, "<home>", home, "<sudohome>", sudoHome).Replace(s)
			}
			imageGeteuid = func() int { return tt.euid }
			imageGetenv = func(k string) string { return tt.env[k] }
			imageUserHomeDir = func() (string, error) { return home, tt.homeErr }
			imageLookupHome = func(uid string) (string, error) {
				if uid != "1234" {
					t.Errorf("imageLookupHome(%q), want uid 1234", uid)
				}
				return sudoHome, tt.lookupErr
			}

			arg := expand(tt.arg)
			got, err := resolveImagePath(arg, "windows", "stable")
			if tt.wantErr {
				if !errors.Is(err, errImageFile) {
					t.Errorf("resolveImagePath(%q) = (%q, %v), want %v", arg, got, err, errImageFile)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveImagePath(%q) = %v, want nil", arg, err)
			}
			if want := expand(tt.want); got != want {
				t.Errorf("resolveImagePath(%q) = %q, want %q", arg, got, want)
			}
		})
	}
}

func TestInvokingOwner(t *testing.T) {
	tests := []struct {
		desc    string
		euid    int
		env     map[string]string
		wantUID int
		wantGID int
		wantOK  bool
		wantErr bool
	}{
		{desc: "normal user", euid: 1000, env: map[string]string{"SUDO_UID": "1234", "SUDO_GID": "5678"}},
		{desc: "root without sudo"},
		{desc: "sudo from root", env: map[string]string{"SUDO_UID": "0", "SUDO_GID": "0"}},
		{desc: "sudo", env: map[string]string{"SUDO_UID": "1234", "SUDO_GID": "5678"}, wantUID: 1234, wantGID: 5678, wantOK: true},
		{desc: "invalid SUDO_UID", env: map[string]string{"SUDO_UID": "user", "SUDO_GID": "5678"}, wantErr: true},
		{desc: "missing SUDO_GID", env: map[string]string{"SUDO_UID": "1234"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			imageGeteuid = func() int { return tt.euid }
			imageGetenv = func(k string) string { return tt.env[k] }
			uid, gid, ok, err := invokingOwner()
			if (err != nil) != tt.wantErr {
				t.Fatalf("invokingOwner() error = %v, want error: %t", err, tt.wantErr)
			}
			if uid != tt.wantUID || gid != tt.wantGID || ok != tt.wantOK {
				t.Errorf("invokingOwner() = (%d, %d, %t), want (%d, %d, %t)", uid, gid, ok, tt.wantUID, tt.wantGID, tt.wantOK)
			}
		})
	}
}

func TestLookupHomeUnknownUID(t *testing.T) {
	requireImageMode(t)
	if got, err := lookupHome("4294967290"); err == nil {
		t.Errorf("lookupHome(unknown uid) = %q, want error", got)
	}
}

func TestFindTool(t *testing.T) {
	requireImageMode(t)
	stubImageGlobals(t)
	pathDir, sbinDir := t.TempDir(), t.TempDir()
	write := func(dir, name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(pathDir, "path-tool", 0755)
	write(sbinDir, "sbin-tool", 0755)
	write(sbinDir, "plain-file", 0644)
	if err := os.Mkdir(filepath.Join(sbinDir, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	imageToolSearchFolders = []string{sbinDir}

	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "path-tool", want: filepath.Join(pathDir, "path-tool")},
		{name: "sbin-tool", want: filepath.Join(sbinDir, "sbin-tool")},
		{name: "plain-file", wantErr: true},
		{name: "folder", wantErr: true},
		{name: "missing", wantErr: true},
	}
	for _, tt := range tests {
		got, err := findTool(tt.name)
		if tt.wantErr {
			if err == nil {
				t.Errorf("findTool(%q) = %q, want error", tt.name, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("findTool(%q) = (%q, %v), want %q", tt.name, got, err, tt.want)
		}
	}
}

func TestRunCmd(t *testing.T) {
	requireImageMode(t)
	out, err := runCmd("sh", "-c", "echo out; echo warning >&2")
	if err != nil || out != "out\n" {
		t.Errorf("runCmd(success) = (%q, %v), want (%q, nil)", out, err, "out\n")
	}
	out, err = runCmd("sh", "-c", "echo partial; echo failure >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "failure") {
		t.Errorf("runCmd(failure) error = %v, want one that includes stderr", err)
	}
	if out != "partial\n" {
		t.Errorf("runCmd(failure) output = %q, want %q", out, "partial\n")
	}
}

func TestFreeBytesAndFileInode(t *testing.T) {
	requireImageMode(t)
	dir := t.TempDir()
	if _, err := freeBytes(dir); err != nil {
		t.Errorf("freeBytes(%q) = %v, want nil", dir, err)
	}
	if _, err := freeBytes(filepath.Join(dir, "missing")); err == nil {
		t.Error("freeBytes(missing) = nil, want error")
	}
	open := func(name string) *os.File {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	a, b := open("a"), open("b")
	inoA, errA := fileInode(a)
	inoB, errB := fileInode(b)
	if errA != nil || errB != nil {
		t.Fatalf("fileInode() errors = %v, %v, want nil", errA, errB)
	}
	if inoA == 0 || inoA == inoB {
		t.Errorf("fileInode() = %d and %d, want distinct non-zero inodes", inoA, inoB)
	}
}

func TestAttachLoop(t *testing.T) {
	tests := []struct {
		desc    string
		out     string
		err     error
		want    string
		wantErr bool
	}{
		{desc: "canonical", out: "/dev/loop7\n", want: "loop7"},
		{desc: "surrounding whitespace", out: "  /dev/loop12 \n", want: "loop12"},
		{desc: "empty", out: "", wantErr: true},
		{desc: "not a loop device", out: "/dev/sda\n", wantErr: true},
		{desc: "partition", out: "/dev/loop7p1\n", wantErr: true},
		{desc: "two lines", out: "/dev/loop7\n/dev/loop8\n", wantErr: true},
		{desc: "trailing garbage", out: "/dev/loop7 done\n", wantErr: true},
		{desc: "warning on stdout", out: "losetup: warning\n/dev/loop7\n", wantErr: true},
		{desc: "command error", err: errors.New("exit status 1"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			var gotCmd string
			imageRunCmd = func(name string, args ...string) (string, error) {
				gotCmd = strings.Join(append([]string{name}, args...), " ")
				return tt.out, tt.err
			}
			got, err := attachLoop("/tmp/out.bin.partial")
			if wantCmd := "sudo losetup --find --show --partscan /tmp/out.bin.partial"; gotCmd != wantCmd {
				t.Errorf("attachLoop() ran %q, want %q", gotCmd, wantCmd)
			}
			if tt.wantErr {
				if !errors.Is(err, errAttach) {
					t.Errorf("attachLoop() = (%q, %v), want %v", got, err, errAttach)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("attachLoop() = (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

func TestDetachLoop(t *testing.T) {
	tests := []struct {
		desc    string
		id      string
		err     error
		wantCmd string
		wantErr bool
	}{
		{desc: "loop device", id: "loop7", wantCmd: "sudo losetup --detach /dev/loop7"},
		{desc: "command error", id: "loop7", err: errors.New("busy"), wantCmd: "sudo losetup --detach /dev/loop7", wantErr: true},
		{desc: "disk", id: "sda", wantErr: true},
		{desc: "partition", id: "loop7p1", wantErr: true},
		{desc: "empty", id: "", wantErr: true},
		{desc: "no number", id: "loop", wantErr: true},
		{desc: "device path", id: "/dev/loop7", wantErr: true},
		{desc: "trailing space", id: "loop7 ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			var gotCmd string
			imageRunCmd = func(name string, args ...string) (string, error) {
				gotCmd = strings.Join(append([]string{name}, args...), " ")
				return "", tt.err
			}
			err := detachLoop(tt.id)
			if gotCmd != tt.wantCmd {
				t.Errorf("detachLoop(%q) ran %q, want %q", tt.id, gotCmd, tt.wantCmd)
			}
			if tt.wantErr != errors.Is(err, errDetach) || (!tt.wantErr && err != nil) {
				t.Errorf("detachLoop(%q) = %v, want error: %t", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestFlexUint(t *testing.T) {
	tests := []struct {
		in      string
		want    flexUint
		wantErr bool
	}{
		{in: `123`, want: 123},
		{in: `"123"`, want: 123},
		{in: `""`, want: 0},
		{in: `null`, want: 0},
		{in: `"abc"`, wantErr: true},
		{in: `-1`, wantErr: true},
		{in: `1.5`, wantErr: true},
	}
	for _, tt := range tests {
		var got struct {
			N flexUint `json:"n"`
		}
		err := json.Unmarshal([]byte(`{"n":`+tt.in+`}`), &got)
		if (err != nil) != tt.wantErr {
			t.Errorf("json.Unmarshal(%s) error = %v, want error: %t", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got.N != tt.want {
			t.Errorf("json.Unmarshal(%s) = %d, want %d", tt.in, got.N, tt.want)
		}
	}
}

func TestVerifyLoop(t *testing.T) {
	const (
		partial = "/tmp/out.bin.partial"
		size    = uint64(1) << 30
	)
	loop := map[string]any{"name": "/dev/loop7", "back-file": partial, "back-ino": testIno, "offset": 0, "sizelimit": 0}
	block := map[string]any{"kname": "loop7", "type": "loop", "size": size, "pkname": nil, "mountpoint": nil}
	good := loopJSON(loop)
	goodBlocks := blockJSON(block)
	tests := []struct {
		desc     string
		loopOut  string
		loopErr  error
		blockOut string
		blockErr error
		wantErr  bool
	}{
		{desc: "match", loopOut: good, blockOut: goodBlocks},
		{
			desc:     "match with numbers as strings",
			loopOut:  loopJSON(with(with(with(loop, "back-ino", "4242"), "offset", "0"), "sizelimit", "0")),
			blockOut: blockJSON(with(block, "size", strconv.FormatUint(size, 10))),
		},
		{desc: "losetup error", loopErr: errors.New("exit status 1"), wantErr: true},
		{desc: "losetup output is not JSON", loopOut: "not json", wantErr: true},
		{desc: "no loop devices", loopOut: loopJSON(), wantErr: true},
		{desc: "two loop devices", loopOut: loopJSON(loop, with(loop, "name", "/dev/loop8")), wantErr: true},
		{desc: "other loop device", loopOut: loopJSON(with(loop, "name", "/dev/loop8")), wantErr: true},
		{desc: "inode mismatch", loopOut: loopJSON(with(loop, "back-ino", testIno+1)), wantErr: true},
		{desc: "back file mismatch", loopOut: loopJSON(with(loop, "back-file", "/tmp/other.bin")), wantErr: true},
		{desc: "offset", loopOut: loopJSON(with(loop, "offset", 512)), wantErr: true},
		{desc: "size limit", loopOut: loopJSON(with(loop, "sizelimit", 1024)), wantErr: true},
		{desc: "lsblk error", loopOut: good, blockErr: errors.New("exit status 1"), wantErr: true},
		{desc: "lsblk output is not JSON", loopOut: good, blockOut: "not json", wantErr: true},
		{
			desc:     "existing partition",
			loopOut:  good,
			blockOut: blockJSON(block, map[string]any{"kname": "loop7p1", "type": "part", "size": size / 2, "pkname": "loop7"}),
			wantErr:  true,
		},
		{desc: "no block devices", loopOut: good, blockOut: blockJSON(), wantErr: true},
		{desc: "other block device", loopOut: good, blockOut: blockJSON(with(block, "kname", "loop8")), wantErr: true},
		{desc: "not a loop device", loopOut: good, blockOut: blockJSON(with(block, "type", "disk")), wantErr: true},
		{desc: "size mismatch", loopOut: good, blockOut: blockJSON(with(block, "size", size+512)), wantErr: true},
		{desc: "mounted", loopOut: good, blockOut: blockJSON(with(block, "mountpoint", "/mnt")), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			imageRunCmd = func(name string, args ...string) (string, error) {
				switch name {
				case "losetup":
					if want := []string{"--list", "--json", "--output", "NAME,BACK-FILE,BACK-INO,OFFSET,SIZELIMIT", "/dev/loop7"}; !slices.Equal(args, want) {
						t.Errorf("losetup args = %q, want %q", args, want)
					}
					return tt.loopOut, tt.loopErr
				case "lsblk":
					return tt.blockOut, tt.blockErr
				}
				t.Errorf("unexpected command %s %q", name, args)
				return "", errors.New("unexpected command")
			}
			err := verifyLoop(testLoopID, testIno, partial, size)
			if tt.wantErr {
				if !errors.Is(err, errAttach) {
					t.Errorf("verifyLoop() = %v, want %v", err, errAttach)
				}
				return
			}
			if err != nil {
				t.Errorf("verifyLoop() = %v, want nil", err)
			}
		})
	}
}

func TestIsLoopChild(t *testing.T) {
	tests := []struct {
		part string
		want bool
	}{
		{part: "loop1p1", want: true},
		{part: "loop1p12", want: true},
		{part: "loop11"},
		{part: "loop1"},
		{part: "loop1p"},
		{part: "loop1px"},
		{part: "loop1p1x"},
		{part: "loop12p1"},
		{part: "sda1"},
		{part: ""},
	}
	for _, tt := range tests {
		if got := isLoopChild("loop1", tt.part); got != tt.want {
			t.Errorf("isLoopChild(%q, %q) = %t, want %t", "loop1", tt.part, got, tt.want)
		}
	}
}

func TestWaitForLoopPartition(t *testing.T) {
	tests := []struct {
		desc        string
		appearAfter int // The lsblk call that first lists p1, or 0 for never.
		nodeAfter   int // The stat call that first finds the device node.
		parent      string
		lsblkErr    error
		wantProbes  int
		wantErr     bool
	}{
		{desc: "already present", appearAfter: 1},
		{desc: "appears late", appearAfter: 5, wantProbes: 1},
		{desc: "device node appears late", appearAfter: 1, nodeAfter: 3, wantProbes: 1},
		{desc: "never appears", wantProbes: 1, wantErr: true},
		{desc: "partition of another device", appearAfter: 1, parent: "loop8", wantProbes: 1, wantErr: true},
		{desc: "lsblk fails", lsblkErr: errors.New("exit status 1"), wantProbes: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			clock := stubImageGlobals(t)
			var lsblkCalls, statCalls, probes int
			imageRunCmd = func(name string, args ...string) (string, error) {
				switch {
				case name == "udevadm":
					// Best effort, so an error must not stop polling.
					return "", errors.New("udevadm: not found")
				case name == "sudo" && len(args) > 0 && args[0] == "partprobe":
					probes++
					return "", nil
				case name == "lsblk":
					lsblkCalls++
					if tt.lsblkErr != nil {
						return "", tt.lsblkErr
					}
					rows := []map[string]any{{"kname": "loop7", "type": "loop"}}
					if tt.appearAfter > 0 && lsblkCalls >= tt.appearAfter {
						parent := tt.parent
						if parent == "" {
							parent = "loop7"
						}
						rows = append(rows, map[string]any{"kname": "loop7p1", "type": "part", "pkname": parent})
					}
					return blockJSON(rows...), nil
				}
				t.Errorf("unexpected command %s %q", name, args)
				return "", errors.New("unexpected command")
			}
			imageStat = func(name string) (os.FileInfo, error) {
				if name != "/dev/loop7p1" {
					t.Errorf("imageStat(%q), want /dev/loop7p1", name)
				}
				statCalls++
				if statCalls < tt.nodeAfter {
					return nil, os.ErrNotExist
				}
				return nil, nil
			}

			err := waitForLoopPartition(testLoopID)
			elapsed := clock.now.Sub(testTime)
			if probes != tt.wantProbes {
				t.Errorf("partprobe ran %d times, want %d", probes, tt.wantProbes)
			}
			if tt.wantErr {
				if !errors.Is(err, errImageFile) {
					t.Errorf("waitForLoopPartition() = %v, want %v", err, errImageFile)
				}
				if elapsed < imagePartitionTimeout || elapsed > imagePartitionTimeout+2*imagePollInterval {
					t.Errorf("waitForLoopPartition() gave up after %v, want about %v", elapsed, imagePartitionTimeout)
				}
				return
			}
			if err != nil {
				t.Errorf("waitForLoopPartition() = %v, want nil", err)
			}
		})
	}
}

func TestAssertUnmounted(t *testing.T) {
	tests := []struct {
		desc    string
		out     string
		err     error
		wantErr bool
	}{
		{desc: "unmounted", out: blockJSON(map[string]any{"kname": "loop7", "type": "loop"})},
		{desc: "partition mounted", out: blockJSON(map[string]any{"kname": "loop7"}, map[string]any{"kname": "loop7p1", "mountpoint": "/mnt"}), wantErr: true},
		{desc: "lsblk error", err: errors.New("exit status 1"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			imageRunCmd = func(string, ...string) (string, error) { return tt.out, tt.err }
			err := assertUnmounted(testLoopID)
			if tt.wantErr != errors.Is(err, errDetach) || (!tt.wantErr && err != nil) {
				t.Errorf("assertUnmounted() = %v, want error: %t", err, tt.wantErr)
			}
		})
	}
}

// fakeImageInner is an installer.Device that records its calls.
type fakeImageInner struct {
	id          string
	log         func(string)
	onPartition func()

	dismountErr error
	detectErr   error
	partErr     error
	selErr      error
	wipeErr     error
	part        *storage.Partition
}

func (f *fakeImageInner) record(s string) {
	if f.log != nil {
		f.log(s)
	}
}

func (f *fakeImageInner) Dismount() error {
	f.record("dismount")
	return f.dismountErr
}

func (f *fakeImageInner) Eject() error {
	f.record("eject")
	return errors.New("inner Eject must not be called")
}

func (f *fakeImageInner) FriendlyName() string {
	return "inner"
}

func (f *fakeImageInner) Identifier() string {
	return f.id
}

func (f *fakeImageInner) Partition(string) error {
	f.record("partition")
	if f.partErr != nil {
		return f.partErr
	}
	if f.onPartition != nil {
		f.onPartition()
	}
	return nil
}

func (f *fakeImageInner) DetectPartitions(mount bool) error {
	f.record(fmt.Sprintf("detect(%t)", mount))
	return f.detectErr
}

func (f *fakeImageInner) SelectPartition(uint64, storage.FileSystem) (*storage.Partition, error) {
	f.record("select")
	if f.selErr != nil {
		return nil, f.selErr
	}
	if f.part == nil {
		f.part = &storage.Partition{}
	}
	return f.part, nil
}

func (f *fakeImageInner) Size() uint64 {
	return 1 << 30
}

func (f *fakeImageInner) Wipe() error {
	f.record("wipe")
	return f.wipeErr
}

func TestImageDeviceDelegation(t *testing.T) {
	stubImageGlobals(t)
	var calls []string
	errTest := errors.New("injected")
	inner := &fakeImageInner{id: "loop1", log: func(s string) { calls = append(calls, s) }, dismountErr: errTest, detectErr: errTest, wipeErr: errTest}
	d := &imageDevice{inner: inner, id: "loop1", path: "/tmp/out.bin"}

	if err := d.Eject(); err != nil {
		t.Errorf("Eject() = %v, want nil", err)
	}
	if got := d.FriendlyName(); !strings.Contains(got, "/tmp/out.bin") || !strings.Contains(got, "/dev/loop1") {
		t.Errorf("FriendlyName() = %q, want the path and the loop device", got)
	}
	if got := d.Identifier(); got != "loop1" {
		t.Errorf("Identifier() = %q, want %q", got, "loop1")
	}
	if got := d.Size(); got != 1<<30 {
		t.Errorf("Size() = %d, want %d", got, 1<<30)
	}
	if err := d.Dismount(); !errors.Is(err, errTest) {
		t.Errorf("Dismount() = %v, want %v", err, errTest)
	}
	if err := d.DetectPartitions(true); !errors.Is(err, errTest) {
		t.Errorf("DetectPartitions() = %v, want %v", err, errTest)
	}
	if err := d.Wipe(); !errors.Is(err, errTest) {
		t.Errorf("Wipe() = %v, want %v", err, errTest)
	}
	if want := []string{"dismount", "detect(true)", "wipe"}; !slices.Equal(calls, want) {
		t.Errorf("inner calls = %q, want %q", calls, want)
	}
}

func TestImageDevicePartition(t *testing.T) {
	errTest := errors.New("injected")
	tests := []struct {
		desc      string
		partErr   error
		appear    bool
		wantErr   error
		wantCalls []string
	}{
		{desc: "success", appear: true, wantCalls: []string{"partition", "detect(false)"}},
		{desc: "inner error", partErr: errTest, wantErr: errTest, wantCalls: []string{"partition"}},
		{desc: "partition never appears", wantErr: errImageFile, wantCalls: []string{"partition"}},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			if tt.partErr == nil {
				imageRunCmd = func(name string, args ...string) (string, error) {
					rows := []map[string]any{{"kname": "loop1", "type": "loop"}}
					if tt.appear {
						rows = append(rows, map[string]any{"kname": "loop1p1", "type": "part", "pkname": "loop1"})
					}
					return blockJSON(rows...), nil
				}
				imageStat = func(string) (os.FileInfo, error) { return nil, nil }
			}
			var calls []string
			inner := &fakeImageInner{id: "loop1", log: func(s string) { calls = append(calls, s) }, partErr: tt.partErr}
			d := &imageDevice{inner: inner, id: "loop1", path: "/tmp/out.bin"}
			err := d.Partition("TEST")
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("Partition() = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("inner calls = %q, want %q", calls, tt.wantCalls)
			}
		})
	}
}

func TestImageDeviceSelectPartition(t *testing.T) {
	errTest := errors.New("injected")
	tests := []struct {
		desc    string
		id      string
		selErr  error
		wantErr error
	}{
		{desc: "child partition", id: "loop1p1"},
		{desc: "stale storage name", id: "loop11", wantErr: errImageFile},
		{desc: "whole device", id: "loop1", wantErr: errImageFile},
		{desc: "other disk", id: "sda1", wantErr: errImageFile},
		{desc: "inner error", selErr: errTest, wantErr: errTest},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			stubImageGlobals(t)
			imagePartitionID = func(*storage.Partition) string {
				if tt.selErr != nil {
					t.Error("imagePartitionID was called after an inner error")
				}
				return tt.id
			}
			inner := &fakeImageInner{id: "loop1", selErr: tt.selErr}
			d := &imageDevice{inner: inner, id: "loop1", path: "/tmp/out.bin"}
			got, err := d.SelectPartition(uint64(oneGB), storage.FAT32)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || got != nil {
					t.Errorf("SelectPartition() = (%v, %v), want (nil, %v)", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != inner.part {
				t.Errorf("SelectPartition() = (%v, %v), want the inner partition", got, err)
			}
		})
	}
}

func TestNotifyInterrupt(t *testing.T) {
	oldVerbose := console.Verbose
	t.Cleanup(func() { console.Verbose = oldVerbose })
	console.Verbose = false

	t.Run("not cancelled", func(t *testing.T) {
		out := captureStdout(t, func() {
			stop := notifyInterrupt(context.Background())
			stop()
		})
		if out != "" {
			t.Errorf("notifyInterrupt() without an interrupt printed %q, want nothing", out)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		orig := os.Stdout
		os.Stdout = w
		defer func() { os.Stdout = orig }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop := notifyInterrupt(ctx)
		cancel()
		// Reading the message before calling stop ensures that the
		// notification won the race against stop.
		br := bufio.NewReader(r)
		first, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the interrupt message: %v", err)
		}
		stop()
		os.Stdout = orig
		w.Close()
		rest, err := io.ReadAll(br)
		if err != nil {
			t.Fatal(err)
		}
		if out := first + string(rest); strings.Count(out, "Interrupt received") != 1 {
			t.Errorf("notifyInterrupt() printed %q, want the interrupt message once", out)
		}
	})
}

func TestCheckpoint(t *testing.T) {
	if err := checkpoint(context.Background(), "next"); err != nil {
		t.Errorf("checkpoint(active) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkpoint(ctx, "next"); !errors.Is(err, errInterrupted) {
		t.Errorf("checkpoint(cancelled) = %v, want %v", err, errInterrupted)
	}
}

func TestRecoveryHint(t *testing.T) {
	tests := []struct {
		desc        string
		loopID      string
		attached    bool
		partial     bool
		want, avoid []string
	}{
		{
			desc:     "verified loop device",
			loopID:   "loop7",
			attached: true,
			partial:  true,
			want:     []string{"sudo umount /dev/loop7p1", "sudo losetup --detach /dev/loop7\n", "rm "},
			avoid:    []string{"--associated"},
		},
		{
			desc:     "unverified loop device",
			attached: true,
			partial:  true,
			want:     []string{"losetup --associated", "sudo losetup --detach <each device listed above>", "rm "},
			avoid:    []string{"umount"},
		},
		{
			desc:    "partial file only",
			partial: true,
			want:    []string{"rm "},
			avoid:   []string{"losetup", "umount"},
		},
		{
			desc:   "partial file already removed",
			loopID: "loop7",
			want:   []string{"sudo losetup --detach /dev/loop7\n"},
			avoid:  []string{"rm "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			j := &imageJob{partial: filepath.Join(t.TempDir(), "out.bin.partial"), loopID: tt.loopID, attached: tt.attached}
			if tt.partial {
				if err := os.WriteFile(j.partial, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := j.recoveryHint()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("recoveryHint() = %q, want it to contain %q", got, want)
				}
			}
			for _, avoid := range tt.avoid {
				if strings.Contains(got, avoid) {
					t.Errorf("recoveryHint() = %q, want it not to contain %q", got, avoid)
				}
			}
		})
	}
}

func TestPublish(t *testing.T) {
	requireImageMode(t)
	tests := []struct {
		desc        string
		finalExists bool
		finalDir    string // A folder below the temporary folder; it is not created.
		wantErr     bool
	}{
		{desc: "free name"},
		{desc: "existing final file", finalExists: true, wantErr: true},
		{desc: "link fails", finalDir: "missing", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			dir := t.TempDir()
			j := &imageJob{final: filepath.Join(dir, tt.finalDir, "out.bin"), partial: filepath.Join(dir, "out.bin.partial")}
			f, err := os.Create(j.partial)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("image"); err != nil {
				t.Fatal(err)
			}
			j.file = f
			if tt.finalExists {
				if err := os.WriteFile(j.final, []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = j.publish()
			if j.file != nil {
				t.Error("publish() left the file open")
			}
			wantFinal := "image"
			if tt.wantErr {
				if !errors.Is(err, errImageFile) {
					t.Errorf("publish() = %v, want %v", err, errImageFile)
				}
				wantFinal = "existing"
			} else if err != nil {
				t.Errorf("publish() = %v, want nil", err)
			}
			if tt.finalDir != "" {
				if _, err := os.Lstat(j.final); err == nil {
					t.Errorf("publish() created %q", j.final)
				}
			} else if got, err := os.ReadFile(j.final); err != nil || string(got) != wantFinal {
				t.Errorf("final file = (%q, %v), want %q", got, err, wantFinal)
			}
			if _, err := os.Lstat(j.partial); (err == nil) != tt.wantErr {
				t.Errorf("partial file exists: %t, want %t", err == nil, tt.wantErr)
			}
		})
	}
}

func TestDiscard(t *testing.T) {
	requireImageMode(t)
	tests := []struct {
		desc        string
		noInfo      bool
		attached    bool
		replace     bool
		wantPartial bool
	}{
		{desc: "removes the partial file"},
		{desc: "keeps a file it did not create", noInfo: true, wantPartial: true},
		{desc: "keeps the file while attached", attached: true, wantPartial: true},
		{desc: "keeps a replaced file", replace: true, wantPartial: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			dir := t.TempDir()
			j := &imageJob{partial: filepath.Join(dir, "out.bin.partial"), attached: tt.attached}
			f, err := os.Create(j.partial)
			if err != nil {
				t.Fatal(err)
			}
			j.file = f
			if !tt.noInfo {
				if j.info, err = f.Stat(); err != nil {
					t.Fatal(err)
				}
			}
			if tt.replace {
				if err := os.Remove(j.partial); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(j.partial, []byte("other"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			j.discard()
			if j.file != nil {
				t.Error("discard() left the file open")
			}
			if _, err := os.Lstat(j.partial); (err == nil) != tt.wantPartial {
				t.Errorf("partial file exists: %t, want %t", err == nil, tt.wantPartial)
			}
		})
	}
}

// captureStdout returns what fn writes to os.Stdout, where console prints.
func captureStdout(t *testing.T, fn func()) (out string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		r.Close()
		done <- string(b)
	}()
	orig := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = orig
		w.Close()
		out = <-done
	}()
	fn()
	return ""
}

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// deckRecorder is a deck backend that keeps info messages in memory.
type deckRecorder struct {
	mu   sync.Mutex
	info []string
}

func (r *deckRecorder) New(lvl deck.Level, msg string) deck.Composer {
	return &deckMessage{r: r, lvl: lvl, msg: msg}
}

func (r *deckRecorder) Close() error { return nil }

// reset discards the recorded messages.
func (r *deckRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.info = nil
}

// messages returns the recorded info messages.
func (r *deckRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.info)
}

type deckMessage struct {
	r   *deckRecorder
	lvl deck.Level
	msg string
}

func (m *deckMessage) Compose(*deck.AttribStore) error { return nil }

func (m *deckMessage) Write() error {
	if m.lvl == deck.INFO {
		m.r.mu.Lock()
		defer m.r.mu.Unlock()
		m.r.info = append(m.r.info, m.msg)
	}
	return nil
}

// imageDeckLog records messages sent to the default deck. Backends cannot be
// removed from the default deck, so a single recorder is shared by all tests.
var imageDeckLog = sync.OnceValue(func() *deckRecorder {
	r := &deckRecorder{}
	deck.Add(r)
	return r
})

func TestPrintSuccess(t *testing.T) {
	final := filepath.Join(t.TempDir(), "my images", "out.bin")
	tests := []struct {
		desc      string
		seed      bool
		verbose   bool
		driveGB   int
		wantDrive string
	}{
		{desc: "without seed", driveGB: 8, wantDrive: "8GB or larger"},
		{desc: "with seed", seed: true, driveGB: 16, wantDrive: "16GB or larger"},
		{desc: "no standard drive size", wantDrive: "at least 4.0 GiB"},
		{desc: "verbose", seed: true, verbose: true, driveGB: 8},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			oldVerbose := console.Verbose
			t.Cleanup(func() { console.Verbose = oldVerbose })
			console.Verbose = tt.verbose
			log := imageDeckLog()
			log.reset()
			j := &imageJob{final: final, size: 4 << 30, driveGB: tt.driveGB}

			out := captureStdout(t, func() { j.printSuccess(tt.seed) })

			msgs := log.messages()
			if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, final) }) {
				t.Errorf("printSuccess() logged %q, want a message containing %q", msgs, final)
			}
			if tt.verbose {
				if out != "" {
					t.Errorf("printSuccess() in verbose mode printed %q, want nothing", out)
				}
				return
			}
			if got, want := lastLine(out), "Image file: "+final; got != want {
				t.Errorf("printSuccess() last line = %q, want %q\noutput:\n%s", got, want, out)
			}
			if strings.Contains(out, strconv.Quote(final)) {
				t.Errorf("printSuccess() quoted the path, want it unquoted:\n%s", out)
			}
			if !strings.Contains(out, tt.wantDrive) {
				t.Errorf("printSuccess() output does not contain %q:\n%s", tt.wantDrive, out)
			}
			if got := strings.Contains(out, "seed"); got != tt.seed {
				t.Errorf("printSuccess() mentions the seed: %t, want %t\noutput:\n%s", got, tt.seed, out)
			}
		})
	}
}

// imageHarness fakes the loop device tools, the storage device, and the
// installer so that runImage can run end to end without root. Every call that
// matters for ordering is recorded in events.
type imageHarness struct {
	t      *testing.T
	clock  *fakeClock
	dir    string // The output folder.
	cache  string // The installer cache folder.
	events []string

	// Failure injection.
	usbErr        error
	missingTool   string
	noLoopControl bool
	notElevated   bool
	free          uint64
	freeErr       error
	attachOut     string
	attachErr     error
	// attachDespiteErr attaches the file even when attachErr is returned.
	attachDespiteErr bool
	inodeErr         error
	reportedIno      uint64
	newDevErr        error
	partitionID      string
	dismountErr      error
	mountStuck       bool
	detachErr        error
	stuckAttached    bool
	associatedErr    error
	interruptAt      string // An event after which the run is interrupted, or "start".
	newInstErr       error
	retrieveErr      error
	skipISO          bool
	prepErr          error
	provErr          error
	finErr           error
	onProvision      func(h *imageHarness)
	onFinalize       func(h *imageHarness)

	// State.
	usbCalls     int
	partial      string
	size         int64
	attached     bool
	partitioned  bool
	finalTargets int
	inner        *fakeImageInner // The storage device, once created.
	ctx          context.Context
	cancel       context.CancelFunc // Simulates the first interrupt.
}

func newImageHarness(t *testing.T) *imageHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &imageHarness{
		t:           t,
		ctx:         ctx,
		cancel:      cancel,
		clock:       stubImageGlobals(t),
		dir:         t.TempDir(),
		cache:       t.TempDir(),
		free:        1 << 50,
		attachOut:   "/dev/" + testLoopID + "\n",
		reportedIno: testIno,
		partitionID: testLoopID + "p1",
	}
}

// log records an event. Repeated status polls are recorded once. The run is
// interrupted after the interruptAt event.
func (h *imageHarness) log(e string) {
	if e == h.interruptAt {
		h.cancel()
	}
	if e == "associated" && len(h.events) > 0 && h.events[len(h.events)-1] == e {
		return
	}
	h.events = append(h.events, e)
}

// keyEvents drops the inner device events that only add detail.
func (h *imageHarness) keyEvents() []string {
	var out []string
	for _, e := range h.events {
		switch e {
		case "partition", "select", "detect(false)", "detect(true)":
			continue
		}
		out = append(out, e)
	}
	return out
}

// install wires the harness into the dependency injections. It runs after the
// test case has configured failure injection.
func (h *imageHarness) install() {
	funcUSBPermissions = func() error {
		h.usbCalls++
		return h.usbErr
	}
	config.IsElevatedCmd = func() (bool, error) { return !h.notElevated, nil }
	imageLookPath = func(name string) (string, error) {
		if name == h.missingTool {
			return "", fmt.Errorf("%q not found", name)
		}
		return "/usr/bin/" + name, nil
	}
	imageStat = func(name string) (os.FileInfo, error) {
		switch name {
		case loopControl:
			if h.noLoopControl {
				return nil, os.ErrNotExist
			}
			return nil, nil
		case "/dev/" + testLoopID + "p1":
			return nil, nil
		}
		h.t.Errorf("unexpected imageStat(%q)", name)
		return nil, os.ErrNotExist
	}
	imageFreeBytes = func(string) (uint64, error) {
		h.log("freebytes")
		return h.free, h.freeErr
	}
	imageFileInode = func(*os.File) (uint64, error) {
		if h.inodeErr != nil {
			return 0, h.inodeErr
		}
		return testIno, nil
	}
	imageRunCmd = h.run
	imageNewDevice = func(id string) (installer.Device, error) {
		if id != testLoopID {
			h.t.Errorf("imageNewDevice(%q), want %q", id, testLoopID)
		}
		if h.newDevErr != nil {
			return nil, h.newDevErr
		}
		h.inner = &fakeImageInner{
			id:          id,
			log:         h.log,
			dismountErr: h.dismountErr,
			onPartition: func() { h.partitioned = true },
		}
		return h.inner, nil
	}
	imagePartitionID = func(*storage.Partition) string { return h.partitionID }
	newInstaller = func(c installer.Configuration) (imageInstaller, error) {
		h.log("installer")
		if h.newInstErr != nil {
			return nil, h.newInstErr
		}
		return &fakeImageInstaller{h: h, conf: c}, nil
	}
}

// run fakes the loop device commands.
func (h *imageHarness) run(name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	dev := "/dev/" + testLoopID
	switch {
	case strings.HasPrefix(cmd, "sudo losetup --find --show --partscan "):
		h.log("attach")
		h.partial = args[len(args)-1]
		if h.attachErr != nil {
			h.attached = h.attachDespiteErr
			return "", h.attachErr
		}
		fi, err := os.Stat(h.partial)
		if err != nil {
			h.t.Errorf("attach: %v", err)
			return "", err
		}
		h.size = fi.Size()
		h.attached = loopDevRE.MatchString(strings.TrimSpace(h.attachOut))
		return h.attachOut, nil
	case cmd == "losetup --list --json --output NAME,BACK-FILE,BACK-INO,OFFSET,SIZELIMIT "+dev:
		h.log("verify")
		return loopJSON(map[string]any{"name": dev, "back-file": h.partial, "back-ino": h.reportedIno, "offset": 0, "sizelimit": 0}), nil
	case cmd == "lsblk --json --bytes --output KNAME,TYPE,SIZE,PKNAME,MOUNTPOINT "+dev:
		rows := []map[string]any{{"kname": testLoopID, "type": "loop", "size": h.size}}
		if h.partitioned {
			p := map[string]any{"kname": testLoopID + "p1", "type": "part", "size": h.size / 2, "pkname": testLoopID}
			if h.mountStuck {
				p["mountpoint"] = "/tmp/installer_mount"
			}
			rows = append(rows, p)
		}
		return blockJSON(rows...), nil
	case cmd == "udevadm settle --timeout=5", cmd == "sudo partprobe "+dev:
		return "", nil
	case cmd == "sudo losetup --detach "+dev:
		h.log("detach")
		if h.detachErr != nil {
			return "", h.detachErr
		}
		if !h.stuckAttached {
			h.attached = false
		}
		return "", nil
	case strings.HasPrefix(cmd, "losetup --associated "):
		h.log("associated")
		if h.associatedErr != nil {
			return "", h.associatedErr
		}
		if h.attached {
			return dev + ": []: (" + h.partial + ")\n", nil
		}
		return "", nil
	}
	h.t.Errorf("unexpected command %q", cmd)
	return "", fmt.Errorf("unexpected command %q", cmd)
}

// runImage parses args for the windows command and runs image mode.
func (h *imageHarness) runImage(args ...string) error {
	h.t.Helper()
	h.install()
	c := &writeCmd{name: "windows", distro: "windows", track: "stable"}
	f, err := newFlagSet(h.t, c, args...)
	if err != nil {
		h.t.Fatalf("Parse(%q) = %v, want nil", args, err)
	}
	if err := validateImageMode(c, f); err != nil {
		h.t.Fatalf("validateImageMode(%q) = %v, want nil", args, err)
	}
	if h.interruptAt == "start" {
		h.cancel()
	}
	return runImage(h.ctx, c, f)
}

// fakeImageInstaller implements imageInstaller on top of the harness.
type fakeImageInstaller struct {
	h    *imageHarness
	conf installer.Configuration
}

func (i *fakeImageInstaller) Cache() string {
	return i.h.cache
}

// Retrieve writes sparse files in place of the downloaded image and config.
func (i *fakeImageInstaller) Retrieve() error {
	i.h.log("retrieve")
	if i.h.retrieveErr != nil {
		return i.h.retrieveErr
	}
	if i.h.skipISO {
		return nil
	}
	files := []string{i.conf.ImageFile()}
	if i.conf.NeedsConfig() {
		files = append(files, i.conf.FFUConfFile())
	}
	for _, name := range files {
		f, err := os.Create(filepath.Join(i.h.cache, name))
		if err != nil {
			return err
		}
		err = f.Truncate(testISOSize)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *fakeImageInstaller) Prepare(d installer.Device) error {
	i.h.log("prepare")
	if i.h.prepErr != nil {
		return i.h.prepErr
	}
	return d.Partition("TEST")
}

func (i *fakeImageInstaller) Provision(d installer.Device) error {
	i.h.log("provision")
	if i.h.onProvision != nil {
		i.h.onProvision(i.h)
	}
	if i.h.provErr != nil {
		return i.h.provErr
	}
	_, err := d.SelectPartition(uint64(oneGB), storage.FAT32)
	return err
}

func (i *fakeImageInstaller) Finalize(devices []installer.Device, dismount bool) error {
	i.h.log("finalize")
	i.h.finalTargets = len(devices)
	if !dismount {
		i.h.t.Error("Finalize() was called with dismount = false")
	}
	if i.h.onFinalize != nil {
		i.h.onFinalize(i.h)
	}
	return i.h.finErr
}

// cacheBytes returns the size of the files that Retrieve wrote.
func (h *imageHarness) cacheBytes() uint64 {
	h.t.Helper()
	entries, err := os.ReadDir(h.cache)
	if err != nil {
		h.t.Fatal(err)
	}
	var total uint64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			h.t.Fatal(err)
		}
		total += uint64(fi.Size())
	}
	return total
}

var happyEvents = []string{"installer", "retrieve", "freebytes", "attach", "verify", "prepare", "provision", "dismount", "finalize", "detach", "associated"}

func TestRunImage(t *testing.T) {
	requireImageMode(t)
	tests := []struct {
		desc     string
		args     func(h *imageHarness) []string
		setup    func(h *imageHarness)
		wantName string // A file name, or a suffix when it starts with '-'.
	}{
		{
			desc:     "explicit file",
			args:     func(h *imageHarness) []string { return []string{"--image_file=" + filepath.Join(h.dir, "out.bin")} },
			wantName: "out.bin",
		},
		{
			desc:     "positional file",
			args:     func(h *imageHarness) []string { return []string{"--image_file", filepath.Join(h.dir, "out.bin")} },
			wantName: "out.bin",
		},
		{
			desc:     "folder gets the default name",
			args:     func(h *imageHarness) []string { return []string{"--image_file=" + h.dir} },
			wantName: "-stable-20260102-030405.bin",
		},
		{
			desc:     "other extensions are allowed",
			args:     func(h *imageHarness) []string { return []string{"--image_file=" + filepath.Join(h.dir, "out.img")} },
			wantName: "out.img",
		},
		{
			desc: "bare flag uses the home folder",
			args: func(*imageHarness) []string { return []string{"--image_file"} },
			setup: func(h *imageHarness) {
				imageUserHomeDir = func() (string, error) { return h.dir, nil }
			},
			wantName: "-stable-20260102-030405.bin",
		},
		{
			desc: "sudo hands the file to the invoking user",
			args: func(h *imageHarness) []string { return []string{"--image_file=" + filepath.Join(h.dir, "out.bin")} },
			setup: func(*imageHarness) {
				env := map[string]string{"SUDO_UID": strconv.Itoa(os.Getuid()), "SUDO_GID": strconv.Itoa(os.Getgid())}
				imageGeteuid = func() int { return 0 }
				imageGetenv = func(k string) string { return env[k] }
			},
			wantName: "out.bin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			h := newImageHarness(t)
			console.Verbose = false
			log := imageDeckLog()
			log.reset()
			if tt.setup != nil {
				tt.setup(h)
			}
			var err error
			out := captureStdout(t, func() { err = h.runImage(tt.args(h)...) })
			if err != nil {
				t.Fatalf("runImage() = %v, want nil", err)
			}
			// The download source and cache folder are details for the log.
			if strings.Contains(out, h.cache) {
				t.Errorf("runImage() printed the cache folder %q; output:\n%s", h.cache, out)
			}
			if msgs := log.messages(); !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, h.cache) }) {
				t.Errorf("runImage() logged %q, want a message containing the cache folder %q", msgs, h.cache)
			}
			if got := h.keyEvents(); !slices.Equal(got, happyEvents) {
				t.Errorf("runImage() events = %q, want %q", got, happyEvents)
			}
			entries, err := os.ReadDir(h.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("output folder contains %v, want only the image file", entries)
			}
			name := entries[0].Name()
			if strings.HasPrefix(tt.wantName, "-") {
				if !strings.HasPrefix(name, binaryName+"-") || !strings.HasSuffix(name, tt.wantName) {
					t.Errorf("image file name = %q, want %q followed by a distribution and %q", name, binaryName+"-", tt.wantName)
				}
			} else if name != tt.wantName {
				t.Errorf("image file name = %q, want %q", name, tt.wantName)
			}
			if got, want := lastLine(out), "Image file: "+filepath.Join(h.dir, name); got != want {
				t.Errorf("runImage() last output line = %q, want %q", got, want)
			}
			fi, err := os.Stat(filepath.Join(h.dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0600 {
				t.Errorf("image file mode = %v, want 0600", got)
			}
			wantSize, _ := computeImageSize(h.cacheBytes())
			if uint64(fi.Size()) != wantSize {
				t.Errorf("image file size = %d, want %d", fi.Size(), wantSize)
			}
			if h.finalTargets != 1 {
				t.Errorf("Finalize() got %d devices, want 1", h.finalTargets)
			}
			if h.attached {
				t.Error("runImage() left the loop device attached")
			}
			if h.usbCalls != 1 {
				t.Errorf("funcUSBPermissions was called %d times, want 1", h.usbCalls)
			}
		})
	}
}

func TestRunImageFailures(t *testing.T) {
	requireImageMode(t)
	errTest := errors.New("injected")
	// seq returns the happy path events up to and including last, followed by
	// rest.
	seq := func(last string, rest ...string) []string {
		i := slices.Index(happyEvents, last)
		return append(slices.Clone(happyEvents[:i+1]), rest...)
	}
	cleanupAfterAttach := []string{"finalize", "detach", "associated"}
	// Before the loop device is verified, cleanup looks up the devices that
	// are attached to the partial file and detaches only those.
	cleanupUnverified := []string{"finalize", "associated", "detach", "associated"}
	cleanupAfterDevice := []string{"dismount", "finalize", "detach", "associated"}
	tests := []struct {
		desc         string
		path         string // Relative to the output folder, "out.bin" by default.
		args         []string
		setup        func(h *imageHarness)
		wantErr      error
		wantEvents   []string
		wantPartial  bool
		wantAttached bool // Whether the loop device is still attached at the end.
		check        func(t *testing.T, h *imageHarness, final string)
	}{
		{
			desc:    "removable media policy",
			setup:   func(h *imageHarness) { h.usbErr = config.ErrWritePerms },
			wantErr: config.ErrUSBwriteAccess,
		},
		{
			desc:    "removable media check error",
			setup:   func(h *imageHarness) { h.usbErr = errTest },
			wantErr: config.ErrUSBwriteAccess,
		},
		{
			desc:    "missing tool",
			setup:   func(h *imageHarness) { h.missingTool = "losetup" },
			wantErr: errImageFile,
		},
		{
			desc:    "no loop device support",
			setup:   func(h *imageHarness) { h.noLoopControl = true },
			wantErr: errImageFile,
		},
		{
			desc:    "unknown distribution",
			args:    []string{"--distro=unknown"},
			wantErr: errConfig,
		},
		{
			desc:    "not elevated",
			setup:   func(h *imageHarness) { h.notElevated = true },
			wantErr: errElevation,
		},
		{
			desc:    "missing output folder",
			path:    "missing/out.bin",
			wantErr: errImageFile,
		},
		{
			desc: "final file exists",
			setup: func(h *imageHarness) {
				if err := os.WriteFile(filepath.Join(h.dir, "out.bin"), []byte("existing"), 0644); err != nil {
					h.t.Fatal(err)
				}
			},
			wantErr: errImageFile,
			check: func(t *testing.T, _ *imageHarness, final string) {
				if got, err := os.ReadFile(final); err != nil || string(got) != "existing" {
					t.Errorf("final file = (%q, %v), want it untouched", got, err)
				}
			},
		},
		{
			desc: "partial file exists",
			setup: func(h *imageHarness) {
				if err := os.WriteFile(filepath.Join(h.dir, "out.bin"+imagePartialExt), []byte("stale"), 0644); err != nil {
					h.t.Fatal(err)
				}
			},
			wantErr:     errImageFile,
			wantPartial: true,
			check: func(t *testing.T, _ *imageHarness, final string) {
				if got, err := os.ReadFile(final + imagePartialExt); err != nil || string(got) != "stale" {
					t.Errorf("partial file = (%q, %v), want it untouched", got, err)
				}
			},
		},
		{
			desc: "invalid SUDO_GID",
			setup: func(*imageHarness) {
				env := map[string]string{"SUDO_UID": strconv.Itoa(os.Getuid()), "SUDO_GID": "group"}
				imageGeteuid = func() int { return 0 }
				imageGetenv = func(k string) string { return env[k] }
			},
			wantErr: errImageFile,
		},
		{
			desc:       "installer error",
			setup:      func(h *imageHarness) { h.newInstErr = errTest },
			wantErr:    errInstaller,
			wantEvents: []string{"installer"},
		},
		{
			desc:       "interrupt before download",
			setup:      func(h *imageHarness) { h.interruptAt = "start" },
			wantErr:    errInterrupted,
			wantEvents: []string{"installer", "finalize"},
		},
		{
			desc:       "retrieve error",
			setup:      func(h *imageHarness) { h.retrieveErr = errTest },
			wantErr:    errRetrieve,
			wantEvents: seq("retrieve", "finalize"),
		},
		{
			desc:       "missing payload",
			setup:      func(h *imageHarness) { h.skipISO = true },
			wantErr:    errImageFile,
			wantEvents: seq("retrieve", "finalize"),
		},
		{
			desc:       "interrupt before sizing",
			setup:      func(h *imageHarness) { h.interruptAt = "retrieve" },
			wantErr:    errInterrupted,
			wantEvents: seq("retrieve", "finalize"),
		},
		{
			desc:       "free space error",
			setup:      func(h *imageHarness) { h.freeErr = errTest },
			wantErr:    errImageFile,
			wantEvents: seq("freebytes", "finalize"),
		},
		{
			desc:       "insufficient free space",
			setup:      func(h *imageHarness) { h.free = oneMiB },
			wantErr:    errImageFile,
			wantEvents: seq("freebytes", "finalize"),
		},
		{
			desc:       "interrupt before attach",
			setup:      func(h *imageHarness) { h.interruptAt = "freebytes" },
			wantErr:    errInterrupted,
			wantEvents: seq("freebytes", "finalize"),
		},
		{
			desc:       "attach error",
			setup:      func(h *imageHarness) { h.attachErr = errTest },
			wantErr:    errAttach,
			wantEvents: seq("attach", "finalize", "associated"),
		},
		{
			// losetup can attach the file and still report an error.
			desc: "attach error after attaching",
			setup: func(h *imageHarness) {
				h.attachErr = errTest
				h.attachDespiteErr = true
			},
			wantErr:    errAttach,
			wantEvents: seq("attach", cleanupUnverified...),
		},
		{
			desc:       "unexpected attach output",
			setup:      func(h *imageHarness) { h.attachOut = "/dev/loop7p1\n" },
			wantErr:    errAttach,
			wantEvents: seq("attach", "finalize", "associated"),
		},
		{
			desc:       "inode error",
			setup:      func(h *imageHarness) { h.inodeErr = errTest },
			wantErr:    errAttach,
			wantEvents: seq("attach", cleanupUnverified...),
		},
		{
			desc:       "loop device backed by another file",
			setup:      func(h *imageHarness) { h.reportedIno = testIno + 1 },
			wantErr:    errAttach,
			wantEvents: seq("verify", cleanupUnverified...),
		},
		{
			desc:       "storage device error",
			setup:      func(h *imageHarness) { h.newDevErr = errTest },
			wantErr:    errDevice,
			wantEvents: seq("verify", cleanupAfterAttach...),
		},
		{
			desc:       "interrupt before prepare",
			setup:      func(h *imageHarness) { h.interruptAt = "verify" },
			wantErr:    errInterrupted,
			wantEvents: seq("verify", cleanupAfterDevice...),
		},
		{
			desc:       "prepare error",
			setup:      func(h *imageHarness) { h.prepErr = errTest },
			wantErr:    errPrepare,
			wantEvents: seq("prepare", cleanupAfterDevice...),
		},
		{
			desc:       "interrupt before provision",
			setup:      func(h *imageHarness) { h.interruptAt = "prepare" },
			wantErr:    errInterrupted,
			wantEvents: seq("prepare", cleanupAfterDevice...),
		},
		{
			desc:       "provision error",
			setup:      func(h *imageHarness) { h.provErr = errTest },
			wantErr:    errProvision,
			wantEvents: happyEvents,
		},
		{
			desc:       "stale partition name is refused",
			setup:      func(h *imageHarness) { h.partitionID = testLoopID + "1" },
			wantErr:    errProvision,
			wantEvents: happyEvents,
		},
		{
			desc:       "dismount error",
			setup:      func(h *imageHarness) { h.dismountErr = errTest },
			wantErr:    errFinalize,
			wantEvents: happyEvents,
		},
		{
			desc: "partition rescan error during cleanup",
			setup: func(h *imageHarness) {
				h.onProvision = func(h *imageHarness) { h.inner.detectErr = errTest }
			},
			wantErr:    errFinalize,
			wantEvents: seq("provision", "finalize", "detach", "associated"),
		},
		{
			desc:       "finalize error",
			setup:      func(h *imageHarness) { h.finErr = errTest },
			wantErr:    errFinalize,
			wantEvents: happyEvents,
		},
		{
			desc:         "still mounted",
			setup:        func(h *imageHarness) { h.mountStuck = true },
			wantErr:      errDetach,
			wantEvents:   seq("dismount"),
			wantPartial:  true,
			wantAttached: true,
		},
		{
			// Without a storage device there is nothing to dismount, so only
			// the check before detaching can see the mount.
			desc: "mounted during finalize without a device",
			setup: func(h *imageHarness) {
				h.newDevErr = errTest
				h.onFinalize = func(h *imageHarness) {
					h.partitioned = true
					h.mountStuck = true
				}
			},
			wantErr:      errDetach,
			wantEvents:   seq("verify", "finalize"),
			wantPartial:  true,
			wantAttached: true,
		},
		{
			desc:         "detach error",
			setup:        func(h *imageHarness) { h.detachErr = errTest },
			wantErr:      errDetach,
			wantEvents:   seq("detach"),
			wantPartial:  true,
			wantAttached: true,
		},
		{
			desc:         "still attached after detach",
			setup:        func(h *imageHarness) { h.stuckAttached = true },
			wantErr:      errDetach,
			wantEvents:   happyEvents,
			wantPartial:  true,
			wantAttached: true,
			check: func(t *testing.T, h *imageHarness, _ string) {
				if h.clock.sleeps == 0 {
					t.Error("assertDetached did not poll")
				}
			},
		},
		{
			desc:        "detach status unknown",
			setup:       func(h *imageHarness) { h.associatedErr = errTest },
			wantErr:     errDetach,
			wantEvents:  happyEvents,
			wantPartial: true,
		},
		{
			desc: "final file appears during the run",
			setup: func(h *imageHarness) {
				h.onProvision = func(h *imageHarness) {
					if err := os.WriteFile(filepath.Join(h.dir, "out.bin"), []byte("racer"), 0644); err != nil {
						h.t.Fatal(err)
					}
				}
			},
			wantErr:    errImageFile,
			wantEvents: happyEvents,
			check: func(t *testing.T, _ *imageHarness, final string) {
				if got, err := os.ReadFile(final); err != nil || string(got) != "racer" {
					t.Errorf("final file = (%q, %v), want it untouched", got, err)
				}
			},
		},
		{
			desc: "partial file replaced during the run",
			setup: func(h *imageHarness) {
				h.provErr = errTest
				h.onProvision = func(h *imageHarness) {
					p := filepath.Join(h.dir, "out.bin"+imagePartialExt)
					if err := os.Remove(p); err != nil {
						h.t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("other"), 0644); err != nil {
						h.t.Fatal(err)
					}
				}
			},
			wantErr:     errProvision,
			wantEvents:  happyEvents,
			wantPartial: true,
			check: func(t *testing.T, _ *imageHarness, final string) {
				if got, err := os.ReadFile(final + imagePartialExt); err != nil || string(got) != "other" {
					t.Errorf("partial file = (%q, %v), want the replacement untouched", got, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			h := newImageHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			path := tt.path
			if path == "" {
				path = "out.bin"
			}
			final := filepath.Join(h.dir, path)
			err := h.runImage(append(slices.Clone(tt.args), "--image_file="+final)...)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("runImage() = %v, want %v", err, tt.wantErr)
			}
			if got := h.keyEvents(); !slices.Equal(got, tt.wantEvents) {
				t.Errorf("runImage() events = %q, want %q", got, tt.wantEvents)
			}
			if _, err := os.Lstat(final + imagePartialExt); (err == nil) != tt.wantPartial {
				t.Errorf("partial file exists: %t, want %t", err == nil, tt.wantPartial)
			}
			if h.attached != tt.wantAttached {
				t.Errorf("loop device attached after runImage(): %t, want %t", h.attached, tt.wantAttached)
			}
			if tt.check != nil {
				tt.check(t, h, final)
			} else if _, err := os.Lstat(final); err == nil {
				t.Errorf("runImage() published %q after an error", final)
			}
		})
	}
}

func TestRunImagePreflight(t *testing.T) {
	requireImageMode(t)
	for _, tool := range imageTools {
		t.Run(tool, func(t *testing.T) {
			h := newImageHarness(t)
			h.missingTool = tool
			err := h.runImage("--image_file=" + filepath.Join(h.dir, "out.bin"))
			if !errors.Is(err, errImageFile) || !strings.Contains(err.Error(), strconv.Quote(tool)) {
				t.Errorf("runImage() without %s = %v, want %v naming the tool", tool, err, errImageFile)
			}
			if len(h.events) != 0 {
				t.Errorf("runImage() without %s recorded %q, want no events", tool, h.events)
			}
		})
	}
}

func TestRunImageRejectsNonISODistribution(t *testing.T) {
	requireImageMode(t)
	h := newImageHarness(t)
	h.install()
	// Not every build of the config package defines a linux distribution.
	want := errImageFile
	if _, err := config.New(true, false, false, false, false, nil, "linux", "stable", "", ""); err != nil {
		want = errConfig
	}
	err := h.runImage("--distro=linux", "--image_file="+filepath.Join(h.dir, "out.bin"))
	if !errors.Is(err, want) {
		t.Errorf("runImage(linux) = %v, want %v", err, want)
	}
	if len(h.events) != 0 {
		t.Errorf("runImage(linux) recorded %q, want no events", h.events)
	}
}
