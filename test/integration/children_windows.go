//go:build integration && windows

package integration

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// EndsChildrenItself tells whether the package ends the API server and etcd itself, as here: the
// envtest controller-runtime builds against stops them with SIGTERM, which Windows cannot send, so
// they would outlive the tests. The test binary holds them in a job object instead: EndChildren ends them before
// envtest removes their directories, and the job ends them when the binary exits however it ends, a
// timeout or a panic included.
const EndsChildrenItself = true

// endMillis is how long EndChildren waits for each process it ends.
const endMillis = 30_000

var job windows.Handle

// OwnChildren puts the test binary into the job object, and with it every process it starts from
// then on; a package calls it once, before it starts its first API server.
func OwnChildren() error {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("job object: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil { //nolint:gosec // the Win32 call takes the structure by address
		return fmt.Errorf("job object limits: %w", err)
	}
	if err := windows.AssignProcessToJobObject(h, windows.CurrentProcess()); err != nil {
		return fmt.Errorf("joining the job object: %w", err)
	}
	job = h
	return nil
}

// processList is JOBOBJECT_BASIC_PROCESS_ID_LIST with room for more processes than a test starts.
type processList struct {
	assigned, listed uint32
	ids              [64]uintptr
}

// EndChildren ends every process in the job but the test binary, and waits until they are gone.
func EndChildren() error {
	var list processList
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil); err != nil { //nolint:gosec // the Win32 call fills the structure by address
		return fmt.Errorf("listing the job's processes: %w", err)
	}
	self := windows.GetCurrentProcessId()
	var errs []error
	for _, wide := range list.ids[:list.listed] {
		id := uint32(wide) //nolint:gosec // process IDs are DWORDs, which the list widens to ULONG_PTR
		if id == self {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, id)
		if err != nil {
			// Exited since the list was taken.
			continue
		}
		// Access is denied to a process already ending.
		if err := windows.TerminateProcess(h, 1); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			errs = append(errs, fmt.Errorf("ending process %d: %w", id, err))
		} else if ev, err := windows.WaitForSingleObject(h, endMillis); err != nil {
			errs = append(errs, fmt.Errorf("waiting for process %d: %w", id, err))
		} else if ev != windows.WAIT_OBJECT_0 {
			errs = append(errs, fmt.Errorf("process %d did not end within %d ms", id, endMillis))
		}
		_ = windows.CloseHandle(h)
	}
	return errors.Join(errs...)
}
