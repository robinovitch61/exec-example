package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/escapingio"
	"github.com/moby/term"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	fmt.Println("Hello world")

	client, err := api.NewClient(clientConfig())
	if err != nil {
		fmt.Printf("Error initializing client: %v", err)
		os.Exit(1)
	}

	q := &api.QueryOptions{Namespace: "default"}
	alloc, _, err := client.Allocations().Info("e48de9e2-7195-5c02-130b-808aed4c77b4", q)
	if err != nil {
		fmt.Printf("Error querying allocation: %s", err)
		os.Exit(1)
	}

	code, err := execImpl(client, alloc, "redis", true, []string{"sh"}, "~", os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Printf("failed to exec into task: %v", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func clientConfig() *api.Config {
	config := api.DefaultConfig()

	//if m.flagAddress != "" {
	//	config.Address = m.flagAddress
	//}
	//if m.region != "" {
	//	config.Region = m.region
	//}
	//if m.namespace != "" {
	//	config.Namespace = m.namespace
	//}
	//
	//if m.token != "" {
	//	config.SecretID = m.token
	//}
	//
	//// Override TLS configuration fields we may have received from env vars with
	//// flag arguments from the user only if they're provided.
	//if m.caCert != "" {
	//	config.TLSConfig.CACert = m.caCert
	//}
	//
	//if m.caPath != "" {
	//	config.TLSConfig.CAPath = m.caPath
	//}
	//
	//if m.clientCert != "" {
	//	config.TLSConfig.ClientCert = m.clientCert
	//}
	//
	//if m.clientKey != "" {
	//	config.TLSConfig.ClientKey = m.clientKey
	//}
	//
	//if m.tlsServerName != "" {
	//	config.TLSConfig.TLSServerName = m.tlsServerName
	//}
	//
	//if m.insecure {
	//	config.TLSConfig.Insecure = m.insecure
	//}

	return config
}

// execImpl invokes the Alloc Exec api call, it also prepares and restores terminal states as necessary.
func execImpl(client *api.Client, alloc *api.Allocation, task string, tty bool,
	command []string, escapeChar string, stdin io.Reader, stdout, stderr io.WriteCloser) (int, error) {

	sizeCh := make(chan api.TerminalSize, 1)

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

	// When tty, ensures we capture all user input and monitor terminal resizes.
	if tty {
		if stdin == nil {
			return -1, fmt.Errorf("stdin is null")
		}

		inCleanup, err := setRawTerminal(stdin)
		if err != nil {
			return -1, err
		}
		defer inCleanup()

		outCleanup, err := setRawTerminalOutput(stdout)
		if err != nil {
			return -1, err
		}
		defer outCleanup()

		sizeCleanup, err := watchTerminalSize(stdout, sizeCh)
		if err != nil {
			return -1, err
		}
		defer sizeCleanup()

		if escapeChar != "" {
			stdin = escapingio.NewReader(stdin, escapeChar[0], func(c byte) bool {
				switch c {
				case '.':
					// need to restore tty state so error reporting here
					// gets emitted at beginning of line
					outCleanup()
					inCleanup()

					stderr.Write([]byte("\nConnection closed\n"))
					cancelFn()
					return true
				default:
					return false
				}
			})
		}
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range signalCh {
			cancelFn()
		}
	}()

	return client.Allocations().Exec(ctx,
		alloc, task, tty, command, stdin, stdout, stderr, sizeCh, nil)
}

// isTty returns true if both stdin and stdout are a TTY
func isTty() bool {
	_, isStdinTerminal := term.GetFdInfo(os.Stdin)
	_, isStdoutTerminal := term.GetFdInfo(os.Stdout)
	return isStdinTerminal && isStdoutTerminal
}

// setRawTerminal sets the stream terminal in raw mode, so process captures
// Ctrl+C and other commands to forward to remote process.
// It returns a cleanup function that restores terminal to original mode.
func setRawTerminal(stream interface{}) (cleanup func(), err error) {
	fd, isTerminal := term.GetFdInfo(stream)
	if !isTerminal {
		return nil, errors.New("not a terminal")
	}

	state, err := term.SetRawTerminal(fd)
	if err != nil {
		return nil, err
	}

	return func() { term.RestoreTerminal(fd, state) }, nil
}

// setRawTerminalOutput sets the output stream in Windows to raw mode,
// so it disables LF -> CRLF translation.
// It's basically a no-op on unix.
func setRawTerminalOutput(stream interface{}) (cleanup func(), err error) {
	fd, isTerminal := term.GetFdInfo(stream)
	if !isTerminal {
		return nil, errors.New("not a terminal")
	}

	state, err := term.SetRawTerminalOutput(fd)
	if err != nil {
		return nil, err
	}

	return func() { term.RestoreTerminal(fd, state) }, nil
}

// watchTerminalSize watches terminal size changes to propagate to remote tty.
func watchTerminalSize(out io.Writer, resize chan<- api.TerminalSize) (func(), error) {
	fd, isTerminal := term.GetFdInfo(out)
	if !isTerminal {
		return nil, errors.New("not a terminal")
	}

	ctx, cancel := context.WithCancel(context.Background())

	signalCh := make(chan os.Signal, 1)
	setupWindowNotification(signalCh)

	sendTerminalSize := func() {
		s, err := term.GetWinsize(fd)
		if err != nil {
			return
		}

		resize <- api.TerminalSize{
			Height: int(s.Height),
			Width:  int(s.Width),
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-signalCh:
				sendTerminalSize()
			}
		}
	}()

	go func() {
		// send initial size
		sendTerminalSize()
	}()

	return cancel, nil
}

func setupWindowNotification(ch chan<- os.Signal) {
	signal.Notify(ch, unix.SIGWINCH)
}
