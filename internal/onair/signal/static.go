// Package signal produces live, deterministic signal screens without clip loops
// or per-tick allocations. The clock is 30 ticks per second, audio is 48 kHz.
package signal

import "math"

func hash(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}
func noise(seed, index int64) float64 {
	return float64(int32(hash(uint64(seed)^uint64(index)*0x9e3779b97f4a7c15)>>32)) / 2147483648
}

// walk is a seeded random walk in 32-step bridges between unique epoch anchors.
// The bridge correction makes epoch boundaries continuous without replaying any
// historical state. Evaluation has constant cost, including after days on air.
func walk(seed, tick int64) float64 {
	epoch := tick / 960
	position := float64(tick%960) / 30
	step := int(position)
	fraction := position - float64(step)
	start, end := noise(seed, epoch), noise(seed, epoch+1)
	sum, total := 0.0, 0.0
	for i := 0; i < 32; i++ {
		v := noise(seed^int64(hash(uint64(epoch))), int64(i))
		total += v
		if i < step {
			sum += v
		} else if i == step {
			sum += fraction * v
		}
	}
	return start + (end-start)*position/32 + (sum-total*position/32)*.08
}

// StaticParameters controls live snow. Start from DefaultStaticParameters;
// zero values intentionally allow silent audio, frozen grain and no effects.
// The type is internal to Ether and is not an owner configuration contract.
type StaticParameters struct {
	GrainSize                                        int
	Detail, Gain, RefreshHz, Persistence, Tint       float64
	BarStrength, BarSpeed, TearAmount, TearFrequency float64
	HissLevel, HumLevel                              float64
}

// DefaultStaticParameters uses owner-selected detail 1.75; every other value
// preserves the approved W18 picture and sound.
func DefaultStaticParameters() StaticParameters {
	return StaticParameters{GrainSize: 6, Detail: 1.75, Gain: 1, RefreshHz: 15,
		BarStrength: 1, BarSpeed: 1, TearAmount: 1, TearFrequency: 1, HissLevel: 1, HumLevel: 1}
}

// StaticVideo fills tightly packed I420 with crisp destination-pixel grains
// at the owner-selected detail, changing at 15 Hz. The
// rolling bar and tears follow the full 30 Hz clock and never use a clip phase.
// Composite an optional pre-rendered ID with ApplyOverlay after this call.
func StaticVideo(dst []byte, w, h int, seed, tick int64) {
	StaticVideoWithParameters(dst, w, h, seed, tick, DefaultStaticParameters())
}

// StaticVideoWithParameters evaluates a stateless frame with explicit controls.
func StaticVideoWithParameters(dst []byte, w, h int, seed, tick int64, p StaticParameters) {
	if w < 2 || h < 2 || w%2 != 0 || h%2 != 0 || len(dst) < w*h*3/2 {
		panic("signal: invalid I420 buffer")
	}
	yplane := dst[:w*h]
	for i := w * h; i < w*h*3/2; i++ {
		dst[i] = 128
	}
	walkTick := tick
	if p.BarSpeed != 1 {
		walkTick = int64(float64(tick) * p.BarSpeed)
	}
	t := float64(tick) / 30 * p.BarSpeed
	bar := int((.5 + .24*math.Sin(t*.173) + .13*math.Sin(t*.071*math.Sqrt2) + .08*walk(seed, walkTick)) * float64(h))
	barHeight := max(12, h/11)
	update := tick / 2
	if p.RefreshHz != 15 {
		update = int64(math.Floor(float64(tick) * max(0, min(30, p.RefreshHz)) / 30))
	}
	grain := max(1, min(128, int(float64(max(1, p.GrainSize))/max(.25, min(4, p.Detail)))))
	gain := max(0, min(3, p.Gain))
	minLuma := max(16, min(235, 115+int(-63*gain)))
	maxLuma := max(16, min(235, 115+int(64*gain)))
	for gy, y := 0, 0; y < h; gy, y = gy+1, y+grain {
		row := yplane[y*w : (y+1)*w]
		for gx, x := 0, 0; x < w; gx, x = gx+1, x+grain {
			v := byte(52 + int(hash(uint64(seed)^uint64(update)*0xd1342543de82ef95^uint64(gy)*0x9e3779b97f4a7c15^uint64(gx))>>57)*1)
			if p.Persistence > 0 {
				prev := byte(52 + int(hash(uint64(seed)^uint64(update-1)*0xd1342543de82ef95^uint64(gy)*0x9e3779b97f4a7c15^uint64(gx))>>57))
				persistence := max(0, min(1, p.Persistence))
				v = byte(float64(v)*(1-persistence) + float64(prev)*persistence)
			}
			if p.Gain != 1 {
				v = byte(max(16, min(235, 115+int((float64(v)-115)*max(0, min(3, p.Gain))))))
			}
			for xx, end := x, min(w, x+grain); xx < end; xx++ {
				row[xx] = v
			}
		}
		for dy := 1; dy < min(grain, h-y); dy++ {
			copy(yplane[(y+dy)*w:(y+dy+1)*w], row)
		}
		for dy := 0; dy < min(grain, h-y); dy++ {
			target := yplane[(y+dy)*w : (y+dy+1)*w]
			distance := abs(y + dy - bar)
			offset := 0
			if distance < barHeight {
				offset = -int(float64((barHeight-distance)*28/barHeight) * p.BarStrength)
			}
			if (y+dy)%3 == 0 {
				offset -= 3
			}
			// Grain values are already legal luma. Most rows need no
			// modulation; other rows can use a tight add loop when the
			// whole source range stays legal after this row's offset.
			if offset == 0 {
				continue
			}
			if minLuma+offset >= 16 && maxLuma+offset <= 235 {
				for x, v := range target {
					target[x] = byte(int(v) + offset)
				}
			} else {
				for x, v := range target {
					target[x] = byte(max(16, min(235, int(v)+offset)))
				}
			}
		}
	}
	if p.Tint != 0 {
		for i := w * h; i < len(dst); i++ {
			dst[i] = byte(128 + int(p.Tint*24*noise(seed, int64(i/(max(1, grain/2)))+update)))
		}
	}
	// Rare horizontal tracking tears; each event uses its own seed and position.
	event := tick / 3
	bits := hash(uint64(seed) ^ uint64(event)*0xa0761d6478bd642f)
	if p.TearFrequency > 0 && bits%uint64(max(1, int(29/p.TearFrequency))) == 0 {
		line := int((bits >> 16) % uint64(h))
		for y := line; y < min(h, line+max(1, h/270)); y++ {
			row := yplane[y*w : (y+1)*w]
			for x := range row {
				row[x] = byte(max(16, min(235, int(row[x])+int(float64(145+int(hash(bits^uint64(x/12))%60)-int(row[x]))*p.TearAmount))))
			}
		}
	}
}
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Overlay is a pre-rendered luma/alpha ID, positioned in the full working frame.
// Construct it off the playout path; applying it never allocates.
type Overlay struct {
	X, Y, Width, Height int
	Luma, Alpha         []byte
}

func ApplyOverlay(dst []byte, w, h int, o Overlay) {
	if len(dst) < w*h*3/2 || len(o.Luma) < o.Width*o.Height || len(o.Alpha) < o.Width*o.Height {
		return
	}
	for y := 0; y < o.Height; y++ {
		yy := o.Y + y
		if yy < 0 || yy >= h {
			continue
		}
		for x := 0; x < o.Width; x++ {
			xx := o.X + x
			if xx < 0 || xx >= w {
				continue
			}
			i := y*o.Width + x
			a := int(o.Alpha[i])
			j := yy*w + xx
			dst[j] = byte((int(dst[j])*(255-a) + int(o.Luma[i])*a + 127) / 255)
		}
	}
}

// A symmetric 11-tap low-pass FIR limits the white noise to about 8 kHz. The
// absolute sample index supplies its history, so adjacent ticks have no seam.
func hiss(seed, n int64) float64 {
	return -.0068*(noise(seed, n-5)+noise(seed, n+5)) - .0131*(noise(seed, n-4)+noise(seed, n+4)) + .0508*(noise(seed, n-2)+noise(seed, n+2)) + .2632*(noise(seed, n-1)+noise(seed, n+1)) + .4118*noise(seed, n)
}

// StaticAudio writes interleaved stereo s16 PCM: approximately -30 dBFS RMS
// band-limited hiss and -42 dBFS RMS 60 Hz hum, with slow non-periodic wander.
// tick is the absolute 30 Hz channel tick, not a looping clip-relative index.
func StaticAudio(dst []int16, tick int64, samplesThisTick int, seed int64) {
	StaticAudioWithParameters(dst, tick, samplesThisTick, seed, DefaultStaticParameters())
}

// StaticAudioWithParameters scales hiss and hum independently.
func StaticAudioWithParameters(dst []int16, tick int64, samplesThisTick int, seed int64, p StaticParameters) {
	if samplesThisTick < 0 || len(dst) < samplesThisTick*2 {
		panic("signal: invalid PCM buffer")
	}
	for i := 0; i < samplesThisTick; i++ {
		n := tick*1600 + int64(i)
		t := float64(n) / 48000
		phase := 2*math.Pi*60*t + .25*math.Sin(t*.037*2*math.Pi+float64(seed%1000)) + .11*math.Sin(t*.017*2*math.Pi*math.Sqrt2)
		v := hiss(seed, n)*(.0988*p.HissLevel) + math.Sin(phase)*(.011233*p.HumLevel)
		if n < 480 {
			v *= float64(n) / 480
		}
		q := int16(math.Round(max(-1, min(1, v)) * 32767))
		dst[2*i] = q
		dst[2*i+1] = q
	}
}
