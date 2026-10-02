//go:build unix

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHelperHoldSigningLog is not a test: run as a separate process by
// TestSigningLogCommandsRefuseWhileTheSignerRuns, it holds a log's lock as
// a running signer does, says so, and waits to be killed.
func TestHelperHoldSigningLog(t *testing.T) {
	path := os.Getenv("DOGEVM_HOLD_LOG")
	if path == "" {
		t.Skip("helper process only")
	}
	f, err := lockSigningLog(path)
	if err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	defer f.Close()
	os.Stdout.WriteString("held\n")
	select {}
}

// A signer-log command that writes the log must refuse while another
// process (the running signer) holds it: otherwise it could write back an
// old copy over a signature the signer just recorded.
func TestSigningLogCommandsRefuseWhileTheSignerRuns(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "signing-log.json")
	_, err := createSigningLog(logPath)
	require.NoError(t, err)

	cmd := exec.Command(os.Args[0], "-test.run", "^TestHelperHoldSigningLog$")
	cmd.Env = append(os.Environ(), "DOGEVM_HOLD_LOG="+logPath)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "held\n", line)

	_, err = lockSigningLog(logPath)
	require.ErrorIs(t, err, errSignerRunning)
	for _, step := range []string{"adopt", "init"} {
		err = cmdSignerLog([]string{step, "-signers", filepath.Join(dir, "signers.json"), "-key-file", filepath.Join(dir, "signer.key"),
			"-log", logPath, "-doge-network", "regtest", "-vm-network", "testnet"})
		require.ErrorIs(t, err, errSignerRunning, step)
	}
	err = cmdSignerLog([]string{"anchors", "-set", "-signers", filepath.Join(dir, "signers.json"), "-key-file", filepath.Join(dir, "signer.key"),
		"-log", logPath, "-doge-network", "regtest", "-vm-network", "testnet"})
	require.ErrorIs(t, err, errSignerRunning)

	// Once the signer has stopped, the lock is free again.
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	f, err := lockSigningLog(logPath)
	require.NoError(t, err)
	f.Close()
}
