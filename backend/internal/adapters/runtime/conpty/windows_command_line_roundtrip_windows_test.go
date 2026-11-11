//go:build windows

package conpty

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// parseWindowsCommandLine runs the command line through the real OS parser
// (shell32!CommandLineToArgvW), the same splitting a native .exe child performs
// on its CreateProcess command line.
func parseWindowsCommandLine(t *testing.T, commandLine string) []string {
	t.Helper()
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	commandLineToArgvW := shell32.NewProc("CommandLineToArgvW")
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	localFree := kernel32.NewProc("LocalFree")

	var nArgs int32
	wide, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", commandLine, err)
	}
	rv, _, callErr := commandLineToArgvW.Call(uintptr(unsafe.Pointer(wide)), uintptr(unsafe.Pointer(&nArgs)))
	if rv == 0 {
		t.Fatalf("CommandLineToArgvW(%q) failed: %v", commandLine, callErr)
	}
	defer localFree.Call(rv)
	ptrs := unsafe.Slice(*(*(**uint16))(unsafe.Pointer(&rv)), int(nArgs))
	out := make([]string, len(ptrs))
	for i, p := range ptrs {
		out[i] = windows.UTF16PtrToString(p)
	}
	return out
}

// AO's launcher escape rules must round-trip the full Qoder worker launch:
// multi-line standing instructions, embedded quotes/backslashes/unicode, and a
// 40-byte leading-dash prompt, so no flag is dropped between composition and
// the native child's argv.
func TestWindowsCommandLineRoundTripNativeChildArgs(t *testing.T) {
	var instructions strings.Builder
	for line := 0; instructions.Len() < 7208; line++ {
		fmt.Fprintf(&instructions, "standing instruction %d: quote \"x\", path C:\\ao\\worker, unicode 🙂\n", line)
	}
	prompt := fmt.Sprintf("%-40s", "-reply OK without tools")

	args := []string{`C:\Users\worker\.qoder\bin\qodercli\qodercli.exe`, "--permission-mode", "accept_edits", "--append-system-prompt", instructions.String()[:7208], "--prompt-interactive=" + prompt}
	if err := validateWindowsCommandLine(args); err != nil {
		t.Fatalf("launch command rejected: %v", err)
	}
	parsed := parseWindowsCommandLine(t, windowsCommandLine(args))
	if len(parsed) != len(args) {
		t.Fatalf("argv count = %d, want %d\nparsed: %#v", len(parsed), len(args), parsed)
	}
	for i := range args {
		if parsed[i] != args[i] {
			t.Fatalf("argv[%d] drift\n got: %q\nwant: %q", i, parsed[i], args[i])
		}
	}

	// The pinned x/sys composer the ConPTY host uses must agree on the same
	// launch, so the child sees every flag when AO execs the native exe.
	composed := parseWindowsCommandLine(t, windows.ComposeCommandLine(args))
	if len(composed) != len(args) {
		t.Fatalf("ComposeCommandLine argv count = %d, want %d", len(composed), len(args))
	}
	for i := range args {
		if composed[i] != args[i] {
			t.Fatalf("ComposeCommandLine argv[%d] drift\n got: %q\nwant: %q", i, composed[i], args[i])
		}
	}
}
