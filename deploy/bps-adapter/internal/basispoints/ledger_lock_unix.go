//go:build unix

package basispoints

import (
	"os"
	"syscall"
	"time"
)

// lockLedgerFile takes an exclusive flock on path for one ledger save
// (ledger_handover.go). flock belongs to the open file, not the process, so
// two plugin instances -- two Go runtimes inside the one CPA process -- that
// each open the lock file exclude each other. A save never waits long for
// it: past ledgerLockWait it goes ahead unlocked, as every save used to.
func lockLedgerFile(path string) func() {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return func() {}
	}
	fd := int(file.Fd())
	deadline := time.Now().Add(ledgerLockWait)
	for {
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = file.Close()
			}
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			countRelayEvent("ledger_handover:lock_timeout")
			return func() {}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const ledgerLockWait = 2 * time.Second
