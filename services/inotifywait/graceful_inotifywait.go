package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	// Fail early if required binaries are missing from the image.
	if err := validateRequiredBinaries(); err != nil {
		log.Fatal(err)
	}

	if len(os.Args) < 3 {
		log.Fatalf("usage: %s <process-name> <file-to-watch> [<file-to-watch>...]", os.Args[0])
	}

	command := buildInotifyWaitCommand(os.Args[1], os.Args[2:])
	log.Println(command)

	// Distroless has shell path at /bin/sh, UBI has shell path at /usr/bin/sh
	// Both the shells are present in the executable PATH, hence directly using "sh".
	cmd := exec.Command(
		"sh",
		"-ecx",
		command,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Set different pgid for child processes to avoid parent process
	// committing accidental seppuku when sending SIGKILL to the process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	waitForSIGTERM()

	// A negative pid is interpreted as pgid, effectively killing all the
	// process' progeny.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		log.Fatal(err)
	}

	if err := cmd.Wait(); err != nil {
		log.Println(err)
	}
}

const inotifywaitCommand = `

while inotifywait -e close_write,moved_to,create %s;
do
	pkill -HUP %s;
done
`

func buildInotifyWaitCommand(processName string, fileNames []string) string {
	filesToWatch := strings.Join(fileNames, " ")
	return fmt.Sprintf(inotifywaitCommand, filesToWatch, processName)
}

// validateRequiredBinaries checks for the presence of inotifywait and pkill.
// Without these binaries, the file watcher cannot function.
func validateRequiredBinaries() error {
	if _, err := exec.LookPath("inotifywait"); err != nil {
		return fmt.Errorf("inotifywait binary not found in PATH: %w", err)
	}

	if _, err := exec.LookPath("pkill"); err != nil {
		return fmt.Errorf("pkill binary not found in PATH: %w", err)
	}

	return nil
}

func waitForSIGTERM() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM)
	<-sigChan
}
