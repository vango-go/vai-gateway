package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

const liveRecorderChunkBytes = 3200 // ~100ms at 16kHz mono PCM16

type livePCMRecorder struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	done   chan struct{}

	mu      sync.Mutex
	readErr error
	closed  bool
}

// newLivePCMRecorder streams raw PCM16 mono 16kHz chunks from the default mic.
func newLivePCMRecorder(onChunk func([]byte) error) (*livePCMRecorder, error) {
	cmd := exec.Command("sox",
		"-q",
		"-d",
		"-t", "raw",
		"-r", "16000",
		"-e", "signed",
		"-b", "16",
		"-c", "1",
		"-",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, err
	}

	r := &livePCMRecorder{
		cmd:    cmd,
		stdout: stdout,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(r.done)
		buf := make([]byte, liveRecorderChunkBytes)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				if onChunk != nil {
					if cbErr := onChunk(chunk); cbErr != nil {
						r.setReadErr(cbErr)
						return
					}
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				r.setReadErr(err)
				return
			}
		}
	}()

	return r, nil
}

func (r *livePCMRecorder) setReadErr(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readErr == nil {
		r.readErr = err
	}
}

func (r *livePCMRecorder) Close() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(syscall.SIGINT)
	}
	<-r.done
	if r.stdout != nil {
		_ = r.stdout.Close()
	}
	if r.cmd != nil {
		_ = r.cmd.Wait()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readErr
}
