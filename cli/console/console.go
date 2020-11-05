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

// Package console provides simple utilities to print human-readable messages
// to the console. For specific message types, additional verbosity is
// available through Verbose.
package console

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"
)

var (
	// Verbose is used to control whether or not print messages are printed.
	// It is exposed as package state to allow the verbosity to be uniformly
	// controlled across packages that use it.
	Verbose = false
)

// Print displays a console message when Verbose is false. Arguments
// are handled in the same manner as fmt.Print.
func Print(v ...interface{}) {
	if !Verbose {
		fmt.Print(v...)
	}
}

// Printf displays a console message when Verbose is false. Arguments
// are handled in the same manner as fmt.Printf.
func Printf(format string, v ...interface{}) {
	if !Verbose {
		fmt.Printf(format+"\n", v...)
	}
}

// PromptUser displays a warning that the actions to be performed are
// destructive. It returns an error if the user does not respond with a 'y'.
// It is always printed, regardless of the value of Verbose.
func PromptUser() error {
	msg := "\nIMPORTANT: Proceeding will DESTROY the contents of a device!\n\n" +
		"Do you want to erase and re-initialize the devices listed? (y/N)? "
	fmt.Print(msg)

	reader := bufio.NewReader(os.Stdin)
	r, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reader.ReadString('\n') returned: %v", err)
	}
	r = strings.Trim(r, "\r\n")
	if !strings.EqualFold(r, "y") {
		return errors.New("canceled media initialization")
	}
	return nil
}

// TargetDevice represents target.Device.
type TargetDevice interface {
	Identifier() string
	FriendlyName() string
	Size() uint64
}

type rawDevice struct {
	ID   string
	Name string
	Size string
}

// PrintDevices takes a slice of target devices and prints relevant information
// as a human-readable table to the console. If the json flag
// is present the target devices will be printed as JSON rather than a table.
func PrintDevices(targets []TargetDevice, w io.Writer, json bool) {

	if json {
		Printjson(targets, w)
		// Return immediately after raw output to ensure the output is proper JSON only.
		return
	}

	//Check if any devices exist.
	if len(targets) == 0 {
		fmt.Fprintf(w, "No matching devices were found.")
		return
	}

	// Display the table to the user otherwise, output devices with table
	table := tablewriter.NewWriter(w)
	table.SetBorder(false)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{"Device", "Model", "Size"})
	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.FgGreenColor}, // Green text for device column.
		tablewriter.Colors{},                         // No color change for model column.
		tablewriter.Colors{},                         // No color change for size column.
	)
	for _, device := range targets {
		table.Append([]string{
			device.Identifier(),
			device.FriendlyName(),
			humanize.Bytes(device.Size()),
		},
		)
	}
	table.Render()
}

// Printjson takes a slice of target devices and prints relevant information
// as JSON to the console when the json flag is present on the PrintDevices
// function.
func Printjson(targets []TargetDevice, w io.Writer) error {

	result := []rawDevice{}
	for _, device := range targets {
		result = append(result, rawDevice{
			ID:   device.Identifier(),
			Name: device.FriendlyName(),
			Size: humanize.Bytes(device.Size()),
		})
	}

	output, err := json.Marshal(result)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s", output)
	return nil
}

type progressReader struct {
	reader    io.Reader
	operation string

	// Total length of data and counter for what has been read.
	length int64
	read   int64

	// Counter for progress bar and how frequently to update the bar in msec.
	bars int64
	freq int64

	start   time.Time
	lastLog time.Time
}

// ProgressReader wraps an io.Reader and writes the read progress to the
// console. The writes are displayed on call of the Read method and at most
// every 5 seconds. The messages include the supplied human readable operation.
// The provided length can also be zero if it is unknown ahead of time. A
// ProgressReader always outputs to the console, regardless of the value of
// verbose.
func ProgressReader(reader io.Reader, operation string, length int64) io.Reader {
	now := time.Now()
	if length < 0 {
		length = 0
	}
	pr := progressReader{
		reader:    reader,
		operation: operation,
		length:    length,
		read:      0,
		bars:      0,
		freq:      300, // The bar is updated every 300 msec.
		start:     now,
		lastLog:   now,
	}
	return &pr
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if err != nil {
		return n, err
	}

	pr.read += int64(n)
	now := time.Now()
	diff := now.Sub(pr.lastLog)
	if diff.Milliseconds() < pr.freq {
		return n, nil
	}

	// Prepare to log progress.
	pr.lastLog = now
	length := float64(pr.length) // in bytes.
	read := float64(pr.read)     // in bytes.

	// Determine read speed.
	diff = now.Sub(pr.start)
	since := diff.Seconds()
	var speed float64 // in bytes/s.
	if since != 0 {
		speed = read / since
	}

	// Log progress.
	speeds := units.BytesSize(speed) + "/s"
	if pr.length >= 0 {
		// Determine remaining bytes and time until finished.
		remain := length - read // Remaining bytes to read.
		if remain < 0 {
			remain = 0 // This shouldn't ever happen.
		}
		var until float64 // Seconds until finished.
		if speed != 0 {
			until = remain / speed
		}
		lengths := units.BytesSize(length)
		// Print the speed and estimated time remaining just once, above
		// the progress bar.
		if diff.Milliseconds() <= pr.freq+(pr.freq/3) {
			fmt.Printf("%s started: %s, %0.2f seconds remaining\n", pr.operation, speeds, until)
			fmt.Printf("Size:     [--------------------------------------------------] %s\n", lengths)
			fmt.Print("Progress:  ")
		}
		// Calculate the progress and update the progress bar.
		progress := int64(read / length * 100 / 2)
		for pr.bars <= progress {
			fmt.Print("=")
			pr.bars++
		}
	}

	return n, nil
}
