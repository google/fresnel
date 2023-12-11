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

package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var (
	imageServer = `https://foo.bar.com`
	// goodDistro represents a valid distribution list.
	goodDistro = distribution{
		imageServer: imageServer,
		images: map[string]string{
			"default": "default_installer.img",
			"stable":  "stable_installer.img",
		},
		configs: map[string]string{
			"default": "default_config.yaml",
			"stable":  "stable_config.yaml",
		},
	}
	distroDefaults = distributions
)

// cmpConfig is a custom comparer for the Configuration struct. We use a custom
// comparer to inspect public-facing members of the two structs. Errors
// describing members that do not match are returned. When all checked fields
// are equal, nil is returned.
// https://godoc.org/github.com/google/go-cmp/cmp#Exporter
func cmpConfig(got, want Configuration) error {
	if got.track != want.track {
		return fmt.Errorf("image track mismatch, got: %q, want: %q", got.track, want.track)
	}
	if got.confTrack != want.confTrack {
		return fmt.Errorf("configuration track mismatch, got: %q, want: %q", got.confTrack, want.confTrack)
	}
	if got.cleanup != want.cleanup {
		return fmt.Errorf("configuration cleanup mismatch, got: %t, want: %t", got.cleanup, want.cleanup)
	}
	if got.warning != want.warning {
		return fmt.Errorf("configuration warning mismatch, got: %t, want: %t", got.warning, want.warning)
	}
	if !equal(got.devices, want.devices) {
		return fmt.Errorf("configuration devices mismatch, got: %v, want: %v", got.devices, want.devices)
	}
	// If no distro was provided anywhere, we can return now.
	if got.distro == nil && want.distro == nil {
		return nil
	}
	// If either distro is nil at this point, we have a mismatch.
	if got.distro == nil || want.distro == nil {
		return fmt.Errorf("configuration distro mismatch, got: %+v\n want: %+v", got.distro, want.distro)
	}
	// distro's are generally static in config, so if we get here, we can safely
	// assume a match, and return.
	return nil
}

// equal compares two string slices and determines if they are the same.
func equal(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i, value := range left {
		if value != right[i] {
			return false
		}
	}
	return true
}

func TestNew(t *testing.T) {
	tests := []struct {
		desc           string
		fakeIsElevated func() (bool, error)
		ffu            bool
		devices        []string
		os             string
		track          string
		confTrack      string
		seedServer     string
		out            *Configuration
		want           error
	}{
		{
			desc:    "bad devices",
			devices: []string{"f:", "/dev/sda1p1"},
			want:    errDevice,
		},
		{
			desc:    "bad distro",
			devices: []string{"disk1"},
			os:      "foo",
			want:    errDistro,
		},
		{
			desc:    "bad track",
			devices: []string{"disk1"},
			os:      "windows",
			track:   "foo",
			want:    errTrack,
		},
		{
			desc:           "bad ffu track",
			devices:        []string{"disk1"},
			ffu:            true,
			os:             "windowsffu",
			confTrack:      "foo",
			track:          "foo",
			fakeIsElevated: func() (bool, error) { return true, nil },
			want:           errTrack,
		},
		{
			desc:       "bad seed server",
			devices:    []string{"disk1"},
			os:         "windows",
			track:      "stable",
			seedServer: "test.foo@bar.com",
			want:       errSeed,
		},
		{
			desc:           "isElevated error",
			devices:        []string{"disk1"},
			os:             "windows",
			track:          "stable",
			fakeIsElevated: func() (bool, error) { return false, errors.New("error") },
			want:           errElevation,
		},
		{
			desc:           "valid config",
			devices:        []string{"disk1"},
			os:             "windows",
			track:          "stable",
			fakeIsElevated: func() (bool, error) { return true, nil },
			out: &Configuration{
				distro:   &goodDistro,
				track:    "stable",
				devices:  []string{"disk1"},
				elevated: true,
			},
			want: nil,
		},
		{
			desc:           "valid config with ffu",
			devices:        []string{"disk1"},
			os:             "windowsffu",
			ffu:            true,
			confTrack:      "unstable",
			track:          "unstable",
			fakeIsElevated: func() (bool, error) { return true, nil },
			out: &Configuration{
				distro:    &goodDistro,
				track:     "unstable",
				confTrack: "unstable",
				devices:   []string{"disk1"},
				elevated:  true,
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		IsElevatedCmd = tt.fakeIsElevated
		c, got := New(false, false, false, tt.ffu, false, tt.devices, tt.os, tt.track, tt.confTrack, tt.seedServer)
		if got == tt.want {
			continue
		}
		if c == tt.out {
			continue
		}
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: New() got: '%v', want: '%v'", tt.desc, got, tt.want)
		}
		if err := cmpConfig(*c, *tt.out); err != nil {
			t.Errorf("%s: %v", tt.desc, err)
		}
	}
}

func TestAddDistro(t *testing.T) {
	// Generate a good distro that is missing seedFile.
	noSeedFile := goodDistro
	noSeedFile.seedServer = `http://foo.bar.com`
	noSeedDest := noSeedFile
	noSeedDest.seedFile = "fake.wim"

	tests := []struct {
		desc    string
		choice  string
		distros map[string]distribution
		out     Configuration
		want    error
	}{
		{
			desc:    "empty choice",
			distros: map[string]distribution{"foo": goodDistro},
			out:     Configuration{},
			want:    errDistro,
		},
		{
			desc:    "bad choice",
			choice:  "foo",
			distros: map[string]distribution{"bar": goodDistro},
			out:     Configuration{},
			want:    errDistro,
		},
		{
			desc:    "missing seedFile",
			choice:  "baz",
			distros: map[string]distribution{"baz": noSeedFile},
			out:     Configuration{},
			want:    errInput,
		},
		{
			desc:    "missing seedDest",
			choice:  "baz",
			distros: map[string]distribution{"baz": noSeedDest},
			out:     Configuration{},
			want:    errSeed,
		},
		{
			desc:    "good choice",
			choice:  "good",
			distros: map[string]distribution{"good": goodDistro},
			out:     Configuration{distro: &goodDistro},
			want:    nil,
		},
	}
	for _, tt := range tests {
		c := Configuration{}
		distributions = tt.distros
		got := c.addDistro(tt.choice)
		if err := cmpConfig(c, tt.out); err != nil {
			t.Errorf("%s: %v", tt.desc, err)
		}
		if got == tt.want {
			continue
		}
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: addDistro() got: '%v', want: '%v'", tt.desc, got, tt.want)
		}
	}
	distributions = distroDefaults // reset defaults for other tests
}

func TestAddDeviceList(t *testing.T) {
	tests := []struct {
		desc    string
		devices []string
		out     Configuration
		want    error
	}{
		{
			desc: "empty list",
			out:  Configuration{},
			want: errInput,
		},
		{
			desc:    "bad windows path",
			devices: []string{"f:", "g", "1"},
			out:     Configuration{},
			want:    errDevice,
		},
		{
			desc:    "bad linux path",
			devices: []string{"/dev/sda", "sdb1", "sdb"},
			out:     Configuration{},
			want:    errDevice,
		},
		{
			desc:    "good linux path",
			devices: []string{"sda"},
			out:     Configuration{devices: []string{"sda"}},
			want:    nil,
		},
		{
			desc:    "good darwin path",
			devices: []string{"disk3"},
			out:     Configuration{devices: []string{"disk3"}},
			want:    nil,
		},
		{
			desc:    "good windows path",
			devices: []string{"2"},
			out:     Configuration{devices: []string{"2"}},
			want:    nil,
		},
	}
	for _, tt := range tests {
		c := Configuration{}
		got := c.addDeviceList(tt.devices)
		if err := cmpConfig(c, tt.out); err != nil {
			t.Errorf("%s: %v", tt.desc, err)
		}
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: addDeviceList() got: '%v', want: '%v'", tt.desc, got, tt.want)
		}
	}
}

func TestAddSeedServer(t *testing.T) {
	tests := []struct {
		desc   string
		server string
		distro distribution
		out    Configuration
		want   error
	}{
		{
			desc:   "no override",
			distro: goodDistro,
			out:    Configuration{distro: &goodDistro},
			want:   nil,
		},
		{
			desc:   "bad fqdn",
			server: "test.foo@bar.com",
			distro: goodDistro,
			out:    Configuration{distro: &goodDistro},
			want:   errSeed,
		},
		{
			desc:   "good fqdn",
			server: "foo.bar.com",
			distro: goodDistro,
			out:    Configuration{distro: &goodDistro},
			want:   nil,
		},
	}
	for _, tt := range tests {
		c := Configuration{distro: &tt.distro}
		got := c.addSeedServer(tt.server)
		if err := cmpConfig(c, tt.out); err != nil {
			t.Errorf("%s: %v", tt.desc, err)
		}
		if got == tt.want {
			continue
		}
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: addSeedServer() got: '%v', want: '%v'", tt.desc, got, tt.want)
		}
	}
}

func TestValidateTrack(t *testing.T) {
	badDistro := distribution{
		imageServer: imageServer,
		images: map[string]string{
			"stable": "stable_installer.img",
		},
	}

	tests := []struct {
		desc  string
		track string
		maps  map[string]string
		out   string
		want  error
	}{
		{
			desc: "no default track",
			maps: badDistro.images,
			out:  "",
			want: errInput,
		},
		{
			desc: "empty track",
			maps: goodDistro.images,
			out:  "default",
			want: nil,
		},
		{
			desc:  "non-existent track",
			track: "foo",
			maps:  goodDistro.images,
			out:   "",
			want:  errTrack,
		},
		{
			desc:  "valid track",
			track: "stable",
			maps:  goodDistro.images,
			out:   "stable",
			want:  nil,
		},
	}
	for _, tt := range tests {
		got, err := validateTrack(tt.track, tt.maps)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: validateTrack() got: '%v', want: '%v'", tt.desc, got, tt.want)
		}
		if got != tt.out {
			t.Errorf("%s: validateTrack() got: '%v', want: '%v'", tt.desc, got, tt.out)
		}
	}
}

func TestDistro(t *testing.T) {
	want := "test"
	distro := distribution{name: want}
	c := Configuration{distro: &distro}
	if got := c.Distro(); got != want {
		t.Errorf("Distro() got: %q, want: %q", got, want)
	}
}

func TestDistroLabel(t *testing.T) {
	want := "testLabel"
	distro := distribution{label: want}
	c := Configuration{distro: &distro}
	if got := c.DistroLabel(); got != want {
		t.Errorf("DistroLabel() got: %q, want: %q", got, want)
	}
}

func TestTrack(t *testing.T) {
	want := "default"
	c := Configuration{track: want}
	if got := c.Track(); got != want {
		t.Errorf("Track() got: %q, want: %q", got, want)
	}
}

func TestImage(t *testing.T) {
	imageServer := `https://foo.bar.com`
	track := `default`
	distro := distribution{
		imageServer: imageServer,
		images: map[string]string{
			track: "test_installer.img",
		},
	}
	want := fmt.Sprintf(`%s/%s`, imageServer, distro.images[track])
	c := Configuration{track: track, distro: &distro}
	if got := c.ImagePath(); got != want {
		t.Errorf("ImagePath() got: %q, want: %q", got, want)
	}
}

func TestImageFile(t *testing.T) {
	tests := []struct {
		desc   string
		images map[string]string
		want   string
	}{
		{
			desc:   "iso",
			images: map[string]string{"default": "test_iso.iso"},
			want:   "test_iso.iso",
		},
		{
			desc:   "nested iso",
			images: map[string]string{"default": "nested/test_iso.iso"},
			want:   "test_iso.iso",
		},
		{
			desc:   "img",
			images: map[string]string{"default": "test_img.img"},
			want:   "test_img.img",
		},
		{
			desc:   "nested img",
			images: map[string]string{"default": "nested/test_img.img"},
			want:   "test_img.img",
		},
		{
			desc:   "compressed img",
			images: map[string]string{"default": "compressed-img.img.gz"},
			want:   "compressed-img.img.gz",
		},
		{
			desc:   "nested compressed img",
			images: map[string]string{"default": "nested/compressed-img.img.gz"},
			want:   "compressed-img.img.gz",
		},
	}
	for _, tt := range tests {
		c := Configuration{
			track: "default",
			distro: &distribution{
				imageServer: imageServer,
				images:      tt.images,
			},
		}
		got := c.ImageFile()
		if got != tt.want {
			t.Errorf("%s: ImageFile() got: %q, want: %q", tt.desc, got, tt.want)
		}
	}
}

func TestFFUConfFile(t *testing.T) {
	track := `default`
	distro := distribution{
		configs: map[string]string{
			track: "conf.yaml",
		},
	}
	want := "conf.yaml"
	c := Configuration{confTrack: track, distro: &distro}
	if got := c.FFUConfFile(); got != want {
		t.Errorf("FFUConfFile() got: %q, want: %q", got, want)
	}
}

func TestFFUConfPath(t *testing.T) {
	track := `default`
	distro := distribution{
		confServer: `https://foo.bar.com/configs/yaml`,
		configs: map[string]string{
			track: "conf.yaml",
		},
	}
	want := "https://foo.bar.com/configs/yaml/conf.yaml"
	c := Configuration{confTrack: track, distro: &distro}
	if got := c.FFUConfPath(); got != want {
		t.Errorf("FFUConfPath() got: %q, want: %q", got, want)
	}
}

func TestCleanup(t *testing.T) {
	want := true
	c := Configuration{cleanup: want}
	if got := c.Cleanup(); got != want {
		t.Errorf("Cleanup() got: %t, want: %t", got, want)
	}
}

func TestPowerOff(t *testing.T) {
	want := true
	c := Configuration{eject: want}
	if got := c.PowerOff(); got != want {
		t.Errorf("PowerOff() got: %t, want: %t", got, want)
	}
}

func TestWarning(t *testing.T) {
	want := true
	c := Configuration{warning: want}
	if got := c.Warning(); got != want {
		t.Errorf("Warning() got: %t, want: %t", got, want)
	}
}

func TestSeedServer(t *testing.T) {
	seedServer := `https://seed.foo.com`
	distro := distribution{
		seedServer: seedServer,
	}
	want := seedServer
	c := Configuration{distro: &distro}
	if got := c.SeedServer(); got != want {
		t.Errorf("SeedServer() got: %q, want: %q", got, want)
	}
}

func TestSeedFile(t *testing.T) {
	seedFile := `sources/base.wim`
	distro := distribution{
		seedFile: seedFile,
	}
	want := seedFile
	c := Configuration{distro: &distro}
	if got := c.SeedFile(); got != want {
		t.Errorf("SeedFile() got: %q, want: %q", got, want)
	}
}

func TestSeedDest(t *testing.T) {
	seedDest := "test"
	distro := distribution{
		seedDest: seedDest,
	}
	want := seedDest
	c := Configuration{distro: &distro}
	if got := c.SeedDest(); got != want {
		t.Errorf("SeedDest() got: %q, want: %q", got, want)
	}
}

func TestString(t *testing.T) {
	want := "test-distro"
	distro := distribution{
		name: want,
	}
	c := Configuration{distro: &distro}
	if got := c.String(); !strings.Contains(got, want) {
		t.Errorf("String() got: %q, want contains: %q", got, want)
	}
}
