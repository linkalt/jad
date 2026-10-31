// Package browser asks the desktop to open a URL, for the commands that sign in through a browser.
package browser

import (
	"errors"
	"os/exec"
	"runtime"
)

// Open asks the desktop to open url. The URL goes to the program as one argument, never
// through a shell. The caller prints the URL as well, so a failure here is not fatal.
func Open(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		command = exec.Command("xdg-open", url)
	default:
		return errors.New("opening a browser is not supported on " + runtime.GOOS)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
