//go:build windows
// +build windows

package installer

import (
	"fmt"
	"os/user"
	"runtime"
	"strings"
	"unsafe"

	"github.com/google/deck"
	"golang.org/x/sys/windows"
)

// wtsConnectStateClass represents the WTS_CONNECTSTATE_CLASS enumeration type.
type wtsConnectStateClass int

const (
	wtsCurrentServerHandle   uintptr              = 0
	wtsActive                wtsConnectStateClass = 0
	securityImpersonation                         = 2
	tokenPrimary             int                  = 1
	swShow                   uint16               = 5
	createUnicodeEnvironment uint16               = 0x00000400
	createNewConsole                              = 0x00000010
	// WTSUserName sets it the name of the user associated with the session.
	// https://learn.microsoft.com/en-us/windows/win32/api/wtsapi32/ne-wtsapi32-wts_info_class
	WTSUserName = 5
)

var (
	modwtsapi32 *windows.LazyDLL = windows.NewLazySystemDLL("wtsapi32.dll")
	modkernel32 *windows.LazyDLL = windows.NewLazySystemDLL("kernel32.dll")
	modadvapi32 *windows.LazyDLL = windows.NewLazySystemDLL("advapi32.dll")
	moduserenv  *windows.LazyDLL = windows.NewLazySystemDLL("userenv.dll")

	procWTSEnumerateSessionsW        *windows.LazyProc = modwtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSGetActiveConsoleSessionID *windows.LazyProc = modkernel32.NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken            *windows.LazyProc = modwtsapi32.NewProc("WTSQueryUserToken")
	procDuplicateTokenEx             *windows.LazyProc = modadvapi32.NewProc("DuplicateTokenEx")
	procCreateEnvironmentBlock       *windows.LazyProc = moduserenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      *windows.LazyProc = moduserenv.NewProc("DestroyEnvironmentBlock")
	procCreateProcessAsUser          *windows.LazyProc = modadvapi32.NewProc("CreateProcessAsUserW")
	procWTSQuerySessionInformationW                    = modwtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory                                  = modwtsapi32.NewProc("WTSFreeMemory")
	procImpersonateLoggedOnUser                        = modadvapi32.NewProc("ImpersonateLoggedOnUser")
	procRevertToSelf                                   = modadvapi32.NewProc("RevertToSelf")
	procImpersonateNamedPipeClient                     = modadvapi32.NewProc("ImpersonateNamedPipeClient")

	interactiveDesktop = windows.StringToUTF16Ptr("winsta0\\default")
)

type wtsSessionInfo struct {
	SessionID      windows.Handle
	WinStationName *uint16
	State          wtsConnectStateClass
}

// GetCurrentUserSessionID will attempt to resolve the session ID of the user currently active on
// the system.
func GetCurrentUserSessionID() (windows.Handle, error) {
	sessionList, err := wtsEnumerateSessions()
	if err != nil {
		return 0xFFFFFFFF, err
	}

	for i := range sessionList {
		if sessionList[i].State == wtsActive {
			return sessionList[i].SessionID, nil
		}
	}
	// WTSGetActiveConsoleSessionId returns the session identifier of the session attached to the physical console.
	// Returns 0xFFFFFFFF if there isn't anything attached to the physical console.
	sessionID, _, err := procWTSGetActiveConsoleSessionID.Call()
	if sessionID == 0xFFFFFFFF {
		return 0xFFFFFFFF, fmt.Errorf("there are no active users connected to the physical console as per WTSGetActiveConsoleSessionID: %s", err)
	}
	return windows.Handle(sessionID), nil

}

// wtsEnumerateSessions retrieves the list of active sessions.
func wtsEnumerateSessions() ([]*wtsSessionInfo, error) {
	var (
		sessionInformation *wtsSessionInfo
		sessionCount       uint32
		sessionList        []*wtsSessionInfo
	)

	if returnCode, _, err := procWTSEnumerateSessionsW.Call(wtsCurrentServerHandle, 0, 1, uintptr(unsafe.Pointer(&sessionInformation)), uintptr(unsafe.Pointer(&sessionCount))); returnCode == 0 {
		return nil, fmt.Errorf("unable to call native WTSEnumerateSessionsW and list active sessions: %s", err)
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(sessionInformation)))

	// Limit array to prevent too large allocation in Go type system
	sessions := (*[1 << 27]wtsSessionInfo)(unsafe.Pointer(sessionInformation))[:sessionCount:sessionCount]
	for i := 0; i < int(sessionCount); i++ {
		infoCopy := sessions[i]
		sessionList = append(sessionList, &infoCopy)
	}

	return sessionList, nil
}

// DuplicateUserTokenFromSessionID will attempt to duplicate the user token for the user logged
// into the provided session ID
func DuplicateUserTokenFromSessionID(sessionID windows.Handle) (windows.Token, error) {
	var (
		impersonationToken windows.Handle
		userToken          windows.Token
	)
	if returnCode, _, err := procWTSQueryUserToken.Call(uintptr(sessionID), uintptr(unsafe.Pointer(&impersonationToken))); returnCode == 0 {
		return 0xFFFFFFFF, fmt.Errorf("unable to obtain the access token from WTSQueryUserToken: %s", err)
	}
	if returnCode, _, err := procDuplicateTokenEx.Call(uintptr(impersonationToken), 0, 0, uintptr(securityImpersonation), uintptr(tokenPrimary), uintptr(unsafe.Pointer(&userToken))); returnCode == 0 {
		return 0xFFFFFFFF, fmt.Errorf("unable to duplicate the access token using DuplicateTokenEx: %s", err)
	}
	if err := windows.CloseHandle(impersonationToken); err != nil {
		return 0xFFFFFFFF, fmt.Errorf("unable to close windows handle used for token duplication: %s", err)
	}

	return userToken, nil
}

// StartProcessAsCurrentUser runs the process as the current interactive user.
func StartProcessAsCurrentUser(appPath, cmdLine, workDir string) error {

	sessionID, err := GetCurrentUserSessionID()
	if err != nil {
		return err
	}

	userToken, err := DuplicateUserTokenFromSessionID(sessionID)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(windows.Handle(userToken))
	var envInfo windows.Handle
	if returnCode, _, err := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&envInfo)), uintptr(userToken), 0); returnCode == 0 {
		return fmt.Errorf("unable to create environment details for process: %s", err)
	}
	defer func() {
		if returnCode, _, err := procDestroyEnvironmentBlock.Call(uintptr(envInfo)); returnCode == 0 {
			deck.Errorf("DestroyEnvironmentBlock failed: %v", err)
		}
	}()

	creationFlags := createUnicodeEnvironment | createNewConsole
	startupInfo := windows.StartupInfo{
		ShowWindow: swShow,
		Desktop:    interactiveDesktop,
	}
	var commandLine uintptr
	var workingDir uintptr
	if len(cmdLine) > 0 {
		commandLine = uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(cmdLine)))
	}
	if len(workDir) > 0 {
		workingDir = uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(workDir)))
	}
	var processInfo windows.ProcessInformation
	if returnCode, _, err := procCreateProcessAsUser.Call(
		uintptr(userToken), uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(appPath))), commandLine, 0, 0, 0,
		uintptr(creationFlags), uintptr(envInfo), workingDir, uintptr(unsafe.Pointer(&startupInfo)), uintptr(unsafe.Pointer(&processInfo)),
	); returnCode == 0 {
		return fmt.Errorf("unable to create process as user: %s", err)
	}
	defer func() {
		if err := windows.CloseHandle(processInfo.Process); err != nil {
			deck.Errorf("CloseHandle(processInfo.Process) failed: %v", err)
		}
		if err := windows.CloseHandle(processInfo.Thread); err != nil {
			deck.Errorf("CloseHandle(processInfo.Thread) failed: %v", err)
		}
	}()

	return nil
}

// WTSQueryString queries WTS for string information.
func WTSQueryString(sessionID uint32, infoClass int) (string, error) {
	var buffer *uint16
	var bytesReturned uint32

	returnCode, _, err := procWTSQuerySessionInformationW.Call(
		0,
		uintptr(sessionID),
		uintptr(infoClass),
		uintptr(unsafe.Pointer(&buffer)),
		uintptr(unsafe.Pointer(&bytesReturned)),
	)
	if returnCode == 0 {
		return "", fmt.Errorf("wtsQuerySessionInformationW call failed: %v", err)
	}
	defer func() {
		// WTSFreeMemory is a void function. We ignore the return value and any error.
		_, _, _ = procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buffer)))
	}()

	return windows.UTF16PtrToString(buffer), nil
}

// User impersonates the logged-on user.
func User(token windows.Token) error {
	if returnCode, _, err := procImpersonateLoggedOnUser.Call(uintptr(token)); returnCode == 0 {
		return fmt.Errorf("impersonateLoggedOnUser failed: %s", err)
	}
	return nil
}

// RevertToSelf terminates the impersonation of the logged-on user.
func RevertToSelf() error {
	if returnCode, _, err := procRevertToSelf.Call(); returnCode == 0 {
		return fmt.Errorf("revertToSelf failed: %s", err)
	}
	return nil
}

// runAsUser executes the given function as the interactive user if currently running as SYSTEM.
func (i *Installer) runAsUser(f func() error) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	if !strings.HasSuffix(u.Username, "SYSTEM") {
		return f()
	}
	var userToken windows.Token
	var closeToken bool
	// Use the explicit client token if available.
	if i.clientToken != 0 {
		userToken = windows.Token(i.clientToken)
		closeToken = false
	}
	if closeToken {
		defer windows.CloseHandle(windows.Handle(userToken))
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := User(userToken); err != nil {
		return err
	}
	defer RevertToSelf()
	return f()
}

// ImpersonateNamedPipeClient wraps the underlying Windows API.
func ImpersonateNamedPipeClient(handle windows.Handle) error {
	if returnCode, _, err := procImpersonateNamedPipeClient.Call(uintptr(handle)); returnCode == 0 {
		return fmt.Errorf("impersonateNamedPipeClient failed: %v", err)
	}
	return nil
}
