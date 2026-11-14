package transcode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Exercise the cross-generation invariant: one software retry, no sequence
// holes, and the same frame-exact cuts even inside the opening ladder.
func TestContinuousHardwareRecovery(t *testing.T) {
	for _, mode := range []string{AccelQSV, AccelVAAPI, AccelNVENC, AccelVideoToolbox} {
		t.Run(mode, func(t *testing.T) {
			runner := &continuousHardwareRunner{fakeRunner: newFakeRunner()}
			m := newTestManager(t, runner.fakeRunner, config.Transcode{}, WithoutHWDetection(), WithRunner(runner))
			m.hw = core.HWAccel{Name: mode, Device: "/dev/dri/renderD129", Encoders: []string{"h264_" + mode}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e := &continuousEncoder{m: m, continuousProcess: &continuousProcess{ctx: ctx, cancel: cancel}, dir: t.TempDir(), stream: newContinuousFanout(), req: EncoderRequest{Source: RawFormat{Width: 320, Height: 180, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}, Decision: core.Decision{VideoCodec: "h264", AudioCodec: "aac", BitrateKbps: 1500}, SegmentDuration: 2 * time.Second, LiveSource: true}}
			e.index = newContinuousSegmentIndex(e)
			var err error
			e.store, err = newContinuousStore(e.dir, 4, e.index.kick)
			if err != nil {
				t.Fatal(err)
			}
			defer e.store.close()
			child, err := e.startOrdinaryChild(0, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { child.forceClose() }()
			args := strings.Join(runner.args[0], " ")
			if !strings.Contains(args, "-c:v h264_"+mode) {
				t.Fatal(args)
			}
			if mode == AccelQSV && (!strings.Contains(args, "-qsv_device /dev/dri/renderD129") || !strings.Contains(args, "-forced_idr 1")) {
				t.Fatal(args)
			}
			if mode == AccelVAAPI && (!strings.Contains(args, "-vaapi_device /dev/dri/renderD129") || !strings.Contains(args, "hwupload")) {
				t.Fatal(args)
			}
			// Segment zero is published, but the hardware dies during segment one.
			e.index.next = 1
			child.forceClose()
			if err := e.recoverFromEncoderFailure(&child, 20); err != nil {
				t.Fatal(err)
			}
			args = strings.Join(runner.args[1], " ")
			for _, want := range []string{"-c:v libx264", "start_number=1", "hls_init_time=0.500000", "-output_ts_offset 0.500000", "-force_key_frames expr:if(lt(n,105),eq(n,0)+eq(n,15)+eq(n,45)+eq(n,75),gte(n,(n_forced+1-3)*60-15))"} {
				if !strings.Contains(args, want) {
					t.Fatalf("missing %s in %s", want, args)
				}
			}
			if !e.forceCPU || child.hardware || child.startSeq != 1 || child.startTick != 20 || e.wantBoundarySwap(child) {
				t.Fatal("software fallback did not latch")
			}
			child.forceClose()
			if err := e.recoverFromEncoderFailure(&child, 21); err == nil || len(runner.args) != 2 {
				t.Fatal("software failure must terminate rather than loop")
			}
		})
	}
	t.Run("launch failure", func(t *testing.T) {
		runner := &continuousHardwareRunner{fakeRunner: newFakeRunner(), failLaunch: true}
		m := newTestManager(t, runner.fakeRunner, config.Transcode{}, WithoutHWDetection(), WithRunner(runner))
		m.hw = core.HWAccel{Name: AccelQSV, Encoders: []string{"h264_qsv"}}
		enc, err := m.StartEncoder(t.Context(), EncoderRequest{Source: RawFormat{Width: 320, Height: 180, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}})
		if err != nil {
			t.Fatal(err)
		}
		enc.Close()
		if len(runner.args) != 2 || !strings.Contains(strings.Join(runner.args[1], " "), "-c:v libx264") {
			t.Fatal("launch did not fall back once")
		}
	})
}

type continuousHardwareRunner struct {
	*fakeRunner
	args       [][]string
	failLaunch bool
}

func (r *continuousHardwareRunner) Start(cmd Command) (Process, error) {
	r.args = append(r.args, append([]string(nil), cmd.Args...))
	if r.failLaunch && len(r.args) == 1 {
		return nil, errors.New("hardware launch failed")
	}
	p := &fakeProc{done: make(chan struct{}), kill: make(chan struct{})}
	go func() { <-p.kill; close(p.done) }()
	return p, nil
}
