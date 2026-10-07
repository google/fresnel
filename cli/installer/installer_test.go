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

package installer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/fresnel/cli/config"
	"github.com/google/fresnel/models"
	"github.com/google/go-cmp/cmp"
	"github.com/google/winops/storage"
)

// fakeConfig inherits all members of config.Configuration through embedding.
// Unimplemented members send a clear signal during tests because they will
// panic if called, allowing us to implement only the minimum set of members
// required for testing.
type fakeConfig struct {
	// config.Configuration is embedded, fakeConfig inherits all its members.
	config.Configuration

	dismount  bool
	eject     bool
	elevated  bool
	ffu       bool
	hasConfig bool
	update    bool
	err       error // the error returned when isElevated is called.

	confFile    string
	distroLabel string
	imagePath   string
	imageFile   string
	seedDest    string
	seedFile    string
	seedServer  string
	track       string
	ffuConfFile string
	ffuConfPath string
}

func (f *fakeConfig) NeedsConfig() bool {
	return f.hasConfig || f.ffu
}

func (f *fakeConfig) ConfFile() string {
	return f.confFile
}

func (f *fakeConfig) Dismount() bool {
	return f.dismount
}

func (f *fakeConfig) DistroLabel() string {
	return f.distroLabel
}

func (f *fakeConfig) ImagePath() string {
	return f.imagePath
}

func (f *fakeConfig) ImageFile() string {
	return f.imageFile
}

func (f *fakeConfig) Elevated() bool {
	return f.elevated
}

func (f *fakeConfig) PowerOff() bool {
	return f.eject
}

func (f *fakeConfig) SeedDest() string {
	return f.seedDest
}

func (f *fakeConfig) SeedFile() string {
	return f.seedFile
}

func (f *fakeConfig) SeedServer() string {
	return f.seedServer
}

func (f *fakeConfig) UpdateOnly() bool {
	return f.update
}

func (f *fakeConfig) FFU() bool {
	return f.ffu
}

func (f *fakeConfig) FFUConfFile() string {
	return f.ffuConfFile
}

func (f *fakeConfig) FFUConfPath() string {
	return f.ffuConfPath
}

func TestNew(t *testing.T) {
	// Generate a fake config to use with New.
	c := &fakeConfig{
		imagePath:  `https://foo.bar.com/test_installer.img`,
		seedServer: `https://bar.baz.com/endpoint`,
	}
	tests := []struct {
		desc          string
		config        Configuration
		fakeConnect   func(string, string) (httpDoer, error)
		wantInstaller bool
		err           error
	}{
		{
			desc:   "nil config",
			config: nil,
			err:    errConfig,
		},
		{
			desc:        "connect error",
			config:      c,
			fakeConnect: func(string, string) (httpDoer, error) { return nil, errors.New("error") },
			err:         errConnect,
		},
		{
			desc:          "success",
			config:        c,
			fakeConnect:   func(string, string) (httpDoer, error) { return nil, nil },
			wantInstaller: true,
			err:           nil,
		},
	}
	for _, tt := range tests {
		connect = tt.fakeConnect
		got, err := New(tt.config)
		if !errors.Is(err, tt.err) {
			t.Errorf("%s: New() err: %v, want err: %v", tt.desc, err, tt.err)
		}
		if (got == nil) == tt.wantInstaller {
			t.Errorf("%s: New() got: %t, want: %t", tt.desc, (got != nil), tt.wantInstaller)
		}
	}
}

func TestUserName(t *testing.T) {
	// stdUser represents the user actually running the binary.
	stdUser := "stdUser"

	tests := []struct {
		desc            string
		fakeCurrentUser func() (*user.User, error)
		want            string
		err             error
	}{
		{
			desc:            "user.Current error",
			fakeCurrentUser: func() (*user.User, error) { return nil, errUser },
			want:            "",
			err:             errUser,
		},
		{
			desc:            "as root",
			fakeCurrentUser: func() (*user.User, error) { return &user.User{Username: "root"}, nil },
			want:            stdUser,
			err:             nil,
		},
		{
			desc:            "as user",
			fakeCurrentUser: func() (*user.User, error) { return &user.User{Username: stdUser}, nil },
			want:            stdUser,
			err:             nil,
		},
	}
	if err := os.Setenv("SUDO_USER", stdUser); err != nil {
		t.Fatalf(`os.SetEnv("SUDO_USER", %q) returned %v`, stdUser, err)
	}
	for _, tt := range tests {
		currentUser = tt.fakeCurrentUser
		got, err := username()
		if !errors.Is(err, tt.err) {
			t.Errorf("%s: isRoot() err: %v, want err: %v", tt.desc, err, tt.err)
		}
		if got != tt.want {
			t.Errorf("%s: isRoot() got: %q, want: %q", tt.desc, got, tt.want)
		}
	}
	// Cleanup
	if err := os.Unsetenv("SUDO_USER"); err != nil {
		t.Errorf(`os.UnsetEnv("SUDO_USER") returned %v`, err)
	}
}

func TestRetrieve(t *testing.T) {
	// Setup a temp folder.
	fakeCache, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf(`ioutil.TempDir("", "test") returned %v`, err)
	}

	tests := []struct {
		desc      string
		installer *Installer
		download  func(client httpDoer, path string, w io.Writer) error
		want      error
	}{
		{
			desc:      "missing image path",
			installer: &Installer{config: &fakeConfig{}},
			want:      errConfig,
		},
		{
			desc: "missing yaml config",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath:   `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				ffu:         true,
				ffuConfFile: "",
				ffuConfPath: "",
			}},
			want: errConfName,
		},
		{
			desc: "missing yaml path",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath:   `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				ffu:         true,
				ffuConfFile: "conf.yaml",
				ffuConfPath: "",
			}},
			want: errConfPath,
		},
		{
			desc: "missing cache",
			installer: &Installer{config: &fakeConfig{
				imagePath: `https://foo.bar.com/test_installer.img`,
				imageFile: `test_installer.img`,
				ffu:       true,
			}},
			want: errCache,
		},
		{
			desc: "download success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath:   `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				ffu:         true,
				ffuConfPath: "https://foo.bar.com/told/conf.yaml",
				ffuConfFile: "conf.yaml",
			}},
			download: func(client httpDoer, path string, w io.Writer) error { return nil },
			want:     nil,
		},
		{
			desc: "non-ffu with hasConfig download success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath:   `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				hasConfig:   true,
				ffuConfPath: "https://foo.bar.com/config/startimage.yaml",
				ffuConfFile: "startimage.yaml",
			}},
			download: func(client httpDoer, path string, w io.Writer) error { return nil },
			want:     nil,
		},
		{
			desc: "non-ffu with hasConfig missing yaml config",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath:   `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				hasConfig:   true,
				ffuConfFile: "",
				ffuConfPath: "",
			}},
			want: errConfName,
		},
		{
			desc: "non-ffu with hasConfig missing yaml path",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath:   `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				hasConfig:   true,
				ffuConfFile: "startimage.yaml",
				ffuConfPath: "",
			}},
			want: errConfPath,
		},
		{
			desc: "non-ffu without hasConfig downloads only image",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imagePath: `https://foo.bar.com/test_installer.img`,
				imageFile: `test_installer.img`,
				hasConfig: false,
			}},
			download: func(client httpDoer, path string, w io.Writer) error { return nil },
			want:     nil,
		},
	}
	for _, tt := range tests {
		downloadFile = tt.download
		got := tt.installer.Retrieve()
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: Retrieve() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
	// Cleanup
	if err := os.RemoveAll(fakeCache); err != nil {
		t.Errorf(`cleanup of %q returned %v`, fakeCache, err)
	}
}

func TestRetrieveFile(t *testing.T) {

	// Setup a temp folder.
	fakeCache, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf(`ioutil.TempDir("", "test") returned %v`, err)
	}

	tests := []struct {
		desc      string
		filePath  string
		fileName  string
		installer *Installer
		doer      func() (httpDoer, error)
		download  func(client httpDoer, path string, w io.Writer) error
		want      error
	}{
		{
			desc:      "connection error",
			filePath:  "https://foo.bar.com/test_installer.img",
			fileName:  "test_installer.img",
			installer: &Installer{cache: fakeCache},
			doer:      func() (httpDoer, error) { return &fakeHTTPDoer{}, errConnect },
			download:  func(client httpDoer, path string, w io.Writer) error { return nil },
			want:      errConnect,
		},
		{
			desc:      "download failure",
			filePath:  "https://foo.bar.com/test_installer.img",
			fileName:  "test_installer.img",
			installer: &Installer{cache: fakeCache},
			doer:      func() (httpDoer, error) { return &fakeHTTPDoer{}, nil },
			download:  func(client httpDoer, path string, w io.Writer) error { return errDownload },
			want:      errDownload,
		},
		{
			desc:      "download success",
			filePath:  "https://foo.bar.com/test_installer.img",
			fileName:  "test_installer.img",
			installer: &Installer{cache: fakeCache},
			doer:      func() (httpDoer, error) { return &fakeHTTPDoer{}, nil },
			download:  func(client httpDoer, path string, w io.Writer) error { return nil },
			want:      nil,
		},
	}
	for _, tt := range tests {
		downloadFile = tt.download
		connectWithCert = tt.doer
		got := tt.installer.retrieveFile(tt.fileName, tt.filePath)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: retrieveFile() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
	// Cleanup
	if err := os.RemoveAll(fakeCache); err != nil {
		t.Errorf(`cleanup of %q returned %v`, fakeCache, err)
	}
}

// fakeHTTPDoer serves as a replacemenet for an http client for testing.
// The contents of body are returned when the Do is called. This method
// is used instead of httptest as a workaround for b/122585482.
type fakeHTTPDoer struct {
	statusCode int
	body       []byte
	err        error
}

// Do provides the contents of fakeHTTPDoer.body as an http.Response by
// wrapping it with an io.ReadCloser.
func (c *fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	reader := bytes.NewReader(c.body)
	readCloser := ioutil.NopCloser(reader)
	return &http.Response{StatusCode: c.statusCode, Body: readCloser}, c.err
}

// fakeWriter serves as a replacement for an io.Writer for testing.
type fakeWriter struct {
	err error
}

func (f *fakeWriter) Write(p []byte) (n int, err error) {
	return 0, f.err
}

func TestDownload(t *testing.T) {
	path := "http://foo.bar.com/source/image.img"

	tests := []struct {
		desc   string
		doer   httpDoer
		path   string
		writer io.Writer
		want   error
	}{
		{
			desc: "missing client",
			want: errConnect,
		},
		{
			desc: "missing path",
			doer: &fakeHTTPDoer{},
			want: errInput,
		},
		{
			desc: "missing writer",
			doer: &fakeHTTPDoer{},
			path: path,
			want: errFile,
		},
		{
			desc:   "doer failure",
			doer:   &fakeHTTPDoer{err: errDownload},
			path:   path,
			writer: &fakeWriter{},
			want:   errDownload,
		},
		{
			desc:   "failed response code",
			doer:   &fakeHTTPDoer{statusCode: http.StatusForbidden},
			path:   path,
			writer: &fakeWriter{},
			want:   errStatus,
		},
	}
	for _, tt := range tests {
		got := download(tt.doer, tt.path, tt.writer)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: download() got: %v, want: %v", tt.desc, got, tt.want)
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

	part partition

	dmErr     error
	ejectErr  error
	detectErr error
	partErr   error
	selErr    error
	wipeErr   error
	writeErr  error
}

func (f *fakeDevice) Dismount() error {
	return f.dmErr
}

func (f *fakeDevice) Eject() error {
	return f.ejectErr
}

func (f *fakeDevice) Partition(label string) error {
	return f.partErr
}

func (f *fakeDevice) DetectPartitions(bool) error {
	return f.detectErr
}

func (f *fakeDevice) SelectPartition(uint64, storage.FileSystem) (*storage.Partition, error) {
	return nil, f.partErr
}

func (f *fakeDevice) Wipe() error {
	return f.wipeErr
}

// fakePartition represents storage.Partition.
type fakePartition struct {
	contents []string
	id       string
	label    string
	mount    string
	mountErr error
	err      error
}

func (f *fakePartition) Contents() ([]string, error) {
	return f.contents, f.err
}

func (f *fakePartition) Identifier() string {
	return f.id
}

func (f *fakePartition) Erase() error {
	return f.err
}

func (f *fakePartition) Mount(string) error {
	return f.mountErr
}

func (f *fakePartition) Format(label string) error {
	return f.err
}

func (f *fakePartition) Label() string {
	return f.label
}

func (f *fakePartition) MountPoint() string {
	return f.mount
}

func TestPrepare(t *testing.T) {
	// Prepare stand-ins for an image file.
	badImage, err := ioutil.TempDir("", "bad.txt")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', 'bad.txt') returned %v", err)
	}
	goodISO, err := ioutil.TempDir("", "good.iso")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', 'good.iso') returned %v", err)
	}
	goodRaw, err := ioutil.TempDir("", "good.img")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', 'good.img') returned %v", err)
	}

	tests := []struct {
		desc      string
		installer *Installer
		config    Configuration
		device    Device
		selPart   func(Device, uint64, storage.FileSystem) (partition, error)

		want error
	}{
		{
			desc:      "missing config",
			installer: &Installer{},
			device:    &fakeDevice{},
			want:      errConfig,
		},
		{
			desc:      "missing image file",
			installer: &Installer{config: &fakeConfig{}},
			device:    &fakeDevice{},
			want:      errInput,
		},
		{
			desc:      "no image file extension",
			installer: &Installer{config: &fakeConfig{imageFile: "bad"}},
			device:    &fakeDevice{},
			want:      errFile,
		},
		{
			desc:      "missing download",
			installer: &Installer{config: &fakeConfig{imageFile: "nonexistent.img"}},
			device:    &fakeDevice{},
			want:      errPath,
		},
		{
			desc:      "not a supported image",
			installer: &Installer{config: &fakeConfig{imageFile: badImage}},
			device:    &fakeDevice{},
			want:      errProvision,
		},
		{
			desc:      "prepare for raw failure",
			installer: &Installer{config: &fakeConfig{imageFile: goodRaw, elevated: true}},
			device:    &fakeDevice{dmErr: errProvision},
			want:      errProvision,
		},
		{
			desc:      "prepare for raw success",
			installer: &Installer{config: &fakeConfig{imageFile: goodRaw, elevated: true}},
			device:    &fakeDevice{},
			want:      nil,
		},
		{
			desc:      "prepare for iso with elevation failure",
			installer: &Installer{config: &fakeConfig{imageFile: goodISO, elevated: true}},
			device:    &fakeDevice{wipeErr: errors.New("error")},
			want:      errWipe,
		},
		{
			desc:      "prepare for iso with elevation success",
			installer: &Installer{config: &fakeConfig{distroLabel: "test", imageFile: goodISO, elevated: true}},
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{}, nil },
			want:      nil,
		},
		{
			desc:      "prepare for iso without elevation failure",
			installer: &Installer{config: &fakeConfig{imageFile: goodISO, elevated: false, update: true}},
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return nil, errors.New("error") },
			want:      errPartition,
		},
		{
			desc:      "prepare for iso without elevation success",
			installer: &Installer{config: &fakeConfig{distroLabel: "test", imageFile: goodISO, elevated: false, update: true}},
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{}, nil },
			want:      nil,
		},
	}
	for _, tt := range tests {
		selectPart = tt.selPart
		got := tt.installer.Prepare(tt.device)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: Prepare() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

func TestPrepareForISOWithElevation(t *testing.T) {
	tests := []struct {
		desc      string
		installer *Installer
		device    *fakeDevice
		selPart   func(Device, uint64, storage.FileSystem) (partition, error)
		want      error
	}{
		{
			desc:      "elevation missing",
			installer: &Installer{config: &fakeConfig{}},
			device:    &fakeDevice{wipeErr: errors.New("error")},
			want:      errElevation,
		},
		{
			desc:      "wipe error",
			installer: &Installer{config: &fakeConfig{elevated: true}},
			device:    &fakeDevice{wipeErr: errors.New("error")},
			want:      errWipe,
		},
		{
			desc:      "partition error",
			installer: &Installer{config: &fakeConfig{elevated: true}},
			device:    &fakeDevice{partErr: errors.New("error")},
			want:      errPartition,
		},
		{
			desc:      "SelectPartition error",
			installer: &Installer{config: &fakeConfig{elevated: true}},
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return nil, errors.New("error") },
			want: func() error {
				if runtime.GOOS != "darwin" {
					return errPrepare
				}
				return nil
			}(),
		},
		{
			desc:      "format error",
			installer: &Installer{config: &fakeConfig{elevated: true}},
			device:    &fakeDevice{},
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{err: errors.New("error")}, nil
			},
			want: func() error {
				if runtime.GOOS != "darwin" {
					return errFormat
				}
				return nil
			}(),
		},
		{
			desc:      "success",
			installer: &Installer{config: &fakeConfig{elevated: true}},
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{}, nil },
			want:      nil,
		},
	}
	for _, tt := range tests {
		selectPart = tt.selPart
		got := tt.installer.prepareForISOWithElevation(tt.device, uint64(1024))
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: prepareForISOWithElevation() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

func TestPrepareForISOWithoutElevation(t *testing.T) {
	tests := []struct {
		desc      string
		installer *Installer
		selPart   func(Device, uint64, storage.FileSystem) (partition, error)
		want      error
	}{
		{
			desc:      "SelectPartition error",
			installer: &Installer{config: &fakeConfig{}},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return nil, errors.New("error") },
			want:      errPartition,
		},
		{
			desc:      "mount error",
			installer: &Installer{config: &fakeConfig{}},
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{mountErr: errors.New("error")}, nil
			},
			want: errMount,
		},
		{
			desc:      "erase error",
			installer: &Installer{config: &fakeConfig{}},
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{err: errors.New("error")}, nil
			},
			want: errWipe,
		},
		{
			desc:      "success",
			installer: &Installer{config: &fakeConfig{}},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{}, nil },
			want:      nil,
		},
	}
	for _, tt := range tests {
		selectPart = tt.selPart
		got := tt.installer.prepareForISOWithoutElevation(&fakeDevice{}, uint64(1024))
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: prepareForISOWithoutElevation() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

func TestPrepareForRaw(t *testing.T) {
	tests := []struct {
		desc   string
		device *fakeDevice
		want   error
	}{
		{
			desc:   "dismount error",
			device: &fakeDevice{dmErr: ErrLabel},
			want:   ErrLabel,
		},
		{
			desc:   "success",
			device: &fakeDevice{},
			want:   nil,
		},
	}
	for _, tt := range tests {
		installer := &Installer{}
		got := installer.prepareForRaw(tt.device)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: prepareForRaw() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

// fakeHandler reprsents iso.Handler for testing. We do not use embedding here
// because we must abstract a concrete return from Mount using an interface.
type fakeHandler struct {
	contents []string
	mount    string
	path     string
	err      error
}

func (f *fakeHandler) Contents() []string {
	return f.contents
}

func (f *fakeHandler) Copy(string) error {
	return f.err
}

func (f *fakeHandler) Dismount() error {
	return f.err
}

func (f *fakeHandler) ImagePath() string {
	return f.path
}

func (f *fakeHandler) MountPath() string {
	return f.mount
}

func (f *fakeHandler) Size() uint64 {
	return 1
}

func TestProvision(t *testing.T) {
	// A fake cache for testing.
	fakeCache, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', '') returned %v", err)
	}
	// A fake image for testing.
	fakeImagePath := filepath.Join(fakeCache, "fake.iso")
	if _, err := os.Create(fakeImagePath); err != nil {
		t.Fatalf("os.Create(%q) returned %v", fakeImagePath, err)
	}
	fakeConfPath := filepath.Join(fakeCache, "fake_conf.yaml")
	if _, err := os.Create(fakeConfPath); err != nil {
		t.Fatalf("os.Create(%q) returned %v", fakeConfPath, err)
	}
	defer os.RemoveAll(fakeCache)

	tests := []struct {
		desc      string
		installer *Installer
		mount     func(string) (isoHandler, error)
		selPart   func(Device, uint64, storage.FileSystem) (partition, error)
		writeISO  func(isoHandler, partition) error
		want      error
	}{
		{
			desc:      "missing config",
			installer: &Installer{},
			want:      errConfig,
		},
		{
			desc:      "missing cache",
			installer: &Installer{config: &fakeConfig{}},
			want:      errCache,
		},
		{
			desc:      "missing image file",
			installer: &Installer{cache: "/fake/path", config: &fakeConfig{}},
			want:      errInput,
		},
		{
			desc:      "no image file extension",
			installer: &Installer{cache: "/fake/path", config: &fakeConfig{imageFile: "bad"}},
			want:      errFile,
		},
		{
			desc:      "image file missing from cache",
			installer: &Installer{cache: "/fake/path", config: &fakeConfig{imageFile: "missing.iso"}},
			want:      errPath,
		},
		{
			desc:      "provision ISO error",
			installer: &Installer{cache: "/fake/path", config: &fakeConfig{imageFile: "fake.iso"}},
			want:      errPath,
		},
		{
			desc: "hasConfig config file missing from cache",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imageFile:   "fake.iso",
				hasConfig:   true,
				ffuConfFile: "missing_conf.yaml",
			}},
			want: errPath,
		},
		{
			desc: "hasConfig success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imageFile:   "fake.iso",
				hasConfig:   true,
				ffuConfFile: "fake_conf.yaml",
				seedDest:    "oci",
			}},
			mount: func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{label: "test", id: "testid", mount: fakeCache}, nil
			},
			writeISO: func(isoHandler, partition) error { return nil },
			want:     nil,
		},
		{
			desc:      "success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{label: "test", id: "testid"}, nil
			},
			writeISO: func(isoHandler, partition) error { return nil },
			want:     nil,
		},
	}
	for _, tt := range tests {
		mount = tt.mount
		writeISOFunc = tt.writeISO
		if tt.selPart != nil {
			selectPart = tt.selPart
		}
		got := tt.installer.Provision(&fakeDevice{})
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: Provision() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

func TestProvisionISO(t *testing.T) {
	// A fake cache for testing.
	fakeCache, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', '') returned %v", err)
	}
	// A fake image for testing.
	fakeImagePath := filepath.Join(fakeCache, "fake.iso")
	if _, err := os.Create(fakeImagePath); err != nil {
		t.Fatalf("os.Create(%q) returned %v", fakeImagePath, err)
	}
	fakeConfPath := filepath.Join(fakeCache, "fake_conf.yaml")
	if _, err := os.Create(fakeConfPath); err != nil {
		t.Fatalf("os.Create(%q) returned %v", fakeConfPath, err)
	}
	defer os.RemoveAll(fakeCache)

	tests := []struct {
		desc      string
		installer *Installer
		device    *fakeDevice
		mount     func(string) (isoHandler, error)
		selPart   func(Device, uint64, storage.FileSystem) (partition, error)
		writeISO  func(isoHandler, partition) error
		want      error
	}{
		{
			desc:      "mount error",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{}, errors.New("error") },
			want:      errMount,
		},
		{
			desc:      "select partition error",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			device:    &fakeDevice{},
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{label: "test"}, errors.New("error")
			},
			want: errPartition,
		},
		{
			desc:      "writeISO error",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{label: "test"}, nil },
			writeISO:  func(isoHandler, partition) error { return errPath },
			want:      errProvision,
		},
		{
			desc:      "dismount deferred error",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{err: errIO}, nil },
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{label: "test"}, nil },
			writeISO:  func(isoHandler, partition) error { return nil },
			want:      errIO,
		},
		{
			desc: "writeConfig error with hasConfig",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imageFile:   "fake.iso",
				hasConfig:   true,
				ffuConfFile: "missing.yaml",
			}},
			mount:    func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			device:   &fakeDevice{},
			selPart:  func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{label: "test"}, nil },
			writeISO: func(isoHandler, partition) error { return nil },
			want:     errIO,
		},
		{
			desc: "success with hasConfig",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				imageFile:   "fake.iso",
				hasConfig:   true,
				ffuConfFile: "fake_conf.yaml",
				seedDest:    "oci",
			}},
			mount:  func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			device: &fakeDevice{},
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{label: "test", mount: fakeCache}, nil
			},
			writeISO: func(isoHandler, partition) error { return nil },
			want:     nil,
		},
		{
			desc:      "success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			device:    &fakeDevice{},
			selPart:   func(Device, uint64, storage.FileSystem) (partition, error) { return &fakePartition{label: "test"}, nil },
			writeISO:  func(isoHandler, partition) error { return nil },
			want:      nil,
		},
	}
	for _, tt := range tests {
		mount = tt.mount
		writeISOFunc = tt.writeISO
		selectPart = tt.selPart
		got := tt.installer.provisionISO(tt.device)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: provisionISO() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

// TestWriteConfig verifies the destination, content, permissions, and failure
// modes of writeConfig, including removal of the counterpart config file.
func TestWriteConfig(t *testing.T) {
	const confFileName = "test_config.yaml"
	confContent := []byte("os_code: test-os-stable\ntrack: stable\nmanaged: true\n")
	staleContent := []byte("stale: true\n")

	// writeFile creates a file and any missing parent directories.
	writeFile := func(t *testing.T, path string, content []byte, perm os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("os.MkdirAll(%q) returned %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, content, perm); err != nil {
			t.Fatalf("os.WriteFile(%q) returned %v", path, err)
		}
	}

	tests := []struct {
		desc string
		ffu  bool
		// seedDest overrides the default "/oci" destination when set.
		seedDest string
		// confFile overrides the default cached config file name when set.
		confFile string
		// emptySource caches an empty config file instead of confContent.
		emptySource bool
		// needsPermEnforcement marks cases that rely on file permission checks,
		// which are not enforced on Windows or for root, so they are skipped there.
		needsPermEnforcement bool
		// setup prepares the destination directory before writeConfig runs.
		setup   func(t *testing.T, dest string)
		wantErr error
		// wantFile is the file expected to hold the config on success.
		wantFile string
		// wantAbsent lists files in dest that must not exist afterwards.
		wantAbsent []string
	}{
		{
			desc:       "FFU writes startimage.yaml to a fresh mount",
			ffu:        true,
			wantFile:   confDestFile,
			wantAbsent: []string{bootConfDestFile},
		},
		{
			desc:       "non-FFU writes bootconfig.yaml to a fresh mount",
			wantFile:   bootConfDestFile,
			wantAbsent: []string{confDestFile},
		},
		{
			desc: "target directory already exists",
			ffu:  true,
			setup: func(t *testing.T, dest string) {
				if err := os.MkdirAll(dest, 0755); err != nil {
					t.Fatalf("os.MkdirAll(%q) returned %v", dest, err)
				}
			},
			wantFile: confDestFile,
		},
		{
			desc: "target file with stale content is overwritten",
			ffu:  true,
			setup: func(t *testing.T, dest string) {
				writeFile(t, filepath.Join(dest, confDestFile), staleContent, 0644)
			},
			wantFile: confDestFile,
		},
		{
			desc: "FFU removes stale bootconfig.yaml",
			ffu:  true,
			setup: func(t *testing.T, dest string) {
				writeFile(t, filepath.Join(dest, bootConfDestFile), staleContent, 0644)
			},
			wantFile:   confDestFile,
			wantAbsent: []string{bootConfDestFile},
		},
		{
			desc: "non-FFU removes stale startimage.yaml",
			setup: func(t *testing.T, dest string) {
				writeFile(t, filepath.Join(dest, confDestFile), staleContent, 0644)
			},
			wantFile:   bootConfDestFile,
			wantAbsent: []string{confDestFile},
		},
		{
			desc:        "empty config file in cache",
			ffu:         true,
			emptySource: true,
			wantFile:    confDestFile,
		},
		{
			desc:     "deep nested destination directory",
			ffu:      true,
			seedDest: "/deep/nested/custom/oci",
			wantFile: confDestFile,
		},
		{
			desc:       "source config missing from cache",
			ffu:        true,
			confFile:   "nonexistent.yaml",
			wantErr:    errIO,
			wantAbsent: []string{confDestFile},
		},
		{
			desc: "target directory path blocked by file",
			ffu:  true,
			setup: func(t *testing.T, dest string) {
				writeFile(t, dest, []byte("blocking file"), 0644)
			},
			wantErr: errPerm,
		},
		{
			desc: "FFU stale bootconfig.yaml cannot be removed",
			ffu:  true,
			setup: func(t *testing.T, dest string) {
				// A non-empty directory at the stale path makes os.Remove fail on every OS.
				writeFile(t, filepath.Join(dest, bootConfDestFile, "child"), staleContent, 0644)
			},
			wantErr:    errIO,
			wantAbsent: []string{confDestFile},
		},
		{
			desc: "non-FFU stale startimage.yaml cannot be removed",
			setup: func(t *testing.T, dest string) {
				// A non-empty directory at the stale path makes os.Remove fail on every OS.
				writeFile(t, filepath.Join(dest, confDestFile, "child"), staleContent, 0644)
			},
			wantErr:    errIO,
			wantAbsent: []string{bootConfDestFile},
		},
		{
			desc:                 "read only destination directory",
			ffu:                  true,
			needsPermEnforcement: true,
			setup: func(t *testing.T, dest string) {
				if err := os.MkdirAll(dest, 0555); err != nil {
					t.Fatalf("os.MkdirAll(%q) returned %v", dest, err)
				}
				t.Cleanup(func() { os.Chmod(dest, 0755) })
			},
			wantErr:    errIO,
			wantAbsent: []string{confDestFile},
		},
		{
			desc:                 "read only destination file",
			ffu:                  true,
			needsPermEnforcement: true,
			setup: func(t *testing.T, dest string) {
				path := filepath.Join(dest, confDestFile)
				writeFile(t, path, []byte("readonly"), 0444)
				t.Cleanup(func() { os.Chmod(path, 0644) })
			},
			wantErr: errIO,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if tt.needsPermEnforcement && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("permission checks are not enforced on Windows or for root")
			}
			cache := t.TempDir()
			mount := t.TempDir()
			source := confContent
			if tt.emptySource {
				source = []byte{}
			}
			writeFile(t, filepath.Join(cache, confFileName), source, 0644)
			seedDest := "/oci"
			if tt.seedDest != "" {
				seedDest = tt.seedDest
			}
			confFile := confFileName
			if tt.confFile != "" {
				confFile = tt.confFile
			}
			dest := filepath.Join(mount, filepath.FromSlash(seedDest))
			if tt.setup != nil {
				tt.setup(t, dest)
			}

			inst := &Installer{cache: cache, config: &fakeConfig{ffu: tt.ffu, ffuConfFile: confFile, seedDest: seedDest}}
			err := inst.writeConfig(&fakePartition{mount: mount})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("writeConfig() returned %v, want %v", err, tt.wantErr)
			}

			if tt.wantFile != "" {
				path := filepath.Join(dest, tt.wantFile)
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("os.ReadFile(%q) returned %v", path, err)
				}
				if string(got) != string(source) {
					t.Errorf("writeConfig() wrote %q to %s, want %q", got, tt.wantFile, source)
				}
				if runtime.GOOS != "windows" {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatalf("os.Stat(%q) returned %v", path, err)
					}
					// The process umask may clear group or other bits from the
					// requested 0644 (for example 0640 under umask 027), so only
					// require owner read/write and nothing beyond 0644.
					if mode := info.Mode().Perm(); mode&0600 != 0600 || mode&^0644 != 0 {
						t.Errorf("%s permissions = %v, want owner read/write and no bits beyond 0644", tt.wantFile, mode)
					}
				}
			}
			for _, name := range tt.wantAbsent {
				path := filepath.Join(dest, name)
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("os.Stat(%q) returned %v, want the file to be absent", path, err)
				}
			}
		})
	}
}

// fakeISO represents iso.Handler. It inherits all members of iso.Handler
// through embedding. Unimplemented members send a clear signal during tests
// because they will panic if called, allowing us to implement only the minimum
// set of members required for testing.
type fakeISO struct {
	contents  []string
	copyErr   error
	imagePath string
	mount     string
	size      uint64
}

func (f *fakeISO) Size() uint64 {
	return f.size
}

func (f *fakeISO) MountPath() string {
	return f.mount
}

func (f *fakeISO) Contents() []string {
	return f.contents
}

func (f *fakeISO) Copy(dest string) error {
	return f.copyErr
}

func (f *fakeISO) Dismount() error {
	return nil
}
func (f *fakeISO) ImagePath() string {
	return f.imagePath
}

// fakeFileSystems returns a fake mount point and contents for testing
// purposes to simulate mounted filesystems. The caller is responsible
// for cleaning up the folders after their tests are complete.
func fakeFileSystems() (string, []string, error) {
	m, err := ioutil.TempDir("", "")
	if err != nil {
		return "", []string{}, fmt.Errorf("ioutil.TempDir() returned %v", err)
	}
	c := []string{"one", "two", "three"}
	return m, c, nil
}

func TestWriteISO(t *testing.T) {
	// Temp folders representing file system contents.
	mount, contents, err := fakeFileSystems()
	if err != nil {
		t.Fatalf("fakeFileSystems() returned %v", err)
	}
	defer os.RemoveAll(mount)

	tests := []struct {
		desc string
		part partition
		iso  isoHandler
		want error
	}{
		{
			desc: "empty partition",
			part: nil,
			want: errPartition,
		},
		{
			desc: "partition not mounted",
			part: &fakePartition{},
			iso:  &fakeISO{},
			want: errMount,
		},
		{
			desc: "partition not empty",
			part: &fakePartition{mount: mount, contents: contents},
			iso:  &fakeISO{},
			want: errNotEmpty,
		},
		{
			desc: "iso not mounted",
			part: &fakePartition{mount: mount},
			iso:  &fakeISO{},
			want: errInput,
		},
		{
			desc: "empty iso",
			part: &fakePartition{mount: mount},
			iso:  &fakeISO{mount: `/fake/path`},
			want: errEmpty,
		},
	}

	for _, tt := range tests {
		got := writeISO(tt.iso, tt.part)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: WriteISO got = %q, want = %q", tt.desc, got, tt.want)
		}
	}
}

func TestWriteSeed(t *testing.T) {
	// Create a temporary file and folder for the test.
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf(`ioutil.TempDir("","") returned %v`, err)
	}
	filePath := filepath.Join(tempDir, "fake.wim")
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("os.Create(%q) returned %v", filePath, err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("test content")); err != nil {
		t.Fatalf("failed to write to %q with %v", f.Name(), err)
	}
	// A fake seed response.
	good, err := json.Marshal(&models.SeedResponse{ErrorCode: models.StatusSuccess})
	if err != nil {
		t.Fatalf("json.Marshal of good request returned %v", err)
	}

	tests := []struct {
		desc        string
		installer   *Installer
		fakeConnect func(string, string) (httpDoer, error)
		handler     *fakeHandler
		part        *fakePartition
		want        error
	}{
		{
			desc: "not mounted",
			part: &fakePartition{label: "Test"},
			want: errInput,
		},
		{
			desc:      "file hash error",
			installer: &Installer{config: &fakeConfig{}},
			handler:   &fakeHandler{},
			part:      &fakePartition{label: "Test", mount: tempDir},
			want:      errFile,
		},
		{
			desc:        "connect error",
			installer:   &Installer{config: &fakeConfig{}},
			fakeConnect: func(string, string) (httpDoer, error) { return nil, errors.New("error") },
			handler:     &fakeHandler{},
			part:        &fakePartition{label: "Test", mount: tempDir},
			want:        errFile,
		},
		{
			desc:        "seed request error",
			installer:   &Installer{config: &fakeConfig{seedServer: `:`, seedFile: f.Name()}},
			fakeConnect: func(string, string) (httpDoer, error) { return nil, nil },
			handler:     &fakeHandler{},
			part:        &fakePartition{label: "Test", mount: tempDir},
			want:        errDownload,
		},
		{
			desc: "success",
			installer: &Installer{
				config: &fakeConfig{
					seedDest:   "test",
					seedFile:   "fake.wim",
					seedServer: `https://foo.bar.com/seed`,
				},
			},
			fakeConnect: func(string, string) (httpDoer, error) { return &fakeHTTPDoer{body: good}, nil },
			handler:     &fakeHandler{mount: tempDir},
			part:        &fakePartition{label: "Test", mount: tempDir},
			want:        nil,
		},
	}
	for _, tt := range tests {
		connect = tt.fakeConnect
		got := tt.installer.writeSeed(tt.handler, tt.part)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: writeSeed() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

func TestFileHash(t *testing.T) {
	// Create a temporary file to test hashing.
	f, err := ioutil.TempFile("", "")
	if err != nil {
		t.Fatalf(`ioutil.TempFile("","") returned %v`, err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("test content")); err != nil {
		t.Fatalf("failed to write to %q with %v", f.Name(), err)
	}
	tempFile := f.Name()

	tests := []struct {
		desc string
		path string
		out  []byte
		want error
	}{
		{
			desc: "empty path",
			want: errInput,
		},
		{
			desc: "bad path",
			path: "nonexistent.iso",
			want: errPath,
		},
		{
			desc: "good path",
			path: tempFile,
			out:  []byte{106, 232, 167, 85, 85, 32, 159, 214, 196, 65, 87, 192, 174, 216, 1, 110, 118, 63, 244, 53, 161, 156, 241, 134, 247, 104, 99, 20, 1, 67, 255, 114},
			want: nil,
		},
	}
	for _, tt := range tests {
		out, got := fileHash(tt.path)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: fileHash() err: %v, want: %v", tt.desc, got, tt.want)
		}
		if bytes.Compare(out, tt.out) != 0 {
			t.Errorf("%s: Compare(%q, %q) = %d, want: %d", tt.desc, hex.EncodeToString(out), hex.EncodeToString(tt.out), bytes.Compare(out, tt.out), 0)
		}
	}
}

func TestSeedRequest(t *testing.T) {
	// Model a bad response and a good response for testing.
	bad, err := json.Marshal(&models.SeedResponse{ErrorCode: models.StatusSignError})
	if err != nil {
		t.Fatalf("json.Marshal of bad request returned %v", err)
	}
	good, err := json.Marshal(&models.SeedResponse{ErrorCode: models.StatusSuccess})
	if err != nil {
		t.Fatalf("json.Marshal of good request returned %v", err)
	}

	tests := []struct {
		desc   string
		client *fakeHTTPDoer
		hash   string
		config *fakeConfig
		out    *models.SeedResponse
		want   error
	}{
		{
			desc:   "build request error",
			hash:   "123",
			config: &fakeConfig{seedServer: `:`},
			want:   errConnect,
		},
		{
			desc:   "post error",
			client: &fakeHTTPDoer{err: errors.New("error")},
			hash:   "123",
			config: &fakeConfig{},
			want:   errPost,
		},
		{
			desc:   "not in allowlist",
			client: &fakeHTTPDoer{body: []byte("not in allowlist")},
			hash:   "123",
			config: &fakeConfig{},
			want:   errResponse,
		},
		{
			desc:   "unmarshal error",
			client: &fakeHTTPDoer{body: []byte(`{"field":what?}`)},
			hash:   "123",
			config: &fakeConfig{},
			want:   errFormat,
		},
		{
			desc:   "status not successful",
			client: &fakeHTTPDoer{body: bad},
			hash:   "123",
			config: &fakeConfig{},
			want:   errSeed,
		},
		{
			desc:   "success",
			client: &fakeHTTPDoer{body: good},
			hash:   "123",
			config: &fakeConfig{},
			out:    &models.SeedResponse{ErrorCode: models.StatusSuccess},
			want:   nil,
		},
	}
	for _, tt := range tests {
		out, got := seedRequest(tt.client, tt.hash, tt.config)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: Finalize() got: %v, want: %v", tt.desc, got, tt.want)
		}
		if diff := cmp.Diff(tt.out, out); diff != "" {
			t.Errorf("%s: seedRequest output mismatch (-want +got):\n%s", tt.desc, diff)
		}
	}
}

func TestFinalize(t *testing.T) {
	tests := []struct {
		desc      string
		installer *Installer
		device    *fakeDevice
		dismount  bool
		want      error
	}{
		{
			desc:      "detection error",
			dismount:  true,
			installer: &Installer{config: &fakeConfig{}},
			device:    &fakeDevice{detectErr: errors.New("error")},
			want:      errFinalize,
		},
		{
			desc:      "dismount error",
			dismount:  true,
			installer: &Installer{config: &fakeConfig{}},
			device:    &fakeDevice{dmErr: errors.New("error")},
			want:      errDevice,
		},
		{
			desc:      "cache removal error",
			installer: &Installer{cache: `.`, config: &fakeConfig{}},
			device:    &fakeDevice{},
			want:      errPath,
		},
		{
			desc:      "eject error",
			installer: &Installer{config: &fakeConfig{eject: true}},
			dismount:  true,
			device:    &fakeDevice{ejectErr: errors.New("error")},
			want:      errIO,
		},
		{
			desc:      "success",
			installer: &Installer{config: &fakeConfig{}},
			device:    &fakeDevice{},
			want:      nil,
		},
	}
	for _, tt := range tests {
		got := tt.installer.Finalize([]Device{tt.device}, tt.dismount)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: Finalize() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

// TestUnconfiguredDistroSkipsConfig verifies that Retrieve and provisionISO do
// not fetch or write a configuration file when NeedsConfig is false.
func TestUnconfiguredDistroSkipsConfig(t *testing.T) {
	oldDownloadFile, oldMount, oldWriteISO, oldSelectPart := downloadFile, mount, writeISOFunc, selectPart
	defer func() {
		downloadFile, mount, writeISOFunc, selectPart = oldDownloadFile, oldMount, oldWriteISO, oldSelectPart
	}()

	fakeCache := t.TempDir()
	fakeMount := t.TempDir()

	// Unconfigured distro: hasConfig=false, ffu=false.
	downloadedPaths := []string{}
	downloadFile = func(client httpDoer, path string, w io.Writer) error {
		downloadedPaths = append(downloadedPaths, path)
		return nil
	}

	inst := &Installer{
		cache: fakeCache,
		config: &fakeConfig{
			imagePath: "https://image.host.com/media/stable/test.iso",
			imageFile: "test.iso",
			hasConfig: false,
			ffu:       false,
		},
	}

	if err := inst.Retrieve(); err != nil {
		t.Fatalf("Retrieve() returned %v", err)
	}
	if len(downloadedPaths) != 1 {
		t.Errorf("Retrieve() called download %d times, want exactly 1", len(downloadedPaths))
	}
	if len(downloadedPaths) > 0 && downloadedPaths[0] != "https://image.host.com/media/stable/test.iso" {
		t.Errorf("downloaded path got %q, want test.iso URL", downloadedPaths[0])
	}

	// Test provisionISO with an unconfigured distro.
	mount = func(string) (isoHandler, error) { return &fakeHandler{}, nil }
	writeISOFunc = func(isoHandler, partition) error { return nil }
	selectPart = func(Device, uint64, storage.FileSystem) (partition, error) {
		return &fakePartition{label: "test", mount: fakeMount}, nil
	}

	if err := inst.provisionISO(&fakeDevice{}); err != nil {
		t.Fatalf("provisionISO() returned %v", err)
	}

	// Verify that no config file was created.
	for _, name := range []string{confDestFile, bootConfDestFile} {
		ociFile := filepath.Join(fakeMount, "oci", name)
		if _, err := os.Stat(ociFile); !os.IsNotExist(err) {
			t.Errorf("provisionISO() created %q for unconfigured distro, want no file", ociFile)
		}
	}
}
