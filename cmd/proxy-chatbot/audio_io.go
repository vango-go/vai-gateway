package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// pcmPlayer pipes raw PCM audio (signed 16-bit LE, mono, 24 kHz) to sox for playback.
type pcmPlayer struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// newPCMPlayer spawns a sox subprocess that reads raw PCM from stdin and plays it.
func newPCMPlayer() (*pcmPlayer, error) {
	cmd := exec.Command("sox",
		"-q",           // suppress progress bar (avoid terminal noise)
		"-t", "raw",    // raw PCM input
		"-r", "24000",  // 24 kHz sample rate
		"-e", "signed", // signed integer encoding
		"-b", "16",     // 16-bit samples
		"-c", "1",      // mono
		"-", "-d",      // stdin → default audio device
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	cmd.Stderr = os.Stderr // errors still visible, -q suppresses progress bar

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}

	return &pcmPlayer{cmd: cmd, stdin: stdin}, nil
}

// Write sends PCM bytes to the sox subprocess.
func (p *pcmPlayer) Write(data []byte) (int, error) {
	if p == nil || p.stdin == nil {
		return 0, nil
	}
	return p.stdin.Write(data)
}

// Close pads a small silence buffer so the OS audio device finishes playing
// the final samples, then closes the stdin pipe and waits for sox to exit.
func (p *pcmPlayer) Close() error {
	if p == nil {
		return nil
	}
	if p.stdin != nil {
		// 250ms of silence at 24kHz, 16-bit mono = 12000 bytes of zeros.
		// Without this, the OS audio buffer may not fully drain before sox exits.
		silence := make([]byte, 24000/4*2) // 250ms
		p.stdin.Write(silence)
		p.stdin.Close()
	}
	if p.cmd != nil {
		return p.cmd.Wait()
	}
	return nil
}

// pcmRecorder captures audio from the default microphone via sox.
type pcmRecorder struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	buf    bytes.Buffer
	done   chan struct{}
	err    error
}

// newPCMRecorder spawns a sox subprocess that records from the mic as raw PCM (16 kHz, 16-bit, mono).
func newPCMRecorder() (*pcmRecorder, error) {
	cmd := exec.Command("sox",
		"-q",           // suppress progress bar (would garble terminal input)
		"-d",           // default audio input device
		"-t", "raw",    // raw PCM output
		"-r", "16000",  // 16 kHz (Cartesia STT default)
		"-e", "signed", // signed integer encoding
		"-b", "16",     // 16-bit samples
		"-c", "1",      // mono
		"-",            // output to stdout
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		stdout.Close()
		return nil, err
	}

	rec := &pcmRecorder{
		cmd:    cmd,
		stdout: stdout,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(rec.done)
		_, rec.err = io.Copy(&rec.buf, stdout)
	}()

	return rec, nil
}

// Stop stops recording, returns the captured audio as WAV bytes.
func (r *pcmRecorder) Stop() ([]byte, error) {
	if r == nil {
		return nil, nil
	}

	// SIGINT tells sox to stop gracefully (flushes buffers).
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(syscall.SIGINT)
	}

	<-r.done // wait for reader goroutine to finish
	_ = r.cmd.Wait()

	if r.err != nil {
		return nil, r.err
	}
	if r.buf.Len() == 0 {
		return nil, nil
	}

	return encodeWAV(r.buf.Bytes(), 16000, 16, 1), nil
}

// encodeWAV prepends a 44-byte WAV header to raw PCM data.
func encodeWAV(pcm []byte, sampleRate, bitsPerSample, channels int) []byte {
	dataSize := len(pcm)
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataSize))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // PCM format
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:36], uint16(bitsPerSample))
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))
	copy(buf[44:], pcm)
	return buf
}
