package transcode

// Opt-in, serial measurements. CPU/RSS cover ffmpeg, while achieved fps also
// includes Go frame production/copying. Invoke go test itself at nice 10.
import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

var continuousMeasure = flag.Bool("continuous-measure", false, "run serial runner resource measurements")
var continuousSize = flag.String("continuous-size", "1920x1080", "raw working resolution, WxH")
var continuousEncoderMode = flag.String("continuous-encoder", "software", "software, videotoolbox, nvenc, qsv or vaapi")
var continuousMeasureDuration = flag.Duration("continuous-duration", 10*time.Second, "paced measurement duration")
var continuousWorkload = flag.String("continuous-workload", "all", "encoder, h264, hevc, or all (standard seven-row matrix)")

type continuousTimedRunner struct {
	Runner
	report string
}

func (r continuousTimedRunner) Start(c Command) (Process, error) {
	options := []string{"-l", "-o", r.report}
	if runtime.GOOS == "linux" {
		options = []string{"-v", "-o", r.report}
	}
	c.Args = append(append(options, c.Path), c.Args...)
	c.Path = "/usr/bin/time"
	return r.Runner.Start(c)
}
func continuousResources(b []byte, wall time.Duration) (float64, float64) {
	var user, system, rss float64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if runtime.GOOS == "darwin" {
			if len(f) >= 6 && f[1] == "real" {
				user, _ = strconv.ParseFloat(f[2], 64)
				system, _ = strconv.ParseFloat(f[4], 64)
			}
			if strings.Contains(line, "maximum resident set size") {
				rss, _ = strconv.ParseFloat(f[0], 64)
			}
		} else {
			_, v, _ := strings.Cut(line, ":")
			v = strings.TrimSpace(v)
			if strings.Contains(line, "User time (seconds)") {
				user, _ = strconv.ParseFloat(v, 64)
			}
			if strings.Contains(line, "System time (seconds)") {
				system, _ = strconv.ParseFloat(v, 64)
			}
			if strings.Contains(line, "Maximum resident set size") {
				rss, _ = strconv.ParseFloat(v, 64)
				rss *= 1024
			}
		}
	}
	return (user + system) / wall.Seconds(), rss / (1024 * 1024)
}
func TestContinuousMeasurements(t *testing.T) {
	if !*continuousMeasure && os.Getenv("ETHER_CONTINUOUS_MEASURE") == "" {
		t.Skip("opt-in resource measurement")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("resource sampler supports macOS and Linux")
	}
	if *continuousMeasureDuration < time.Second {
		t.Fatal("measurement duration must be at least one second")
	}
	e := newRealEnv(t, WithoutHWDetection())
	base := e.m.runner
	var width, height int
	if _, err := fmt.Sscanf(*continuousSize, "%dx%d", &width, &height); err != nil {
		t.Fatal(err)
	}
	type workload struct {
		name, codec, mode        string
		width, height, outHeight int
	}
	modes := []workload{{name: "encoder", mode: *continuousEncoderMode, width: width, height: height, outHeight: height}}
	switch *continuousWorkload {
	case "all":
		modes = []workload{{"software-720", "", "software", 1280, 720, 720}, {"software-1080", "", "software", 1920, 1080, 1080}, {"hardware-720", "", *continuousEncoderMode, 1280, 720, 720}, {"hardware-1080", "", *continuousEncoderMode, 1920, 1080, 1080}, {"extra-720-from-1080", "", "software", 1920, 1080, 720}, {"decode-h264", "h264", "", 1920, 1080, 1080}, {"decode-hevc", "hevc", "", 1920, 1080, 1080}}
		if *continuousEncoderMode == "software" {
			hw := "nvenc"
			if runtime.GOOS == "darwin" {
				hw = "videotoolbox"
			}
			modes[2].mode = hw
			modes[3].mode = hw
		}
	case "encoder":
	case "h264", "hevc":
		modes = []workload{{name: "decode-" + *continuousWorkload, codec: *continuousWorkload, width: width, height: height, outHeight: height}}
	default:
		t.Fatal("unknown workload")
	}
	t.Log("workload | encoder | CPU cores | peak RSS MiB | achieved fps")
	for _, w := range modes {
		t.Run(w.name, func(t *testing.T) {
			f := RawFormat{Width: w.width, Height: w.height, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
			if err := validateRaw(f); err != nil {
				t.Fatal(err)
			}
			report := filepath.Join(e.dir, w.name+".time")
			e.m.runner = base
			var input string
			if w.codec != "" {
				input = filepath.Join(e.dir, w.name+".mp4")
				size, encoder := "1920x1080", "libx264"
				if w.codec == "hevc" {
					size = "3840x2160"
					encoder = "libx265"
				}
				args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=30", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", fmt.Sprint(continuousMeasureDuration.Seconds() + 2), "-c:v", encoder, "-preset", "ultrafast", "-threads", "2", "-c:a", "aac"}
				if w.codec == "hevc" {
					args = append(args, "-x265-params", "pools=2:frame-threads=2:log-level=error")
				}
				args = append(args, input)
				if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, args...).CombinedOutput(); err != nil {
					t.Fatalf("fixture: %v %s", err, out)
				}
			}
			e.m.runner = continuousTimedRunner{Runner: base, report: report}
			e.m.hw = core.HWAccel{}
			if w.mode != "" && w.mode != "software" {
				switch w.mode {
				case "nvenc", "videotoolbox", "qsv", "vaapi":
				default:
					t.Fatal("unsupported encoder mode")
				}
				e.m.hw = core.HWAccel{Name: w.mode, Encoders: []string{"h264_" + w.mode}, Device: "/dev/dri/renderD128"}
			}
			ctx, cancel := context.WithTimeout(t.Context(), *continuousMeasureDuration+10*time.Second)
			defer cancel()
			var proc *continuousProcess
			var encode ContinuousEncoder
			var decode Decoder
			var err error
			if w.codec == "" {
				encode, err = e.m.StartEncoder(ctx, EncoderRequest{Source: f, Decision: core.Decision{VideoCodec: "h264", AudioCodec: "aac", MaxHeight: w.outHeight, BitrateKbps: 3000}})
				if err == nil {
					defer encode.Close()
					proc = encode.(*continuousEncoder).continuousProcess
				}
			} else {
				decode, err = e.m.StartDecoder(ctx, DecoderRequest{InputURL: input, Format: f})
				if err == nil {
					defer decode.Close()
					proc = decode.(*continuousDecoder).continuousProcess
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			frame := RawFrame{Video: make([]byte, f.Width*f.Height*3/2), PCM: make([]byte, 6400)}
			for i := f.Width * f.Height; i < len(frame.Video); i++ {
				frame.Video[i] = 128
			}
			start := time.Now()
			count := int(continuousMeasureDuration.Seconds() * 30)
			finished := 0
			for n := 0; n < count; n++ {
				if encode != nil {
					for y := 0; y < f.Height; y++ {
						for x := 0; x < f.Width; x++ {
							frame.Video[y*f.Width+x] = byte(16 + ((x+n*3)/16+y/16)%200)
						}
					}
					err = encode.Write(ctx, frame)
				} else {
					_, err = decode.Next(ctx)
				}
				if err != nil {
					break
				}
				finished++
				time.Sleep(time.Until(start.Add(time.Duration(n+1) * time.Second / 30)))
			}
			for _, s := range proc.sockets {
				s.close()
			}
			select {
			case <-proc.done:
			case <-time.After(2 * time.Second):
			}
			if encode != nil {
				encode.Close()
			} else {
				decode.Close()
			}
			wall := time.Since(start)
			b, readErr := os.ReadFile(report)
			if readErr != nil {
				t.Fatal(readErr)
			}
			cpu, rss := continuousResources(b, wall)
			t.Logf("%s | %s | %.3f | %.1f | %.2f", w.name, w.mode, cpu, rss, float64(finished)/wall.Seconds())
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
