package speech

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

type wavChunk struct {
	path   string
	offset time.Duration
}

// ExtractWAV writes 16 kHz mono PCM wav for input (a file or URL ffmpeg can read).
// cleanup removes the file. The caller asks for this only after a viewer
// requests subtitles.
func ExtractWAV(ctx context.Context, ffmpegPath, input string) (path string, cleanup func(), err error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	f, err := os.CreateTemp("", "ether-speech-*.wav")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	f.Close()
	cleanup = func() { _ = os.Remove(name) }
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-i", input,
		"-vn", "-ac", "1", "-ar", "16000",
		"-f", "wav", "-y", name,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		msg := string(out)
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", nil, fmt.Errorf("could not read the audio: %s", msg)
	}
	return name, cleanup, nil
}

func splitWAV(path string, span time.Duration) (chunks []wavChunk, cleanup func(), err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	data, rate, err := readPCM(f)
	if err != nil {
		return nil, nil, err
	}
	if rate <= 0 {
		rate = 16000
	}
	bytesPerSec := rate * 2 // mono s16le
	if bytesPerSec <= 0 {
		return nil, nil, fmt.Errorf("speech wav has no sample rate")
	}
	spanBytes := int(span.Seconds()) * bytesPerSec
	if spanBytes < bytesPerSec {
		spanBytes = bytesPerSec
	}
	var temps []string
	cleanup = func() {
		for _, p := range temps {
			_ = os.Remove(p)
		}
	}
	if len(data) <= spanBytes {
		return []wavChunk{{path: path}}, cleanup, nil
	}
	for off := 0; off < len(data); off += spanBytes {
		end := off + spanBytes
		if end > len(data) {
			end = len(data)
		}
		// Keep whole samples.
		end -= (end - off) % 2
		if end <= off {
			break
		}
		tmp, err := os.CreateTemp("", "ether-speech-part-*.wav")
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if err := writePCM(tmp, data[off:end], rate); err != nil {
			tmp.Close()
			cleanup()
			return nil, nil, err
		}
		tmp.Close()
		temps = append(temps, tmp.Name())
		chunks = append(chunks, wavChunk{path: tmp.Name(), offset: time.Duration(off/bytesPerSec) * time.Second})
	}
	if len(chunks) == 0 {
		cleanup()
		return nil, nil, fmt.Errorf("speech wav was empty")
	}
	return chunks, cleanup, nil
}

func readPCM(r io.Reader) (pcm []byte, rate int, err error) {
	raw, err := io.ReadAll(io.LimitReader(r, 512<<20))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("speech audio is not a wav file")
	}
	off := 12
	var data []byte
	for off+8 <= len(raw) {
		id := string(raw[off : off+4])
		n := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		off += 8
		if n < 0 || off+n > len(raw) {
			return nil, 0, fmt.Errorf("speech wav is truncated")
		}
		chunk := raw[off : off+n]
		off += n
		if n%2 == 1 {
			off++
		}
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, 0, fmt.Errorf("speech wav fmt is short")
			}
			format := binary.LittleEndian.Uint16(chunk[0:2])
			channels := int(binary.LittleEndian.Uint16(chunk[2:4]))
			rate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			bits := binary.LittleEndian.Uint16(chunk[14:16])
			if format != 1 || channels != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("speech wav must be 16-bit mono PCM")
			}
		case "data":
			data = chunk
		}
	}
	if data == nil {
		return nil, 0, fmt.Errorf("speech wav has no audio")
	}
	return data, rate, nil
}

func writePCM(w io.Writer, pcm []byte, rate int) error {
	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+len(pcm)))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], 1)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(hdr[32:34], 2)
	binary.LittleEndian.PutUint16(hdr[34:36], 16)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(len(pcm)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(pcm)
	return err
}
