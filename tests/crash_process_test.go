package tests

import (
	"os/exec"
	"testing"
)

// Exercise both shutdown paths without opting into the full crash workload.
// The race detector checks that shutdown never competes with the process waiter.
func TestCrashProcess_ShutdownWaitsForExit(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		name := "kill"
		if graceful {
			name = "graceful"
		}
		t.Run(name, func(t *testing.T) {
			command := exec.Command("sleep", "60")
			err := command.Start()
			if err != nil {
				t.Fatal(err)
			}
			server := watchCrashServer(command)
			harness := &crashHarness{t: t}
			t.Cleanup(func() { harness.kill(server) })
			if graceful {
				harness.stopGracefully(server)
			} else {
				harness.kill(server)
			}
			select {
			case <-server.exited:
			default:
				t.Fatal("shutdown returned before the process waiter finished")
			}
			if command.ProcessState == nil {
				t.Fatal("shutdown returned without collecting the process state")
			}
		})
	}
}
