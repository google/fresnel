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

package console

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeTerm describes the stubbed console environment for a test.
type fakeTerm struct {
	tty   bool
	width int
	utf8  bool
	env   map[string]string
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }

// stubConsole replaces the package seams with fakes and restores them when
// the test ends. It returns the buffer that receives progress output and the
// fake clock.
func stubConsole(t *testing.T, ft fakeTerm) (*bytes.Buffer, *fakeClock) {
	t.Helper()
	oldOut, oldNow, oldGetenv := out, now, getenv
	oldIsTerminal, oldTermWidth, oldSupportsUTF8 := isTerminal, termWidth, supportsUTF8
	oldVerbose := Verbose
	t.Cleanup(func() {
		out, now, getenv = oldOut, oldNow, oldGetenv
		isTerminal, termWidth, supportsUTF8 = oldIsTerminal, oldTermWidth, oldSupportsUTF8
		Verbose = oldVerbose
	})
	buf := &bytes.Buffer{}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	out = buf
	now = clk.now
	getenv = func(k string) string { return ft.env[k] }
	isTerminal = func() bool { return ft.tty }
	termWidth = func() int { return ft.width }
	supportsUTF8 = func() bool { return ft.utf8 }
	Verbose = false
	return buf, clk
}

// step is a single scripted Read result. The clock is advanced by adv before
// the Read returns n bytes and err.
type step struct {
	adv time.Duration
	n   int
	err error
}

// scriptReader replays a list of steps against a fake clock.
type scriptReader struct {
	clk   *fakeClock
	steps []step
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	s := r.steps[0]
	r.steps = r.steps[1:]
	r.clk.t = r.clk.t.Add(s.adv)
	n := min(s.n, len(p))
	return n, s.err
}

// drain reads from r until an error is returned, using a buffer large enough
// for any scripted step.
func drain(t *testing.T, r io.Reader) error {
	t.Helper()
	p := make([]byte, 1<<20)
	for i := 0; i < 1000; i++ {
		if _, err := r.Read(p); err != nil {
			return err
		}
	}
	t.Fatal("reader never returned an error")
	return nil
}

// frames splits interactive output after the title line into the frames that
// were drawn with a carriage return.
func frames(s string) []string {
	parts := strings.Split(s, "\r")
	return parts[1:]
}

func TestProgressInteractiveDownload(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	r := ProgressReader(&scriptReader{clk: clk, steps: []step{
		{adv: 100 * time.Millisecond, n: 100},       // Throttled.
		{adv: 150 * time.Millisecond, n: 100},       // Drawn at 250ms.
		{adv: 50 * time.Millisecond, n: 100},        // Throttled.
		{adv: 1 * time.Second, n: 700, err: io.EOF}, // Final.
	}}, "  Downloading foo.iso  ", 1000)
	if err := drain(t, r); !errors.Is(err, io.EOF) {
		t.Fatalf("drain() = %v, want io.EOF", err)
	}
	got := buf.String()
	if want := "Downloading foo.iso (1000 B)\n"; !strings.HasPrefix(got, want) {
		t.Errorf("output = %q, want prefix %q", got, want)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output = %q, want trailing newline", got)
	}
	f := frames(got)
	if len(f) != 3 {
		t.Fatalf("got %d frames %q, want 3 (initial, 250ms, final)", len(f), f)
	}
	if !strings.Contains(f[0], "  0%") || !strings.Contains(f[0], "ETA --:--") {
		t.Errorf("initial frame = %q, want 0%% and unknown ETA", f[0])
	}
	if !strings.Contains(f[1], " 20%") || !strings.Contains(f[1], "200 B / 1000 B") || !strings.Contains(f[1], "ETA ") {
		t.Errorf("mid frame = %q, want 20%%, byte counts and ETA", f[1])
	}
	last := f[2]
	if !strings.Contains(last, "100%") {
		t.Errorf("final frame = %q, want 100%%", last)
	}
	if strings.Contains(last, "ETA") {
		t.Errorf("final frame = %q, want no ETA", last)
	}
	if !strings.Contains(last, strings.Repeat("█", barWidth)) {
		t.Errorf("final frame = %q, want a full UTF-8 bar", last)
	}
	if !strings.Contains(last, "[00:01]") {
		t.Errorf("final frame = %q, want elapsed [00:01]", last)
	}
}

func TestProgressThrottle(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	var steps []step
	// 50 reads 1ms apart all fall within a single redraw interval.
	for i := 0; i < 50; i++ {
		steps = append(steps, step{adv: time.Millisecond, n: 1})
	}
	r := ProgressReader(&scriptReader{clk: clk, steps: steps}, "op", 1000)
	p := make([]byte, 10)
	for range steps {
		if _, err := r.Read(p); err != nil {
			t.Fatalf("Read() = %v", err)
		}
	}
	if n := strings.Count(buf.String(), "\r"); n != 1 {
		t.Errorf("got %d frames, want only the initial frame: %q", n, buf.String())
	}
}

func TestProgressSkipsIdenticalFrames(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	// Zero-byte reads that keep elapsed time under half a second render the
	// same line as the initial frame.
	r := ProgressReader(&scriptReader{clk: clk, steps: []step{
		{adv: 210 * time.Millisecond},
		{adv: 210 * time.Millisecond},
	}}, "op", 1000)
	p := make([]byte, 10)
	r.Read(p)
	r.Read(p)
	if n := strings.Count(buf.String(), "\r"); n != 1 {
		t.Errorf("got %d frames, want identical frames skipped: %q", n, buf.String())
	}
}

func TestProgressPadsStaleCharactersByRuneWidth(t *testing.T) {
	buf, _ := stubConsole(t, fakeTerm{tty: true, utf8: true})
	pr := ProgressReader(strings.NewReader(""), "op", 1000).(*progressReader)
	// A longer previous line made of multi-byte runes must be blanked by its
	// rune count, not its byte length.
	pr.lastLine = strings.Repeat("█", 100)
	buf.Reset()
	pr.draw(pr.start, false)
	got := buf.String()
	s := pr.line(pr.start, false)
	want := "\r" + s + strings.Repeat(" ", 100-utf8.RuneCountInString(s))
	if got != want {
		t.Errorf("draw() wrote %q, want %q", got, want)
	}
	if pr.lastLine != s {
		t.Errorf("lastLine = %q, want %q", pr.lastLine, s)
	}
}

func TestProgressLineFitsTerminalWidth(t *testing.T) {
	for _, length := range []int64{0, 5 << 30} {
		_, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
		pr := ProgressReader(strings.NewReader(""), "op", length).(*progressReader)
		pr.read = 1536 << 20
		at := clk.t.Add(83 * time.Second)
		for w := 1; w <= 120; w++ {
			termWidth = func() int { return w }
			s := pr.line(at, false)
			// The percent (or byte count when the length is unknown) and the
			// elapsed time are never dropped, so only widths that can hold
			// them are checked.
			minimal := " 30%  [01:23]"
			if length <= 0 {
				minimal = "  1.5 GiB  [01:23]"
			}
			if got := utf8.RuneCountInString(s); got > w-1 && w-1 >= len(minimal) {
				t.Errorf("length=%d width=%d: line %q has %d columns, want <= %d", length, w, s, got, w-1)
			}
			hasETA := strings.Contains(s, "ETA")
			hasSpeed := strings.Contains(s, "/s")
			hasBytes := strings.Contains(s, "1.5 GiB")
			if hasETA && !hasSpeed {
				t.Errorf("length=%d width=%d: line %q dropped speed before ETA", length, w, s)
			}
			if hasSpeed && !hasBytes {
				t.Errorf("length=%d width=%d: line %q dropped byte counts before speed", length, w, s)
			}
			if !strings.Contains(s, "[01:23]") {
				t.Errorf("length=%d width=%d: line %q dropped the elapsed time", length, w, s)
			}
			if strings.HasPrefix(s, "|") {
				bar := s[1 : 1+strings.Index(s[1:], "|")]
				if cells := utf8.RuneCountInString(bar); cells < minBarWidth || cells > barWidth {
					t.Errorf("length=%d width=%d: line %q draws a bar of %d cells, want [%d, %d]", length, w, s, cells, minBarWidth, barWidth)
				}
			}
		}
	}
}

func TestProgressLineDropOrder(t *testing.T) {
	_, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	pr := ProgressReader(strings.NewReader(""), "op", 1000).(*progressReader)
	pr.read = 500
	at := clk.t.Add(5 * time.Second)
	full := pr.status(at, false)
	minimal := utf8.RuneCountInString(" 50%  [00:05]")
	// The narrowest bar takes minBarWidth cells plus "|| ".
	bar := minBarWidth + 3
	tests := []struct {
		name      string
		width     int
		wantETA   bool
		wantSpeed bool
		wantBytes bool
		wantBar   bool
	}{
		{"everything", 200, true, true, true, true},
		{"bar shrinks text kept", utf8.RuneCountInString(full) + 1 + bar, true, true, true, true},
		{"drop ETA keep bar", utf8.RuneCountInString(full) + bar, false, true, true, true},
		{"only percent and elapsed keep bar", minimal + 1 + bar, false, false, false, true},
		{"no bar", minimal + bar, false, false, false, false},
		{"tiny", 5, false, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			termWidth = func() int { return tc.width }
			s := pr.line(at, false)
			if got := strings.Contains(s, "ETA"); got != tc.wantETA {
				t.Errorf("line %q: ETA present = %v, want %v", s, got, tc.wantETA)
			}
			if got := strings.Contains(s, "/s"); got != tc.wantSpeed {
				t.Errorf("line %q: speed present = %v, want %v", s, got, tc.wantSpeed)
			}
			if got := strings.Contains(s, "500 B / 1000 B"); got != tc.wantBytes {
				t.Errorf("line %q: bytes present = %v, want %v", s, got, tc.wantBytes)
			}
			if got := strings.HasPrefix(s, "|"); got != tc.wantBar {
				t.Errorf("line %q: bar present = %v, want %v", s, got, tc.wantBar)
			}
			if !strings.Contains(s, " 50%") || !strings.Contains(s, "[00:05]") {
				t.Errorf("line %q: want percent and elapsed always present", s)
			}
		})
	}
}

func TestProgressETAOverflow(t *testing.T) {
	_, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	pr := ProgressReader(strings.NewReader(""), "op", 2<<30).(*progressReader)
	// One byte in ten seconds would take centuries, which overflows
	// time.Duration if it is not capped.
	pr.read = 1
	if s := pr.status(clk.t.Add(10*time.Second), false); !strings.Contains(s, "ETA --:--") {
		t.Errorf("status() = %q, want an unknown ETA for a stalled transfer", s)
	}
}

func TestProgressClose(t *testing.T) {
	for _, tty := range []bool{true, false} {
		buf, clk := stubConsole(t, fakeTerm{tty: tty, utf8: true})
		r := ProgressReader(&scriptReader{clk: clk, steps: []step{
			{adv: 300 * time.Millisecond, n: 400},
		}}, "op", 1000)
		if _, err := r.Read(make([]byte, 1024)); err != nil {
			t.Fatalf("tty=%v: Read() = %v", tty, err)
		}
		// The consumer stops early, such as when a write fails.
		if err := r.Close(); err != nil {
			t.Errorf("tty=%v: Close() = %v, want nil", tty, err)
		}
		got := buf.String()
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("tty=%v: output = %q, want the line terminated", tty, got)
		}
		lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		if last := lines[len(lines)-1]; !strings.Contains(last, " 40%") {
			t.Errorf("tty=%v: last line = %q, want partial 40%%", tty, last)
		}
		before := buf.Len()
		r.Close()
		if buf.Len() != before {
			t.Errorf("tty=%v: second Close() wrote %q, want nothing", tty, buf.String()[before:])
		}
	}
}

func TestProgressCloseAfterEOFWritesNothing(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	r := ProgressReader(&scriptReader{clk: clk, steps: []step{
		{adv: time.Second, n: 10, err: io.EOF},
	}}, "op", 10)
	drain(t, r)
	before := buf.String()
	r.Close()
	if buf.String() != before {
		t.Errorf("Close() after EOF wrote %q, want nothing", buf.String()[len(before):])
	}
}

func TestProgressUnknownLength(t *testing.T) {
	for _, length := range []int64{0, -1} {
		buf, clk := stubConsole(t, fakeTerm{tty: true, width: 80, utf8: true})
		r := ProgressReader(&scriptReader{clk: clk, steps: []step{
			{adv: 300 * time.Millisecond, n: 2048},
			{adv: 2 * time.Second, n: 2048, err: io.EOF},
		}}, "Downloading foo", length)
		if err := drain(t, r); !errors.Is(err, io.EOF) {
			t.Fatalf("drain() = %v, want io.EOF", err)
		}
		got := buf.String()
		if !strings.HasPrefix(got, "Downloading foo\n") {
			t.Errorf("length=%d: output = %q, want title without size", length, got)
		}
		if strings.Contains(got, "%") || strings.Contains(got, "ETA") || strings.Contains(got, "|") {
			t.Errorf("length=%d: output = %q, want no percent, ETA or bar", length, got)
		}
		f := frames(got)
		last := f[len(f)-1]
		for _, want := range []string{"4.0 KiB", "/s", "[00:02]"} {
			if !strings.Contains(last, want) {
				t.Errorf("length=%d: final frame = %q, want %q", length, last, want)
			}
		}
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("length=%d: output = %q, want trailing newline", length, got)
		}
	}
}

func TestProgressErrorLeavesPartialState(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	boom := errors.New("boom")
	src := &scriptReader{clk: clk, steps: []step{
		{adv: 300 * time.Millisecond, n: 250},
		{adv: 300 * time.Millisecond, n: 0, err: boom},
		{adv: 300 * time.Millisecond, n: 10},
	}}
	r := ProgressReader(src, "op", 1000)
	if err := drain(t, r); !errors.Is(err, boom) {
		t.Fatalf("drain() = %v, want %v", err, boom)
	}
	got := buf.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output = %q, want trailing newline", got)
	}
	f := frames(got)
	if last := f[len(f)-1]; !strings.Contains(last, " 25%") || strings.Contains(last, "100%") {
		t.Errorf("final frame = %q, want partial 25%%", last)
	}
	// Reads after the final render pass through silently.
	before := buf.Len()
	n, err := r.Read(make([]byte, 64))
	if n != 10 || err != nil {
		t.Errorf("Read() after error = (%d, %v), want (10, nil)", n, err)
	}
	if buf.Len() != before {
		t.Errorf("Read() after error wrote %q, want nothing", buf.String()[before:])
	}
}

func TestProgressPassThroughAfterEOF(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: true})
	r := ProgressReader(&scriptReader{clk: clk, steps: []step{
		{adv: time.Second, n: 10, err: io.EOF},
		{adv: time.Second, n: 0, err: io.EOF},
		{adv: time.Second, n: 0, err: io.EOF},
	}}, "op", 10)
	drain(t, r)
	before := buf.String()
	for i := 0; i < 2; i++ {
		if _, err := r.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
			t.Errorf("Read() = %v, want io.EOF", err)
		}
	}
	if buf.String() != before {
		t.Errorf("reads after EOF wrote %q, want nothing", buf.String()[len(before):])
	}
}

func TestProgressNonInteractive(t *testing.T) {
	tests := []struct {
		name    string
		ft      fakeTerm
		verbose bool
	}{
		{"not a terminal", fakeTerm{tty: false, utf8: true}, false},
		{"TERM=dumb", fakeTerm{tty: true, utf8: true, env: map[string]string{"TERM": "dumb"}}, false},
		{"verbose", fakeTerm{tty: true, utf8: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf, clk := stubConsole(t, tc.ft)
			Verbose = tc.verbose
			r := ProgressReader(&scriptReader{clk: clk, steps: []step{
				{adv: 10 * time.Second, n: 100}, // Too soon after the title.
				{adv: 10 * time.Second, n: 100}, // Still within 30s.
				{adv: 11 * time.Second, n: 100}, // 31s: status line.
				{adv: 10 * time.Second, n: 100}, // 41s: throttled.
				{adv: 25 * time.Second, n: 100}, // 66s: status line.
				{adv: 1 * time.Second, n: 500, err: io.EOF},
			}}, "Downloading foo", 1000)
			if err := drain(t, r); !errors.Is(err, io.EOF) {
				t.Fatalf("drain() = %v, want io.EOF", err)
			}
			got := buf.String()
			if strings.Contains(got, "\r") {
				t.Errorf("output = %q, want no carriage returns", got)
			}
			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			if len(lines) != 4 {
				t.Fatalf("got %d lines %q, want title, 2 status lines and a final line", len(lines), lines)
			}
			if lines[0] != "Downloading foo (1000 B)" {
				t.Errorf("title = %q, want %q", lines[0], "Downloading foo (1000 B)")
			}
			if !strings.Contains(lines[1], " 30%") || !strings.Contains(lines[1], "[00:31]") {
				t.Errorf("first status = %q, want 30%% at [00:31]", lines[1])
			}
			if !strings.Contains(lines[2], " 50%") || !strings.Contains(lines[2], "[01:06]") {
				t.Errorf("second status = %q, want 50%% at [01:06]", lines[2])
			}
			if !strings.Contains(lines[3], "100%") || strings.Contains(lines[3], "ETA") || !strings.Contains(lines[3], "[01:07]") {
				t.Errorf("final status = %q, want 100%% without ETA at [01:07]", lines[3])
			}
			for _, l := range lines[1:] {
				if strings.Contains(l, "|") || strings.Contains(l, "█") {
					t.Errorf("status line %q, want no bar", l)
				}
			}
		})
	}
}

func TestProgressNonInteractiveError(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: false})
	boom := errors.New("boom")
	r := ProgressReader(&scriptReader{clk: clk, steps: []step{
		{adv: time.Second, n: 400},
		{adv: time.Second, err: boom},
	}}, "op", 1000)
	if err := drain(t, r); !errors.Is(err, boom) {
		t.Fatalf("drain() = %v, want %v", err, boom)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], " 40%") {
		t.Errorf("output lines = %q, want title and a 40%% final status", lines)
	}
}

func TestProgressASCIIBar(t *testing.T) {
	buf, clk := stubConsole(t, fakeTerm{tty: true, utf8: false})
	r := ProgressReader(&scriptReader{clk: clk, steps: []step{
		{adv: 300 * time.Millisecond, n: 500},
		{adv: 300 * time.Millisecond, n: 500, err: io.EOF},
	}}, "op", 1000)
	drain(t, r)
	got := buf.String()
	if strings.Contains(got, "█") {
		t.Errorf("output = %q, want no UTF-8 block characters", got)
	}
	f := frames(got)
	if !strings.Contains(f[0], "|"+strings.Repeat("-", barWidth)+"|") {
		t.Errorf("initial frame = %q, want an empty ASCII bar", f[0])
	}
	if !strings.Contains(f[len(f)-1], "|"+strings.Repeat("=", barWidth)+"|") {
		t.Errorf("final frame = %q, want a full ASCII bar", f[len(f)-1])
	}
}

func TestBarChars(t *testing.T) {
	stubConsole(t, fakeTerm{utf8: true})
	if f, e := barChars(); f != "█" || e != " " {
		t.Errorf("barChars() with UTF-8 = (%q, %q), want (\"█\", \" \")", f, e)
	}
	supportsUTF8 = func() bool { return false }
	if f, e := barChars(); f != "=" || e != "-" {
		t.Errorf("barChars() without UTF-8 = (%q, %q), want (\"=\", \"-\")", f, e)
	}
}

func TestLocaleIsUTF8(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"unset", nil, false},
		{"LANG utf-8", map[string]string{"LANG": "en_US.UTF-8"}, true},
		{"LANG utf8", map[string]string{"LANG": "en_US.utf8"}, true},
		{"LANG C", map[string]string{"LANG": "C"}, false},
		{"LC_CTYPE over LANG", map[string]string{"LC_CTYPE": "C", "LANG": "en_US.UTF-8"}, false},
		{"LC_CTYPE utf8", map[string]string{"LC_CTYPE": "C.UTF-8", "LANG": "C"}, true},
		{"LC_ALL over all", map[string]string{"LC_ALL": "POSIX", "LC_CTYPE": "C.UTF-8", "LANG": "en_US.UTF-8"}, false},
		{"LC_ALL utf8", map[string]string{"LC_ALL": "de_DE.UTF-8", "LC_CTYPE": "C", "LANG": "C"}, true},
		{"empty LC_ALL ignored", map[string]string{"LC_ALL": "", "LANG": "en_US.UTF-8"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubConsole(t, fakeTerm{env: tc.env})
			if got := localeIsUTF8(); got != tc.want {
				t.Errorf("localeIsUTF8() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "00:00"},
		{-5 * time.Second, "00:00"},
		{400 * time.Millisecond, "00:00"},
		{59*time.Second + 600*time.Millisecond, "01:00"},
		{61 * time.Second, "01:01"},
		{59*time.Minute + 59*time.Second, "59:59"},
		{time.Hour, "1:00:00"},
		{time.Hour + 2*time.Minute + 5*time.Second, "1:02:05"},
		{25*time.Hour + 30*time.Second, "25:00:30"},
	}
	for _, tc := range tests {
		if got := elapsed(tc.d); got != tc.want {
			t.Errorf("elapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
