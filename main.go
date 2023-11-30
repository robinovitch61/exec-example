package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/hashicorp/nomad/api"
	"github.com/moby/term"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/signal"
	"syscall"
)

type execFinishedMsg struct{ err error }

type cmd struct {
	tea.ExecCommand
}

func (c cmd) Run() error {
	_, err := allocExec()

	//r := exec.Command("bash")
	//err := r.Run()

	if err != nil {
		return err
	}
	return nil
}

func (c cmd) SetStdin(r io.Reader) {
	return
}

func (c cmd) SetStdout(w io.Writer) {
	return
}

func (c cmd) SetStderr(w io.Writer) {
	return
}

func runAllocExec() tea.Cmd {
	return tea.Exec(cmd{}, func(err error) tea.Msg {
		return execFinishedMsg{err}
	})
}

type model struct {
	err     error
	lastMsg tea.Msg
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.lastMsg = msg
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "e":
			return m, runAllocExec()
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	case execFinishedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return "Error: " + m.err.Error() + "\n"
	}
	return fmt.Sprintf("Msg %T\nPress 'e' to run.\nPress 'q' to quit.\n", m.lastMsg)
}

func main() {
	m := model{}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}

func allocExec() (int, error) {
	client, err := api.NewClient(clientConfig())
	if err != nil {
		fmt.Printf("Error initializing client: %v", err)
		return 1, err
	}

	q := &api.QueryOptions{Namespace: "default"}
	alloc, _, err := client.Allocations().Info("e48de9e2-7195-5c02-130b-808aed4c77b4", q)
	if err != nil {
		fmt.Printf("Error querying allocation: %s", err)
		return 1, err
	}

	code, err := execImpl(client, alloc, "redis", []string{"sh"}, "~", os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Printf("failed to exec into task: %v", err)
		return 1, err
	}
	return code, nil
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
func execImpl(client *api.Client, alloc *api.Allocation, task string,
	command []string, escapeChar string, stdin io.Reader, stdout, stderr io.WriteCloser) (int, error) {

	sizeCh := make(chan api.TerminalSize, 1)

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

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

	stdin = NewReader(stdin, escapeChar[0], func(c byte) bool {
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

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range signalCh {
			cancelFn()
		}
	}()

	return client.Allocations().Exec(ctx,
		alloc, task, true, command, stdin, stdout, stderr, sizeCh, nil)
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

	return func() {
		term.RestoreTerminal(fd, state)
	}, nil
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
	//_, err = term.SetRawTerminalOutput(fd)
	if err != nil {
		return nil, err
	}

	return func() {
		term.RestoreTerminal(fd, state)
	}, nil
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

// Handler is a callback for handling an escaped char.  Reader would skip
// the escape char and passed char if returns true; otherwise, it preserves them
// in output
type Handler func(c byte) bool

// NewReader returns a reader that escapes the c character (following new lines),
// in the same manner OpenSSH handling, which defaults to `~`.
//
// For illustrative purposes, we use `~` in documentation as a shorthand for escaping character.
//
// If following a new line, reader sees:
//   - `~~`, only one is emitted
//   - `~.` (or any character), the handler is invoked with the character.
//     If handler returns true, `~.` will be skipped; otherwise, it's propagated.
//   - `~` and it's the last character in stream, it's propagated
//
// Appearances of `~` when not preceded by a new line are propagated unmodified.
func NewReader(r io.Reader, c byte, h Handler) io.Reader {
	pr, pw := io.Pipe()
	reader := &reader{
		impl:       r,
		escapeChar: c,
		handler:    h,
		pr:         pr,
		pw:         pw,
	}
	go reader.pipe()
	return reader
}

// lookState represents the state of reader for what character of `\n~.` sequence
// reader is looking for
type lookState int

const (
	// sLookNewLine indicates that reader is looking for new line
	sLookNewLine lookState = iota

	// sLookEscapeChar indicates that reader is looking for ~
	sLookEscapeChar

	// sLookChar indicates that reader just read `~` is waiting for next character
	// before acting
	sLookChar
)

// to ease comments, i'll assume escape character to be `~`
type reader struct {
	impl       io.Reader
	escapeChar uint8
	handler    Handler

	// buffers
	pw *io.PipeWriter
	pr *io.PipeReader
}

func (r *reader) Read(buf []byte) (int, error) {
	return r.pr.Read(buf)
}

// TODO LEO: cancel channel if back to bubble tea??
func (r *reader) pipe() {
	rb := make([]byte, 4096)
	bw := bufio.NewWriter(r.pw)

	state := sLookEscapeChar

	for {
		n, err := r.impl.Read(rb)

		if n > 0 {
			state = r.processBuf(bw, rb, n, state)
			bw.Flush()
			if state == sLookChar {
				// terminated with ~ - let's read one more character
				n, err = r.impl.Read(rb[:1])
				if n == 1 {
					state = sLookNewLine
					if rb[0] == r.escapeChar {
						// only emit escape character once
						bw.WriteByte(rb[0])
						bw.Flush()
					} else if r.handler(rb[0]) {
						// skip if handled
					} else {
						bw.WriteByte(r.escapeChar)
						bw.WriteByte(rb[0])
						bw.Flush()
						if rb[0] == '\n' || rb[0] == '\r' {
							state = sLookEscapeChar
						}
					}
				}
			}
		}

		if err != nil {
			// write ~ if it's the last thing
			if state == sLookChar {
				bw.WriteByte(r.escapeChar)
			}
			bw.Flush()
			r.pw.CloseWithError(err)
			break
		}
	}
}

// processBuf process buffer and emits all output to writer
// if the last part of buffer is a new line followed by sequnce, it writes
// all output until the new line and returns sLookChar
func (r *reader) processBuf(bw io.Writer, buf []byte, n int, s lookState) lookState {
	i := 0

	wi := 0

START:
	if s == sLookEscapeChar && buf[i] == r.escapeChar {
		if i+1 >= n {
			// buf terminates with ~ - write all before
			bw.Write(buf[wi:i])
			return sLookChar
		}

		nc := buf[i+1]
		if nc == r.escapeChar {
			// skip one escape char
			bw.Write(buf[wi:i])
			i++
			wi = i
		} else if r.handler(nc) {
			// skip both characters
			bw.Write(buf[wi:i])
			i = i + 2
			wi = i
		} else if nc == '\n' || nc == '\r' {
			i = i + 2
			s = sLookEscapeChar
			goto START
		} else {
			i = i + 2
			// need to write everything keep going
		}
	}

	// search until we get \n~, or buf terminates
	for {
		if i >= n {
			// got to end without new line, write and return
			bw.Write(buf[wi:n])
			return sLookNewLine
		}

		if buf[i] == '\n' || buf[i] == '\r' {
			// buf terminated at new line
			if i+1 >= n {
				bw.Write(buf[wi:n])
				return sLookEscapeChar
			}

			// peek to see escape character go back to START if so
			if buf[i+1] == r.escapeChar {
				s = sLookEscapeChar
				i++
				goto START
			}
		}

		i++
	}
}
