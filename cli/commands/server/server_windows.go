//go:build windows
// +build windows

package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"net/http"
	"net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"runtime"
	"unsafe"

	"flag"
	sso "github.com/google/splice/cli/appclient"
	"github.com/google/fresnel/cli/config"
	"github.com/google/fresnel/cli/installer"
	"github.com/google/deck"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows"
	"github.com/google/subcommands"
	"github.com/Microsoft/go-winio"
)

// PipeName is the name of the named pipe used for RPC communication.
const PipeName = `\\.\pipe\fresnel_service`

var (
	// PreWriteDiskHook allows executing custom setup logic prior to writing a disk.
	PreWriteDiskHook = func() error { return nil }
	// PostWriteDiskHook allows executing custom teardown logic after writing a disk.
	PostWriteDiskHook = func() {}
	// StartupHook allows executing custom logic when the server service starts.
	StartupHook = func() {}
)

// FresnelService is the RPC service exposed over the named pipe.
type FresnelService struct {
	writeMu sync.Mutex
}

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
	ClientToken windows.Token `json:"-"`
}

// WriteDisk handles the RPC request to write the disk.
func (s *FresnelService) WriteDisk(req *WriteRequest, resp *WriteResponse) error {
	deck.InfofA("Received RPC request to write disk").With(deck.V(1)).Go()
	if req.ClientToken == 0 {
		resp.Error = "Access denied: Could not securely identify the calling user."
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := PreWriteDiskHook(); err != nil {
		resp.Error = fmt.Sprintf("pre-write hook error: %v", err)
		return nil
	}
	defer PostWriteDiskHook()

	conf, err := config.New(req.Cleanup, req.Warning, req.Eject, req.FFU, req.Update, req.Devices, req.Distro, req.Track, req.ConfTrack, req.SeedServer)
	if err != nil {
		resp.Error = fmt.Sprintf("config error: %v", err)
		return nil
	}

	i, err := installer.New(conf, true)
	if err != nil {
		resp.Error = fmt.Sprintf("installer error: %v", err)
		return nil
	}
	i.SetCache(req.CacheDir)
	i.SetClientToken(uintptr(req.ClientToken))

	if req.SSOCookie != "" {
		tlsClient, err := sso.TLSClient(nil, nil)
		if err != nil {
			deck.Errorf("TLSClient error: %v", err)
		}
		if err == nil {
			client := &ssoHTTPClient{
				cookie: req.SSOCookie,
				client: tlsClient,
			}
			i.SetHTTPClient(client)
		}
	}

	for _, deviceID := range req.Devices {
		// StorageSearch requires SYSTEM privileges to properly query physical drives.
		devices, err := installer.StorageSearch(deviceID, 0, 0, true)
		if err != nil || len(devices) == 0 {
			resp.Error = fmt.Sprintf("could not find device %s: %v", deviceID, err)
			return nil
		}
		device := devices[0]
		// Execute Prepare() as the Impersonated User.
		// This ensures that the user-provided CacheDir and SeedServer are accessed
		// securely under their own permissions, preventing Arbitrary File Writes as SYSTEM.
		err = func() error {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if err := installer.User(req.ClientToken); err != nil {
				return fmt.Errorf("failed to impersonate client: %v", err)
			}
			defer windows.RevertToSelf()
			if err := i.Prepare(device); err != nil {
				return fmt.Errorf("prepare error for %s: %v", device.FriendlyName(), err)
			}
			return nil
		}()

		if err != nil {
			resp.Error = err.Error()
			return nil
		}
		// The thread is now SYSTEM again.
		// Execute Provision() natively as SYSTEM to allow raw physical disk writes.
		if err := i.Provision(device); err != nil {
			resp.Error = fmt.Sprintf("provision error for %s: %v", device.FriendlyName(), err)
			return nil
		}
	}

	return nil
}

type fresnelSvc struct {
	l    net.Listener
	sddl string
}

// tokenCodec wraps jsonrpc to inject the client's token into the request.
type tokenCodec struct {
	rpc.ServerCodec
	clientToken windows.Token
}

// Close closes the codec and the client token handle if it's open.
func (m *tokenCodec) Close() error {
	if m.clientToken != 0 {
		windows.CloseHandle(windows.Handle(m.clientToken))
		m.clientToken = 0
	}
	return m.ServerCodec.Close()
}

func (m *fresnelSvc) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	changes <- svc.Status{State: svc.StartPending}

	StartupHook()

	service := new(FresnelService)
	rpc.Register(service)

	pc := &winio.PipeConfig{
		SecurityDescriptor: m.sddl,
		MessageMode:        true,
	}

	l, err := winio.ListenPipe(PipeName, pc)
	if err != nil {
		deck.Errorf("ListenPipe error: %v", err)
		return false, 1
	}
	m.l = l
	defer m.l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			clientToken := getClientToken(conn)
			// Serve the RPC connection using our injecting codec.
			codec := &tokenCodec{
				ServerCodec: jsonrpc.NewServerCodec(conn),
				clientToken: clientToken,
			}
			go func(c *tokenCodec) {
				defer c.Close()
				rpc.ServeCodec(c)
			}(codec)
		}
	}()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

// getPipeHandle uses reflection to extract the raw windows.Handle from a winio.PipeConn.
func getPipeHandle(conn net.Conn) windows.Handle {
	v := reflect.ValueOf(conn)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		v = v.Elem()
	}

	if v.Kind() == reflect.Struct {
		win32PipeField := v.FieldByName("win32Pipe")
		if win32PipeField.IsValid() {
			win32FileField := win32PipeField.FieldByName("win32File")
			if win32FileField.IsValid() && !win32FileField.IsNil() {
				handleField := win32FileField.Elem().FieldByName("handle")
				if handleField.IsValid() && handleField.CanAddr() {
					ptr := unsafe.Pointer(handleField.UnsafeAddr())
					return *(*windows.Handle)(ptr)
				}
			}
		}
	}
	return 0
}

// getClientToken impersonates the connected pipe client to retrieve their Windows token.
func getClientToken(conn net.Conn) windows.Token {
	fd := getPipeHandle(conn)
	if fd == 0 {
		deck.Errorf("Could not extract raw pipe handle from connection")
		return 0
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := installer.ImpersonateNamedPipeClient(fd); err != nil {
		deck.Errorf("Failed to impersonate pipe client: %v", err)
		return 0
	}
	defer windows.RevertToSelf()

	var clientToken windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_ALL_ACCESS, true, &clientToken); err != nil {
		deck.Errorf("Failed to open thread token: %v", err)
		return 0
	}

	return clientToken
}

type ssoHTTPClient struct {
	cookie string
	client *http.Client
}

func (c *ssoHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Add("Cookie", c.cookie)
	return c.client.Do(req)
}

func (c *ssoHTTPClient) Get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Execute runs the named pipe server.
func (c *serverCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	deck.InfofA("Starting Fresnel background service on %s", PipeName).With(deck.V(1)).Go()
	// Safe default: SYSTEM (SY) and Built-in Administrators (BA).
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
	if c.allowedGroup != "" {
		// Look up the SID for the provided local group name.
		sid, _, _, err := windows.LookupSID("", c.allowedGroup)
		if err != nil {
			deck.Errorf("Failed to look up SID for allowed_group %q: %v", c.allowedGroup, err)
			return subcommands.ExitFailure
		}
		// Append Generic Read/Write (GRGW) for the group's SID.
		sddl += fmt.Sprintf("(A;;GRGW;;;%s)", sid.String())
	}
	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		deck.Errorf("IsAnInteractiveSession error: %v", err)
		return subcommands.ExitFailure
	}

	if !isInteractive {
		if err := svc.Run("fresnel", &fresnelSvc{sddl: sddl}); err != nil {
			deck.Errorf("svc.Run error: %v", err)
			return subcommands.ExitFailure
		}
		return subcommands.ExitSuccess
	}

	// Interactive mode.
	service := new(FresnelService)
	rpc.Register(service)

	pc := &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        true,
	}

	l, err := winio.ListenPipe(PipeName, pc)
	if err != nil {
		deck.Errorf("ListenPipe error: %v", err)
		return subcommands.ExitFailure
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			deck.Errorf("Accept error: %v", err)
			continue
		}
		go rpc.ServeCodec(jsonrpc.NewServerCodec(conn))
	}
}

// ClientWrite connects to the named pipe service and sends a WriteRequest.
func ClientWrite(req *WriteRequest) error {
	access := windows.GENERIC_READ | windows.GENERIC_WRITE
	conn, err := winio.DialPipeAccessImpLevel(
		context.Background(),
		PipeName,
		uint32(access),
		winio.PipeImpLevelImpersonation,
	)
	if err != nil {
		return fmt.Errorf("could not connect to background service (is it running?): %v", err)
	}
	defer conn.Close()

	client := jsonrpc.NewClient(conn)
	var resp WriteResponse
	if err := client.Call("FresnelService.WriteDisk", req, &resp); err != nil {
		return fmt.Errorf("rpc call failed: %v", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("service error: %s", resp.Error)
	}
	return nil
}

// ReadRequestBody reads the request body from the server codec and injects the client token if
// the request is a WriteRequest.
func (m *tokenCodec) ReadRequestBody(body any) error {
	if err := m.ServerCodec.ReadRequestBody(body); err != nil {
		return err
	}
	if req, ok := body.(*WriteRequest); ok {
		req.ClientToken = m.clientToken
	}
	return nil
}
