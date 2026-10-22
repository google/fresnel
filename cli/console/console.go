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

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/dustin/go-humanize"
)

var (
	// Verbose is used to control whether or not print messages are printed.
	// It is exposed as package state to allow the verbosity to be uniformly
	// controlled across packages that use it.
	Verbose = false
)

// Print displays a console message when Verbose is false. Arguments
// are handled in the same manner as fmt.Print.
func Print(v ...any) {
	if !Verbose {
		fmt.Print(v...)
	}
}

// Printf displays a console message when Verbose is false. Arguments
// are handled in the same manner as fmt.Printf.
func Printf(format string, v ...any) {
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

	// Check if any devices exist.
	if len(targets) == 0 {
		fmt.Fprintf(w, "No matching devices were found.")
		return
	}

	// Display the table to the user otherwise, output devices with table
	table := tablewriter.NewTable(w,
		tablewriter.WithRendition(tw.Rendition{Borders: tw.BorderNone}),
		tablewriter.WithRowAutoWrap(tw.WrapNone))
	table.Header("\033[32mDevice\033[0m", "Model", "Size")
	for _, device := range targets {
		table.Append(
			device.Identifier(),
			device.FriendlyName(),
			humanize.Bytes(device.Size()),
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
