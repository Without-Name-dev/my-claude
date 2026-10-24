//go:build windows

package connector

import (
	"errors"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procUpdateProcThreadAttribute = kernel32.NewProc("UpdateProcThreadAttribute")
)

// setPseudoConsoleAttribute attaches the HPCON to the child's attribute list.
//
// The value is the handle itself, not a pointer to the variable holding it:
// UpdateProcThreadAttribute copies the handle value into the list. Calling the
// DLL directly keeps the uintptr the API expects without an unsafe.Pointer
// conversion that vet correctly flags.
func setPseudoConsoleAttribute(list *windows.ProcThreadAttributeList, hpc windows.Handle) error {
	ret, _, callErr := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(list)),
		0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0,
		0,
	)
	if ret == 0 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return callErr
		}
		return errors.New("UpdateProcThreadAttribute failed")
	}
	return nil
}

// windowsTerminal is one ConPTY session.
//
// ConPTY is the only way to give a child process a real console on modern
// Windows: a pair of pipes is handed to CreatePseudoConsole, the pseudo
// console handle is attached to the child through PROCESS_THREAD_ATTRIBUTE_
// PSEUDOCONSOLE, and the child's console traffic appears on the pipes.
type windowsTerminal struct {
	mu sync.Mutex

	hpc    windows.Handle
	in     *os.File // write end: our keystrokes
	out    *os.File // read end: the shell's output
	proc   windows.Handle
	job    windows.Handle
	pid    int
	closed bool
}

// startTerminal starts a shell attached to a new ConPTY.
func startTerminal(shell, cwd string, cols, rows int) (terminalProcess, error) {
	if shell == "" {
		shell = "powershell.exe"
	}
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, err
	}

	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}
	defer attributes.Delete()
	if err := setPseudoConsoleAttribute(attributes.List(), hpc); err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}

	commandLine, err := windows.UTF16FromString(shell)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}
	var cwdPtr *uint16
	if cwd != "" {
		cwdPtr, err = windows.UTF16PtrFromString(cwd)
		if err != nil {
			windows.ClosePseudoConsole(hpc)
			windows.CloseHandle(inRead)
			windows.CloseHandle(inWrite)
			windows.CloseHandle(outRead)
			windows.CloseHandle(outWrite)
			return nil, err
		}
	}

	// KILL_ON_JOB_CLOSE makes the whole process tree (the shell and anything
	// it spawned) die when the terminal is released, which is what stops
	// children from outliving a closed tab.
	job, jobErr := windows.CreateJobObject(nil, nil)
	if jobErr == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(
			job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
		); err != nil {
			windows.CloseHandle(job)
			job = 0
		}
	} else {
		job = 0
	}

	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attributes.List(),
	}
	var processInfo windows.ProcessInformation
	err = windows.CreateProcess(
		nil,
		&commandLine[0],
		nil,
		nil,
		false, // the pseudo console is inherited through the attribute list
		windows.EXTENDED_STARTUPINFO_PRESENT,
		nil,
		cwdPtr,
		&startup.StartupInfo,
		&processInfo,
	)
	if err != nil {
		if job != 0 {
			windows.CloseHandle(job)
		}
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}

	// The pseudo console keeps its own references to these ends.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	windows.CloseHandle(processInfo.Thread)

	if job != 0 {
		if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
			// Nested jobs are refused in a few locked-down environments.
			// Terminating the shell directly still works; only descendants
			// would escape. Do not fail the terminal for that.
			windows.CloseHandle(job)
			job = 0
		}
	}

	return &windowsTerminal{
		hpc:  hpc,
		in:   os.NewFile(uintptr(inWrite), "conpty-input"),
		out:  os.NewFile(uintptr(outRead), "conpty-output"),
		proc: processInfo.Process,
		job:  job,
		pid:  int(processInfo.ProcessId),
	}, nil
}

func (t *windowsTerminal) Read(p []byte) (int, error) {
	t.mu.Lock()
	file := t.out
	t.mu.Unlock()
	if file == nil {
		return 0, io.EOF
	}
	return file.Read(p)
}

func (t *windowsTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	file := t.in
	t.mu.Unlock()
	if file == nil {
		return 0, errors.New("terminal is closed")
	}
	return file.Write(p)
}

func (t *windowsTerminal) Resize(cols, rows int) error {
	t.mu.Lock()
	hpc := t.hpc
	t.mu.Unlock()
	if hpc == 0 {
		return errors.New("terminal is closed")
	}
	return windows.ResizePseudoConsole(hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (t *windowsTerminal) Wait() (int, error) {
	t.mu.Lock()
	proc := t.proc
	t.mu.Unlock()
	if proc == 0 {
		return 0, errors.New("terminal has no process")
	}
	if _, err := windows.WaitForSingleObject(proc, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(proc, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}

func (t *windowsTerminal) Kill() error {
	t.mu.Lock()
	job, proc := t.job, t.proc
	t.mu.Unlock()
	if job != 0 {
		return windows.TerminateJobObject(job, 1)
	}
	if proc != 0 {
		return windows.TerminateProcess(proc, 1)
	}
	return nil
}

// Close shuts the pseudo console down. Output already buffered in the pipe is
// still readable until the reader reaches EOF; Release closes that end.
func (t *windowsTerminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.hpc != 0 {
		windows.ClosePseudoConsole(t.hpc)
		t.hpc = 0
	}
	if t.in != nil {
		_ = t.in.Close()
		t.in = nil
	}
	return nil
}

func (t *windowsTerminal) Release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.out != nil {
		_ = t.out.Close()
		t.out = nil
	}
	if t.proc != 0 {
		_ = windows.CloseHandle(t.proc)
		t.proc = 0
	}
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}

func (t *windowsTerminal) PID() int { return t.pid }
