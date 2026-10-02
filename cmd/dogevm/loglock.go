package main

import (
	"errors"
	"fmt"
	"os"
)

// errLocked: another process holds a lock this one asked for without
// waiting.
var errLocked = errors.New("locked by another process")

// errSignerRunning means a signer process holds its signing log's lock.
var errSignerRunning = errors.New("the signer using this signing log is running; stop it first")

// lockSigningLog takes the lock that lets one process at a time write a
// signing log: the signer, for as long as it runs, or a signer-log command
// that changes the log (init, adopt, anchors -set). Each writes the whole
// log from what it read, so two at once would lose what the other wrote: a
// command that read the log, then wrote it back after the signer had
// recorded a signature, would erase that signature. The lock lives in a
// sibling file (signing-log.json.lock) and is released when the process
// exits, however it exits. The caller closes the file to release it.
func lockSigningLog(logPath string) (*os.File, error) {
	path := logPath + ".lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
			return nil, errSignerRunning
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}
