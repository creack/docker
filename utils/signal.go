package utils

import (
	"os"
	"os/signal"
)

func StopCatch(sigc chan os.Signal) {
	signal.Stop(sigc)
	close(sigc)
}

// Suspend sends SIGSTOP to the calling process.
func Suspend() error {
	return suspend()
}
