//go:build !windows
// +build !windows

package server

import (
	"context"
	"fmt"

	"flag"
	"github.com/google/subcommands"
)

// WriteRequest represents a request to write an image to disk.
type WriteRequest struct {
	Cleanup     bool
	Warning     bool
	Eject       bool
	FFU         bool
	Update      bool
	Devices     []string
	Distro      string
	Track       string
	ConfTrack   string
	SeedServer  string
	CacheDir    string
	SSOCookie   string
	ClientToken any
}

// Execute returns an error on non-Windows platforms.
func (c *serverCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	fmt.Println("The background service is only supported on Windows.")
	return subcommands.ExitFailure
}

// ClientWrite returns an error on non-Windows platforms.
func ClientWrite(req *WriteRequest) error {
	return fmt.Errorf("background service delegation is only supported on Windows")
}
