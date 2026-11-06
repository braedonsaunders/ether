//go:build splicelab

// splicelab prints the real HLS command arguments for synthetic offline media.
package main

import (
	"encoding/json"
	"flag"
	"github.com/braedonsaunders/ether/internal/transcode"
	"os"
)

func main() {
	input := flag.String("input", "", "fixture")
	probe := flag.String("probe", "", "ffprobe JSON")
	dir := flag.String("dir", "", "output directory")
	container := flag.String("container", "mpegts", "mpegts or fmp4")
	first := flag.Int("first", 0, "first segment")
	copyVideo := flag.Bool("copy-video", false, "remux source keyframe grid")
	hardware := flag.String("hardware", "", "JSON shipping HWAccel, HWAccels, Filters and negotiated H264Level/H264FrameRate inventory (no GPU probe)")
	flag.Parse()
	b, e := os.ReadFile(*probe)
	if e != nil {
		panic(e)
	}
	info, e := transcode.ParseProbe(b)
	if e != nil {
		panic(e)
	}
	var hw transcode.SpliceLabHardware
	if *hardware != "" {
		b, e := os.ReadFile(*hardware)
		if e != nil {
			panic(e)
		}
		if e = json.Unmarshal(b, &hw); e != nil {
			panic(e)
		}
	}
	json.NewEncoder(os.Stdout).Encode(transcode.SpliceLabHardwareArgs(*input, *dir, *info, *container, *first, *copyVideo, hw))
}
