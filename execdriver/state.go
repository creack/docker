package execdriver

import (
	"bufio"
	"fmt"
	"github.com/dotcloud/docker/utils"
	"os"
	"sync"
	"time"
)

const (
	DefaultTimeFormat = time.RFC3339
)

// FIXME: Use only private members and create an API type for the transport
type State struct {
	sync.RWMutex
	running    bool
	pid        int
	exitCode   int
	startedAt  time.Time
	finishedAt time.Time
	ghost      bool
}

// String returns a human-readable description of the state
func (s *State) String() string {
	s.RLock()
	defer s.RUnlock()

	if s.running {
		if s.ghost {
			return fmt.Sprintf("Ghost")
		}
		return fmt.Sprintf("Up %s", utils.HumanDuration(time.Now().UTC().Sub(s.startedAt)))
	}
	return fmt.Sprintf("Exit %d", s.exitCode)
}

func (s *State) IsRunning() bool {
	s.RLock()
	defer s.RUnlock()

	return s.running
}

func (s *State) IsGhost() bool {
	s.RLock()
	defer s.RUnlock()

	return s.ghost
}

func (s *State) GetPid() int {
	s.RLock()
	defer s.RUnlock()

	return s.pid
}

func (s *State) GetExitCode() int {
	s.RLock()
	defer s.RUnlock()

	return s.exitCode
}

func (s *State) SetGhost(val bool) {
	s.Lock()
	defer s.Unlock()

	s.ghost = val
}

func (s *State) SetPid(pid int) {
	s.Lock()
	defer s.Unlock()

	s.pid = pid
}

func (s *State) SetRunning(pid int) {
	s.Lock()
	defer s.Unlock()

	s.running = true
	s.ghost = false
	s.exitCode = 0
	s.pid = pid
	s.startedAt = time.Now().UTC()
}

func (s *State) SetStopped(exitCode int) {
	s.Lock()
	defer s.Unlock()

	s.running = false
	s.pid = 0
	s.finishedAt = time.Now().UTC()
	s.exitCode = exitCode
}

func (s *State) GetStartedAt() *time.Time {
	s.RLock()
	defer s.RUnlock()

	return &s.startedAt
}

func (s *State) GetFinishedAt() *time.Time {
	s.RLock()
	defer s.RUnlock()

	return &s.finishedAt
}

func SaveState(state *State, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, "%v\t%d\t%d\t%s\t%s\n", state.IsRunning(), state.GetPid(),
		state.GetExitCode(), state.GetStartedAt().UTC().Format(DefaultTimeFormat), state.GetFinishedAt().UTC().Format(DefaultTimeFormat))
	return err
}

func LoadState(path string) (*State, error) {
	var (
		running          bool
		pid              int
		exitcode         int
		startStr, endStr string
		start, end       time.Time
	)

	// open the file
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Create a scanner
	r := bufio.NewScanner(f)
	// Scan until the end
	for r.Scan() {
	}

	// r.Text() is now the last line of the file, scanf it into variables
	if _, err = fmt.Sscanf(r.Text(), "%v\t%d\t%d\t%s\t%s\n", &running, &pid, &exitcode, &startStr, &endStr); err != nil {
		return nil, err
	}

	// Parse the time as string into time.Time
	start, err = time.Parse(time.RFC3339, startStr)
	if err != nil {
		return nil, err
	}
	end, err = time.Parse(time.RFC3339, endStr)
	if err != nil {
		return nil, err
	}

	return &State{
		running:    running,
		pid:        pid,
		startedAt:  start,
		finishedAt: end,
		exitCode:   exitcode,
	}, nil
}
