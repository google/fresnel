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

//go:build windows

package console

import (
	"os"

	"golang.org/x/sys/windows"
)

// utf8CodePage is the Windows code page identifier for UTF-8.
const utf8CodePage = 65001

// stderrIsTerminal reports whether stderr is a Windows console.
func stderrIsTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stderr.Fd()), &mode) == nil
}

// stderrWidth returns the width of the visible stderr console window in
// columns, or 0 if it is unknown.
func stderrWidth() int {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stderr.Fd()), &info); err != nil {
		return 0
	}
	return int(info.Window.Right-info.Window.Left) + 1
}

// consoleSupportsUTF8 reports whether the console is expected to render
// UTF-8: Windows Terminal always does, while conhost does only when the
// console output code page is UTF-8.
func consoleSupportsUTF8() bool {
	if getenv("WT_SESSION") != "" {
		return true
	}
	cp, err := windows.GetConsoleOutputCP()
	return err == nil && cp == utf8CodePage
}
