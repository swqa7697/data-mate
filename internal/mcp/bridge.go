package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/muesli/cancelreader"
	"golang.org/x/sys/unix"
)

// Bridge relays complete bounded frames only. It owns the socket and streams;
// closing them interrupts pending reads and writes on disconnect/cancellation.
func Bridge(ctx context.Context, socket net.Conn, input io.ReadCloser, output io.WriteCloser) error {
	writer, restore, err := pollableOutput(output)
	if err != nil {
		socket.Close()
		input.Close()
		output.Close()
		return errProtocol
	}
	defer restore()
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		socket.Close()
		writer.Close()
		input.Close()
		return errProtocol
	}
	defer reader.Close()
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			socket.Close()
			reader.Cancel()
			input.Close()
			writer.Close()
		})
	}
	defer closeAll()
	stop := context.AfterFunc(ctx, closeAll)
	defer stop()
	type completion struct {
		err   error
		input bool
	}
	done := make(chan completion, 2)
	relay := func(dst io.Writer, src io.Reader, limit int, input bool) {
		reader := bufio.NewReaderSize(src, limit+1)
		for {
			line, err := reader.ReadSlice('\n')
			if err != nil {
				done <- completion{err, input}
				return
			}
			if len(line) > limit {
				done <- completion{errProtocol, input}
				return
			}
			timer := time.AfterFunc(5*time.Second, closeAll)
			_, err = io.Copy(dst, bytes.NewReader(line))
			timer.Stop()
			if err != nil {
				done <- completion{err, input}
				return
			}
		}
	}
	go relay(socket, reader, inboundLimit, true)
	go relay(writer, socket, outboundLimit, false)
	result := <-done
	closeAll()
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if result.input && errors.Is(result.err, io.EOF) {
		return nil
	}
	return errProtocol
}

// Inherited stdout is normally a blocking descriptor that os.File.Close cannot
// interrupt during Write. Register a nonblocking duplicate with Go's poller so
// shutdown and the five-second output bound also hold for real process pipes.
func pollableOutput(output io.WriteCloser) (io.WriteCloser, func(), error) {
	file, ok := output.(*os.File)
	if !ok {
		return output, func() {}, nil
	}
	original := int(file.Fd())
	flags, err := unix.FcntlInt(uintptr(original), unix.F_GETFL, 0)
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.FcntlInt(uintptr(original), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	restore := func() { _ = unix.SetNonblock(original, flags&unix.O_NONBLOCK != 0); _ = output.Close() }
	writer := os.NewFile(uintptr(fd), file.Name())
	if err = writer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		writer.Close()
		restore()
		return nil, nil, err
	}
	// Each frame has its own timer; this initial deadline only checks support.
	_ = writer.SetWriteDeadline(time.Time{})
	return writer, restore, nil
}
