package appliance

import (
	"errors"
	"sync"
	"time"
)

// The states a job, or the updater, ends in.
const (
	stateRunning   = "running"
	stateSucceeded = "succeeded"
	stateFailed    = "failed"
)

// Job is one run of the module.
type Job struct {
	User string `json:"user"`
	// Automatic tells a run the appliance started on its own, as User, from an automatic upgrade.
	Automatic bool `json:"automatic,omitempty"`
	// Kind is apply or remove.
	Kind     string    `json:"kind"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// State is running, succeeded or failed.
	State string `json:"state"`
	Error string `json:"error,omitempty"`

	mu  sync.Mutex
	log []string
}

// Log returns the job's messages so far, an empty list rather than none: it is sent and stored as
// JSON, where the interface reads a list.
func (j *Job) Log() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string{}, j.log...)
}

// Add logs a line into the job, for steps a caller runs within it.
func (j *Job) Add(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
}

// running reports whether j, which may be nil, is still running. The job's goroutine sets its
// outcome under j.mu alone.
func (j *Job) running() bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.State == stateRunning
}

// view is the job as the interface shows it; nil for none.
func (j *Job) view() *JobView {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	shown := j.log[max(0, len(j.log)-shownLines):]
	v := &JobView{User: j.User, Automatic: j.Automatic, Kind: j.Kind, Started: j.Started, Finished: j.Finished, State: j.State, Error: j.Error, Log: append([]string{}, shown...)}
	if len(shown) < len(j.log) {
		v.Lines = len(j.log)
	}
	return v
}

// JobView is a job as the interface shows it.
type JobView struct {
	User      string    `json:"user"`
	Automatic bool      `json:"automatic,omitempty"`
	Kind      string    `json:"kind"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitzero"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	Log       []string  `json:"log"`
	// Lines is how many lines the log has in all, where Log holds only the last of them.
	Lines int `json:"lines,omitempty"`
}

// shownLines is how much of a job's log its view carries: the interface polls it every few seconds
// and shows the last 500 lines, while an apply of the management cluster logs thousands as it waits.
const shownLines = 500

// ErrBusy is a job already running.
var ErrBusy = errors.New("the management cluster is being applied or removed; try again once that has finished")

// ErrUpgrading is the appliance's software being replaced, which restarts the interface and would
// cut a run off.
var ErrUpgrading = errors.New("the appliance is being upgraded; try again once it is back")

// ErrTemplateBusy is a VM template being built, which holds the same lock as runs of the module.
var ErrTemplateBusy = errors.New("a VM template is being built; try again once that has finished")

var errLocked = errors.New("locked")

// LockJobs keeps jobs from starting until the returned unlock is called, and fails with ErrBusy
// while one runs. The updater holds it across replacing the software and restarting the service;
// the installer takes the same lock on Dir with flock(1). The kernel drops it when its holder ends,
// so a crash leaves none behind.
func LockJobs(dir string) (func(), error) {
	unlock, err := tryLock(dir)
	if errors.Is(err, errLocked) {
		return nil, ErrBusy
	}
	return unlock, err
}
