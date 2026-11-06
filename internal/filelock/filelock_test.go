package filelock

import (
	"os"
	"testing"
)

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("current pid must look alive")
	}
	if pidAlive(-1) || pidAlive(0) {
		t.Fatal("invalid pids must be dead")
	}
}
