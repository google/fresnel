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

	dismount bool
	eject    bool
	elevated bool
	ffu      bool
	update   bool
	err      error // the error returned when isElevated is called.

	distroLabel string
	image       string
	imageFile   string
	seedDest    string
	seedFile    string
	seedServer  string
	track       string
	ffuDest     string
	ffuPath     string
	ffuManifest string
}

func (f *fakeConfig) Dismount() bool {
	return f.dismount
}

func (f *fakeConfig) DistroLabel() string {
	return f.distroLabel
}

func (f *fakeConfig) Image() string {
	return f.image
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

func (f *fakeConfig) FFUDest() string {
	return f.ffuDest
}

func (f *fakeConfig) FFUManifest() string {
	return f.ffuManifest
}

func (f *fakeConfig) FFUPath() string {
	return f.ffuPath
}

func TestNew(t *testing.T) {
	// Generate a fake config to use with New.
	c := &fakeConfig{
		image:      `https://foo.bar.com/test_installer.img`,
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
			desc: "missing ffu path",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				image:     `https://foo.bar.com/test_installer.img`,
				imageFile: `test_installer.img`,
				ffuPath:   ``,
				ffu:       true,
			}},
			want: errConfig,
		},
		{
			desc: "missing ffu manifest",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				image:       `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				ffuPath:     `https://foo.bar.com/once/OS/stable/`,
				ffu:         true,
				ffuManifest: "",
			}},
			want: errConfig,
		},
		{
			desc: "missing cache",
			installer: &Installer{config: &fakeConfig{
				image:       `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				ffuPath:     `https://foo.bar.com/once/OS/stable/`,
				ffu:         true,
				ffuManifest: "manifest.json",
			}},
			want: errCache,
		},
		{
			desc: "download success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{
				image:       `https://foo.bar.com/test_installer.img`,
				imageFile:   `test_installer.img`,
				ffuPath:     `https://foo.bar.com/once/OS/stable/`,
				ffu:         true,
				ffuManifest: "manifest.json",
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

	tests := []struct {
		desc      string
		installer *Installer
		mount     func(string) (isoHandler, error)
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
			desc:      "success",
			installer: &Installer{cache: fakeCache, config: &fakeConfig{imageFile: "fake.iso"}},
			mount:     func(string) (isoHandler, error) { return &fakeHandler{}, nil },
			writeISO:  func(isoHandler, partition) error { return nil },
			want:      nil,
		},
	}
	for _, tt := range tests {
		mount = tt.mount
		writeISOFunc = tt.writeISO
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

func fakeReadManifest() []SFUManifest {
	return []SFUManifest{
		SFUManifest{
			Filename: "testsfu.sfu",
		},
		SFUManifest{
			Filename: "testsfu2.sfu",
		},
		SFUManifest{
			Filename: "testsfu3.sfu",
		},
	}
}

func TestDownloadSFU(t *testing.T) {
	// Setup a temp folder.
	fakeCache, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', '') returned %v", err)
	}
	c := &fakeConfig{
		track:       "stable",
		ffuPath:     "https://www.somebody.com/once/windows/stable/",
		ffuManifest: "manifest.json",
	}
	tests := []struct {
		desc         string
		installer    *Installer
		download     func(client httpDoer, path string, w io.Writer) error
		fakeManifest func(string) ([]SFUManifest, error)
		want         error
	}{
		{
			desc:         "download success",
			installer:    &Installer{cache: fakeCache, config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return nil },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), nil },
			want:         nil,
		},
		{
			desc:         "missing cache",
			installer:    &Installer{cache: "", config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return nil },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), nil },
			want:         errCache,
		},
		{
			desc:         "manifest error",
			installer:    &Installer{cache: fakeCache, config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return nil },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), errManifest },
			want:         errManifest,
		},
		{
			desc:         "download error",
			installer:    &Installer{cache: fakeCache, config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return errDownload },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), nil },
			want:         errDownload,
		},
	}
	for _, tt := range tests {
		getManifest = tt.fakeManifest
		downloadFile = tt.download
		got := tt.installer.DownloadSFU()
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: DownloadSFU() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

// createFakeSFU is used to create a set of fake SFU files.
func createFakeSFU(fakeCache string) error {
	sfus := fakeReadManifest()
	for _, sfu := range sfus {
		path := filepath.Join(fakeCache, sfu.Filename)
		f, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("ioutil.TempFile(%q, %q) returned %w: %v", fakeCache, sfu.Filename, errFile, err)
		}
		defer f.Close()
	}
	return nil
}

func TestPlaceSFU(t *testing.T) {
	// Setup a temp folder.
	fakeCache, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', '') returned %v", err)
	}
	if err := createFakeSFU(fakeCache); err != nil {
		t.Fatalf("createFakeSFU(%s) returned: %v", fakeCache, err)
	}

	// Temp folders representing file system contents.
	mount, contents, err := fakeFileSystems()
	if err != nil {
		t.Fatalf("fakeFileSystems() returned %v", err)
	}
	defer os.RemoveAll(mount)

	c := &fakeConfig{
		track:       "stable",
		ffuManifest: "manifest.json",
	}
	tests := []struct {
		desc         string
		installer    *Installer
		download     func(client httpDoer, path string, w io.Writer) error
		fakeManifest func(string) ([]SFUManifest, error)
		device       *fakeDevice
		selPart      func(Device, uint64, storage.FileSystem) (partition, error)
		want         error
	}{
		{
			desc:         "successful place",
			installer:    &Installer{cache: fakeCache, config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return nil },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), nil },
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{mount: mount, contents: contents}, nil
			},
			device: &fakeDevice{},
			want:   nil,
		},
		{
			desc:         "manifest error",
			installer:    &Installer{cache: fakeCache, config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return nil },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), errManifest },
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{mount: mount, contents: contents}, nil
			},
			device: &fakeDevice{},
			want:   errManifest,
		},
		{
			desc:         "partition select failure",
			installer:    &Installer{cache: fakeCache, config: c},
			download:     func(client httpDoer, path string, w io.Writer) error { return nil },
			fakeManifest: func(string) ([]SFUManifest, error) { return fakeReadManifest(), nil },
			selPart: func(Device, uint64, storage.FileSystem) (partition, error) {
				return &fakePartition{mount: mount, contents: contents}, errPartition
			},
			device: &fakeDevice{},
			want:   errPartition,
		},
	}
	for _, tt := range tests {
		getManifest = tt.fakeManifest
		downloadFile = tt.download
		selectPart = tt.selPart
		got := tt.installer.PlaceSFU(tt.device)
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: PlaceSFU() got: %v, want: %v", tt.desc, got, tt.want)
		}
	}
}

func createFakeJSON(name, fakeJSON, cache string) error {

	// A fake manifest for testing.
	fakeJSONPath := filepath.Join(cache, name)
	if _, err := os.Create(fakeJSONPath); err != nil {
		return fmt.Errorf("os.Create(%q) returned %v", fakeJSONPath, err)
	}
	return ioutil.WriteFile(fakeJSONPath, []byte(fakeJSON), 0644)
}

func TestReadManifest(t *testing.T) {
	// A fake cache for testing.
	fakeCache, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("ioutil.TempDir('', '') returned %v", err)
	}
	// Valid JSON data for tests.
	testJSON := `[{"filename": "testsfu.sfu"}, {"filename": "testsfu2.sfu"}]`
	// Bad JSON data for tests.
	badJSON := `[dasd{"filename": "testsfu.sfu"}, {"filename": "testsfu2.sfu"}]`

	if err := createFakeJSON("good.json", testJSON, fakeCache); err != nil {
		t.Fatalf("createFakeJSON(%q) returned %v", testJSON, err)
	}
	if err := createFakeJSON("bad.json", badJSON, fakeCache); err != nil {
		t.Fatalf("createFakeJSON(%q) returned %v", badJSON, err)
	}

	tests := []struct {
		desc string
		path string
		want error
	}{
		{
			desc: "bad path",
			path: fmt.Sprintf("%s/%s", fakeCache, ""),
			want: errFile,
		},
		{
			desc: "malformed json",
			path: fmt.Sprintf("%s/%s", fakeCache, "bad.json"),
			want: errUnmarshal,
		},
		{
			desc: "success",
			path: fmt.Sprintf("%s/%s", fakeCache, "good.json"),
			want: nil,
		},
	}
	for _, tt := range tests {
		_, got := readManifest(tt.path)

		if !errors.Is(got, tt.want) {
			t.Errorf("%s: readManifest() got: %v, want: %v", tt.desc, got, tt.want)
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
