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
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dustin/go-humanize"
)

const (
	// barWidth is the maximum number of cells in the rendered progress bar.
	barWidth = 35
	// minBarWidth is the narrowest bar worth drawing; below it only the
	// text portion of the line is shown.
	minBarWidth = 10
	// redrawInterval throttles how often the progress line is redrawn.
	// Serial console loggers record every frame, so this is deliberately
	// coarser than what an interactive terminal alone would need.
	redrawInterval = 200 * time.Millisecond
	// plainInterval throttles how often a plain progress line is written when
	// the line cannot be redrawn in place, such as when stderr is redirected
	// to a log file.
	plainInterval = 30 * time.Second
	// maxETA is the longest estimated time remaining that is displayed.
	maxETA = 100 * time.Hour
)

var (
	// out is where progress is rendered. Progress goes to stderr, like curl,
	// Cabbie and GooGet, so that stdout remains clean for regular output.
	out io.Writer = os.Stderr
	// now is overridable by tests.
	now = time.Now
	// getenv is overridable by tests.
	getenv = os.Getenv
	// isTerminal reports whether stderr is an interactive terminal or
	// console. It is a variable so tests can stub it.
	isTerminal = stderrIsTerminal
	// termWidth returns the width of the stderr terminal in columns, or 0 if
	// it is unknown. It is a variable so tests can stub it.
	termWidth = stderrWidth
	// supportsUTF8 reports whether the console can render the Unicode block
	// characters used for the bar. It is a variable so tests can stub it.
	supportsUTF8 = consoleSupportsUTF8
)

// interactive reports whether progress can be redrawn in place on a single
// console line.
func interactive() bool {
	// TERM=dumb (Emacs shells, some CI and serial consoles) means carriage
	// returns are not interpreted, so a redrawn line would render as garbage.
	if getenv("TERM") == "dumb" {
		return false
	}
	// Verbose mode logs to stdout, which would land in the middle of a
	// redrawn line and corrupt it.
	if Verbose {
		return false
	}
	return isTerminal()
}

// barChars returns the characters used for the filled and empty bar cells.
// Unicode block characters are used only when the console is known to render
// them; legacy consoles fall back to ASCII to avoid mojibake.
func barChars() (fill, empty string) {
	if supportsUTF8() {
		return "█", " "
	}
	return "=", "-"
}

// localeIsUTF8 reports whether the POSIX locale environment selects a UTF-8
// character encoding. As with setlocale, LC_ALL takes precedence over
// LC_CTYPE, which takes precedence over LANG.
func localeIsUTF8() bool {
	for _, v := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if l := getenv(v); l != "" {
			l = strings.ToLower(l)
			return strings.Contains(l, "utf-8") || strings.Contains(l, "utf8")
		}
	}
	return false
}

// elapsed formats d as mm:ss, or hh:mm:ss when d is an hour or longer.
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// width approximates the number of console columns s occupies by counting
// runes. This is exact for the ASCII text and bar characters rendered here,
// except in East Asian locales where "█" may render two columns wide.
func width(s string) int {
	return utf8.RuneCountInString(s)
}

type progressReader struct {
	reader io.Reader
	title  string

	// Total length of data, where a length <= 0 means it is unknown, and a
	// counter for what has been read.
	length int64
	read   int64

	// interactive reports whether the progress line is redrawn in place.
	interactive bool
	// fill and empty are the characters used for the bar cells.
	fill, empty string

	start    time.Time
	lastDraw time.Time
	// lastLine is the line currently rendered with a carriage return, used to
	// blank stale characters when a shorter line replaces it and to skip
	// redraws that would not change anything.
	lastLine string
	// done is set once the final state has been rendered.
	done bool
}

// ProgressReader wraps an io.Reader and writes the read progress to stderr.
// The messages include the supplied human readable operation, such as
// "Downloading image.iso". The provided length can be zero or negative if it
// is unknown ahead of time.
//
// On an interactive terminal, the progress is rendered as a single line that
// is redrawn in place at most every 200 milliseconds, and that is fitted to
// the terminal width. Otherwise, such as when stderr is redirected, when
// TERM=dumb, or when Verbose is set, plain progress lines are written
// periodically instead so that logs remain readable.
//
// The final state is rendered once the wrapped reader returns io.EOF. If it
// returns any other error, the partial state is left visible above whatever
// error follows. Close must be called once the caller stops reading, so that
// a transfer abandoned for another reason, such as a failed write, also
// leaves its partial state on a line of its own; Close does not close the
// wrapped reader. A ProgressReader always outputs to the console, regardless
// of the value of Verbose.
func ProgressReader(reader io.Reader, operation string, length int64) io.ReadCloser {
	if length < 0 {
		length = 0
	}
	fill, empty := barChars()
	t := now()
	pr := &progressReader{
		reader:      reader,
		title:       strings.TrimSpace(operation),
		length:      length,
		interactive: interactive(),
		fill:        fill,
		empty:       empty,
		start:       t,
		// The title line counts as the first report, so the first plain
		// status line waits for a full interval.
		lastDraw: t,
	}
	if pr.length > 0 {
		fmt.Fprintf(out, "%s (%s)\n", pr.title, humanize.IBytes(uint64(pr.length)))
	} else {
		fmt.Fprintf(out, "%s\n", pr.title)
	}
	if pr.interactive {
		pr.draw(pr.start, false)
	}
	return pr
}

// Close renders the partial state one last time and terminates the line if
// the final state has not been rendered yet. It does not close the wrapped
// reader. Close is idempotent and always returns nil.
func (pr *progressReader) Close() error {
	if !pr.done {
		pr.end(now(), false)
	}
	return nil
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if pr.done {
		return n, err
	}
	pr.read += int64(n)
	t := now()
	switch {
	case errors.Is(err, io.EOF):
		pr.end(t, true)
	case err != nil:
		pr.end(t, false)
	case pr.interactive && t.Sub(pr.lastDraw) >= redrawInterval:
		pr.draw(t, false)
	case !pr.interactive && t.Sub(pr.lastDraw) >= plainInterval:
		pr.lastDraw = t
		fmt.Fprintf(out, "  %s\n", pr.status(t, false))
	}
	return n, err
}

// end renders the state at time t one last time and terminates the line. A
// final state omits the estimated time remaining; a partial state is left
// visible above whatever error follows.
func (pr *progressReader) end(t time.Time, final bool) {
	pr.done = true
	if !pr.interactive {
		fmt.Fprintf(out, "  %s\n", pr.status(t, final))
		return
	}
	pr.draw(t, final)
	fmt.Fprintln(out)
}

// speed returns the average transfer speed in bytes per second at time t.
func (pr *progressReader) speed(t time.Time) float64 {
	since := t.Sub(pr.start).Seconds()
	if since <= 0 {
		return 0
	}
	return float64(pr.read) / since
}

// percent returns the completed percentage, clamped to 100.
func (pr *progressReader) percent() int64 {
	if pr.length <= 0 {
		return 0
	}
	return min(pr.read, pr.length) * 100 / pr.length
}

// fields returns the text fields describing the progress at time t, in order
// of importance. The final field is always the elapsed time.
func (pr *progressReader) fields(t time.Time, final bool) []string {
	spd := pr.speed(t)
	speed := humanize.IBytes(uint64(spd)) + "/s"
	var f []string
	if pr.length > 0 {
		f = append(f, fmt.Sprintf("%3d%%", pr.percent()),
			fmt.Sprintf("%s / %s", humanize.IBytes(uint64(min(pr.read, pr.length))), humanize.IBytes(uint64(pr.length))))
	} else {
		f = append(f, humanize.IBytes(uint64(pr.read)))
	}
	f = append(f, speed)
	if pr.length > 0 && !final {
		eta := "--:--"
		// A stalled transfer would overflow time.Duration, so estimates
		// beyond maxETA are shown as unknown too.
		if remain := float64(max(pr.length-pr.read, 0)); spd > 0 && remain/spd < maxETA.Seconds() {
			eta = elapsed(time.Duration(remain / spd * float64(time.Second)))
		}
		f = append(f, "ETA "+eta)
	}
	return append(f, "["+elapsed(t.Sub(pr.start))+"]")
}

// status returns a plain, single-line description of the progress at time t.
func (pr *progressReader) status(t time.Time, final bool) string {
	return strings.Join(pr.fields(t, final), "  ")
}

// line returns the progress line for time t, fitted to the terminal width.
// Fields of lesser importance are dropped first, then the bar itself.
func (pr *progressReader) line(t time.Time, final bool) string {
	f := pr.fields(t, final)
	// The last column is kept free: a line that wraps cannot be redrawn with
	// a carriage return, so each frame would otherwise land on a new line.
	room := termWidth() - 1
	if room <= 0 {
		// An unknown width gets the full bar and all fields.
		room = math.MaxInt
	}
	// A line with a known length keeps room for the narrowest bar, drawn as
	// "|cells| ", until only the percentage and elapsed time are left. A line
	// without a bar is indented instead, and the indent must fit too.
	const indent = "  "
	reserve := minBarWidth + 3
	if pr.length <= 0 {
		reserve = width(indent)
	}
	// Drop the least important field preceding the elapsed time (the ETA,
	// then the speed, then the byte counts) until the text fits.
	for len(f) > 2 && width(strings.Join(f, "  "))+reserve > room {
		f = append(f[:len(f)-2], f[len(f)-1])
	}
	text := strings.Join(f, "  ")
	if pr.length <= 0 {
		return indent + text
	}
	cells := min(barWidth, room-width(text)-3)
	if cells < minBarWidth {
		return text
	}
	n := int(pr.percent()) * cells / 100
	return "|" + strings.Repeat(pr.fill, n) + strings.Repeat(pr.empty, cells-n) + "| " + text
}

// draw overwrites the current console line with the progress line for time
// t, blanking any trailing characters left over from a longer previous line.
// It writes nothing if the line is unchanged, since serial console loggers
// record every frame and identical frames are pure noise.
func (pr *progressReader) draw(t time.Time, final bool) {
	pr.lastDraw = t
	s := pr.line(t, final)
	if s == pr.lastLine {
		return
	}
	pad := ""
	if n := width(pr.lastLine) - width(s); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(out, "\r%s%s", s, pad)
	pr.lastLine = s
}
