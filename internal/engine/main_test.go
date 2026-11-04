package engine

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// No test may start a real Captain (a claude process) by sending it a message.
	testCaptainHook = func(string) {}
	os.Exit(m.Run())
}
