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

//go:build unix

package console

import (
	"os"

	"golang.org/x/sys/unix"
)

// stderrIsTerminal reports whether stderr is a terminal.
func stderrIsTerminal() bool {
	_, err := unix.IoctlGetWinsize(int(os.Stderr.Fd()), unix.TIOCGWINSZ)
	return err == nil
}

// stderrWidth returns the width of the stderr terminal in columns, or 0 if it
// is unknown.
func stderrWidth() int {
	ws, err := unix.IoctlGetWinsize(int(os.Stderr.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}

// consoleSupportsUTF8 reports whether the terminal is expected to render
// UTF-8, based on the locale environment.
func consoleSupportsUTF8() bool {
	return localeIsUTF8()
}
