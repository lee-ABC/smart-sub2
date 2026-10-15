//go:build !unix

package basispoints

// lockLedgerFile: the plugin ships for linux only; elsewhere a save is
// unlocked, as every save used to be.
func lockLedgerFile(path string) func() { return func() {} }
