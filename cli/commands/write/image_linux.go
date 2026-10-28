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
	"fmt"
	"os"
	"syscall"
)

// imageModeSupported reports whether --image_file is available on this
// platform.
const imageModeSupported = true

// freeBytes returns the space available to unprivileged users in the
// filesystem that contains dir.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs(%q) returned %v", dir, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// fileInode returns the inode number of an open file.
func fileInode(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type %T for %q", fi.Sys(), f.Name())
	}
	return st.Ino, nil
}
