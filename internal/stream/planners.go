package stream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/par2"
)

// planPlain maps a posted video file directly.
func (b *builder) planPlain(ctx context.Context, pf *postedFile) (*plan, error) {
	seg, err := b.e.segment(ctx, b.rel, pf, 0, b.prio, b.fo)
	if err != nil {
		return nil, err
	}
	if seg.begin != 0 {
		// A numbered first article is not necessarily the file's beginning:
		// damaged/reindexed NZBs can point it into the middle of the upload.
		// Reject before publishing a playable plan or caching healthy status.
		return nil, fmt.Errorf("stream: first article of %s begins at %d, not zero: %w", pf.Name(), seg.begin, ErrCorrupt)
	}
	if !pf.SizeKnown() {
		// No yEnc file size: the last part tells where the file ends.
		last := pf.numSlots() - 1
		ls, err := b.e.segment(ctx, b.rel, pf, last, b.prio, b.fo)
		if err != nil {
			return nil, err
		}
		pf.learn(last, ls.begin, ls.end(), ls.end(), 0)
	}
	name := pf.Name()
	if (pf.file.Obfuscated || name == "") && seg.name != "" && !nzb.IsObfuscated(seg.name) {
		name = path.Base(seg.name)
	}
	size := pf.Size()
	if size <= 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrNoPlayableFile, name)
	}
	p := newPlan(b.rel, kindPlain, name, size, []*postedFile{pf}, []volInfo{{size: size, dataSize: size}}, []bool{true})
	return p, p.init()
}

// planSplit maps a plain split (.001, .002 ...) of a video. Split sets are
// rare and carry no internal totals, so every volume's size is learned.
func (b *builder) planSplit(ctx context.Context, vols []*postedFile) (*plan, error) {
	if err := b.heads(ctx, vols); err != nil {
		return nil, err
	}
	head, err := b.sniff(ctx, vols[0])
	if err != nil {
		return nil, err
	}
	if f := archive.Detect(head); f != archive.FormatUnknown {
		return nil, fmt.Errorf("stream: %s archive split into plain volumes: %w", f, ErrUnsupported)
	}
	ext := vols[0].file.Info.Ext
	if !nzb.IsVideoExt(ext) && !looksLikeVideo(head) {
		return nil, fmt.Errorf("%w: split set %s is not a video", ErrNoPlayableFile, vols[0].Name())
	}
	name := vols[0].Name()
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	info := make([]volInfo, len(vols))
	verified := make([]bool, len(vols))
	var size int64
	for k, pf := range vols {
		s := pf.Size()
		info[k] = volInfo{size: s, dataSize: s}
		verified[k] = true
		size += s
	}
	p := newPlan(b.rel, kindSplit, name, size, vols, info, verified)
	return p, p.init()
}

// planRAR maps the main video inside a store-mode RAR set.
func (b *builder) planRAR(ctx context.Context, vols []*postedFile) (*plan, error) {
	if err := b.checkCachedRAR(ctx, candidate{kind: nzb.KindRAR, files: vols}); err != nil {
		return nil, err
	}
	n := len(vols)
	var v0 *archive.RARVolume
	if n == 1 || vols[0].SizeKnown() {
		if err := b.heads(ctx, vols[:1]); err != nil {
			return nil, err
		}
		var err error
		v0, err = archive.ReadRARVolume(b.reader(ctx, vols[0]), vols[0].Size(), b.rarOptions(ctx, 0))
		if err != nil {
			return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
		}
		if err := b.checkRARVolume(v0); err != nil {
			return nil, err
		}
		if n == 1 {
			return b.rarFromVolumes(ctx, vols, []*archive.RARVolume{v0})
		}
	}
	probe := []*postedFile{vols[0]}
	if n > 1 {
		probe = append(probe, vols[1])
	}
	if n > 2 {
		probe = append(probe, vols[n-1])
	}
	if err := b.parallel(ctx, len(probe), func(ctx context.Context, i int) error {
		seg, err := b.e.segment(ctx, b.rel, probe[i], 0, b.prio, b.fo)
		if err != nil {
			return err
		}
		if seg != nil && seg.begin == 0 {
			v, err := b.readCachedRARVolume(ctx, probe[i], seg.data)
			if err != nil {
				if errors.Is(err, archive.ErrEncrypted) {
					return fmt.Errorf("stream: rar %s: %w", probe[i].Name(), err)
				}
				return nil
			}
			return b.checkRARVolume(v)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if v0 == nil {
		var err error
		v0, err = archive.ReadRARVolume(b.reader(ctx, vols[0]), vols[0].Size(), b.rarOptions(ctx, 0))
		if err != nil {
			return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
		}
		if err := b.checkRARVolume(v0); err != nil {
			return nil, err
		}
	}
	if n == 1 {
		return b.rarFromVolumes(ctx, vols, []*archive.RARVolume{v0})
	}
	// Estimated decoded size of the whole set (middle volumes as volume 2).
	setBytes := vols[0].Size() + vols[n-1].Size()
	if n > 2 {
		setBytes += int64(n-2) * vols[1].Size()
	}
	main := pickRARFile(v0.Files, v0.Format)
	if main == nil || main.Encrypted || !main.SplitAfter || main.SplitBefore || main.UnpackedSize <= 0 || main.UnpackedSize*2 < setBytes {
		for _, k := range []int{1, n - 1} {
			if k <= 0 || k >= n {
				continue
			}
			v, err := archive.ReadRARVolume(b.reader(ctx, vols[k]), vols[k].Size(), b.rarOptions(ctx, 0))
			if err != nil {
				return nil, fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
			}
			if err := b.checkRARVolume(v); err != nil {
				return nil, err
			}
		}
		// The video is not a single member spanning the set from volume 1
		// (or volume 1 has no video): read every volume's headers.
		return b.rarFullScan(ctx, vols)
	}

	read := func(k int) (*archive.RARFile, error) {
		v, err := archive.ReadRARVolume(b.reader(ctx, vols[k]), vols[k].Size(), b.rarOptions(ctx, 1))
		if err != nil {
			return nil, fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
		}
		if err := b.checkRARVolume(v); err != nil {
			return nil, err
		}
		if len(v.Files) == 0 || v.Files[0].Name != main.Name || !v.Files[0].SplitBefore {
			return nil, nil
		}
		return &v.Files[0], nil
	}
	f1, err := read(1)
	if err != nil {
		return nil, err
	}
	fl := f1
	if n > 2 {
		if fl, err = read(n - 1); err != nil {
			return nil, err
		}
	}
	if f1 == nil || fl == nil || fl.SplitAfter {
		return b.rarFullScan(ctx, vols)
	}

	info := make([]volInfo, n)
	verified := make([]bool, n)
	info[0] = volInfo{size: vols[0].Size(), dataOff: main.DataOffset, dataSize: main.PackedSize}
	info[1] = volInfo{size: vols[1].Size(), dataOff: f1.DataOffset, dataSize: f1.PackedSize}
	info[n-1] = volInfo{size: vols[n-1].Size(), dataOff: fl.DataOffset, dataSize: fl.PackedSize}
	verified[0], verified[1], verified[n-1] = true, true, true
	rar5 := v0.Format == archive.FormatRAR5
	for k := 2; k < n-1; k++ {
		// Middle volumes repeat volume 2's layout. RAR5 headers carry the
		// volume number as a vint, which grows by a byte at 128 and 16384.
		var delta int64
		if rar5 {
			delta = int64(vintLen(uint64(k)) - vintLen(1))
		}
		info[k] = volInfo{size: info[1].size, dataOff: info[1].dataOff + delta, dataSize: info[1].dataSize - delta}
	}
	p := newPlan(b.rel, kindRAR, path.Base(main.Name), main.UnpackedSize, vols, info, verified)
	p.member, p.rar5 = main.Name, rar5
	if err := p.init(); err != nil {
		// The uniform model does not add up: learn every volume.
		b.e.log.Debug("rar uniform layout rejected; reading all volume headers", "name", main.Name, "err", err)
		if err := p.rebuildExact(ctx, verifier{e: b.e, prio: b.prio, fo: b.fo}); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// rarFullScan reads the headers of every volume and composes members.
func (b *builder) rarFullScan(ctx context.Context, vols []*postedFile) (*plan, error) {
	if err := b.heads(ctx, vols); err != nil {
		return nil, err
	}
	parsed := make([]*archive.RARVolume, len(vols))
	err := b.parallel(ctx, len(vols), func(ctx context.Context, k int) error {
		v, err := archive.ReadRARVolume(b.reader(ctx, vols[k]), vols[k].Size(), b.rarOptions(ctx, 0))
		if err != nil {
			return fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
		}
		if err := b.checkRARVolume(v); err != nil {
			return err
		}
		parsed[k] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return b.rarFromVolumes(ctx, vols, parsed)
}

func (b *builder) rarFromVolumes(ctx context.Context, vols []*postedFile, parsed []*archive.RARVolume) (*plan, error) {
	members, err := archive.ComposeRAR(parsed)
	if err != nil {
		return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
	}
	m := chooseMember(members)
	if m == nil {
		return nil, fmt.Errorf("%w: rar %s holds no video (%d files)", ErrNoPlayableFile, vols[0].Name(), len(members))
	}
	if m.Encrypted && b.rel.doc.Password() == "" {
		return nil, archive.ErrPasswordRequired
	}
	if err := m.Streamable(); err != nil {
		return nil, fmt.Errorf("stream: rar %s: %s: %w", vols[0].Name(), m.Name, err)
	}
	info := make([]volInfo, len(vols))
	verified := make([]bool, len(vols))
	for k := range vols {
		info[k] = volInfo{size: vols[k].Size()}
		verified[k] = true
	}
	if m.Encryption != nil {
		return b.encryptedPlan(ctx, kindRAR, m, vols, info)
	}
	for _, x := range m.Extents {
		if x.Volume < 0 || x.Volume >= len(vols) {
			return nil, fmt.Errorf("stream: rar extent in volume %d of %d: %w", x.Volume, len(vols), ErrCorrupt)
		}
		if info[x.Volume].dataSize != 0 {
			return nil, fmt.Errorf("stream: rar member %s has two extents in volume %d: %w", m.Name, x.Volume, ErrUnsupported)
		}
		info[x.Volume].dataOff, info[x.Volume].dataSize = x.Offset, x.Size
	}
	p := newPlan(b.rel, kindRAR, path.Base(m.Name), m.Size, vols, info, verified)
	p.member = m.Name
	return p, p.init()
}

// plan7z maps the main video inside a copy-method 7z set.
func (b *builder) plan7z(ctx context.Context, vols []*postedFile) (*plan, error) {
	n := len(vols)
	// Encrypted headers and payloads cannot use unverified volume offsets.
	if b.rel.doc.Password() != "" {
		if err := b.heads(ctx, vols); err != nil {
			return nil, err
		}
	}
	// The signature header is at the start of volume 1 and the archive
	// header at the end of the last volume: fetch both ends at once.
	last := vols[n-1]
	err := b.parallel(ctx, 2, func(ctx context.Context, i int) error {
		if i == 0 {
			_, err := b.e.segment(ctx, b.rel, vols[0], 0, b.prio, b.fo)
			return err
		}
		_, err := b.e.segment(ctx, b.rel, last, last.numSlots()-1, b.prio, b.fo)
		return err
	})
	if err != nil {
		return nil, err
	}
	head, err := b.sniff(ctx, vols[0])
	if err != nil {
		return nil, err
	}
	if archive.Detect(head) != archive.Format7z || len(head) < 32 {
		return nil, fmt.Errorf("stream: %s is not a 7z archive: %w", vols[0].Name(), ErrCorrupt)
	}
	want := 32 + int64(binary.LittleEndian.Uint64(head[12:20])) + int64(binary.LittleEndian.Uint64(head[20:28]))

	info := make([]volInfo, n)
	verified := make([]bool, n)
	sizes := func() int64 {
		var t int64
		for k := range vols {
			if k == 0 || k == n-1 || verified[k] || vols[k].SizeKnown() {
				info[k] = volInfo{size: vols[k].Size()}
				verified[k] = true
			} else {
				info[k] = volInfo{size: vols[0].Size()}
			}
			t += info[k].size
		}
		return t
	}
	total := sizes()
	if total != want {
		b.e.log.Debug("7z uniform volume sizes rejected; learning every volume", "name", vols[0].Name(), "total", total, "want", want)
		if err := b.heads(ctx, vols); err != nil {
			return nil, err
		}
		for k := range verified {
			verified[k] = true
		}
		if total = sizes(); total != want {
			return nil, fmt.Errorf("stream: 7z volumes total %d bytes, header says %d: %w", total, want, ErrCorrupt)
		}
	}
	for k := range vols {
		vols[k].setAssumed(info[k].size)
	}
	av := make([]archive.Volume, n)
	for k, pf := range vols {
		av[k] = archive.Volume{R: b.reader(ctx, pf), Size: info[k].size}
	}
	members, err := archive.List7zWithPassword(ctx, archive.NewMultiReader(av), total, b.rel.doc.Password())
	if err != nil {
		return nil, fmt.Errorf("stream: 7z %s: %w", vols[0].Name(), err)
	}
	m := chooseMember(members)
	if m == nil {
		return nil, fmt.Errorf("%w: 7z %s holds no video (%d files)", ErrNoPlayableFile, vols[0].Name(), len(members))
	}
	if err := m.Streamable(); err != nil {
		return nil, fmt.Errorf("stream: 7z %s: %s: %w", vols[0].Name(), m.Name, err)
	}
	if m.Encryption != nil {
		return b.encryptedPlan(ctx, kind7z, m, vols, info)
	}
	p := newPlan(b.rel, kind7z, path.Base(m.Name), m.Size, vols, info, verified)
	p.concatExt = m.Extents
	return p, p.init()
}

// pickRARFile chooses the likely video among the file headers of a volume.
func pickRARFile(files []archive.RARFile, format ...archive.Format) *archive.RARFile {
	var fmt archive.Format
	if len(format) > 0 {
		fmt = format[0]
	}
	members := make([]archive.Member, len(files))
	for i, f := range files {
		members[i] = rarMember(fmt, &f)
	}
	m := chooseMember(members)
	if m == nil {
		return nil
	}
	for i := range files {
		if files[i].Name == m.Name {
			return &files[i]
		}
	}
	return nil
}

func rarMember(format archive.Format, f *archive.RARFile) archive.Member {
	return archive.Member{
		Name: f.Name, Size: f.UnpackedSize, Stored: f.Stored,
		Encrypted: f.Encrypted, Encryption: f.Encryption,
		Solid: f.Solid, Dir: f.Dir, Method: f.MethodName(format),
	}
}

// checkRARVolume fails fast when a parsed RAR volume's main video is
// compressed, solid, or encrypted without a usable password.
func (b *builder) checkRARVolume(v *archive.RARVolume) error {
	if v == nil {
		return nil
	}
	main := pickRARFile(v.Files, v.Format)
	if main == nil {
		return nil
	}
	if main.Encrypted && b.rel.doc.Password() == "" {
		return archive.ErrPasswordRequired
	}
	if m := rarMember(v.Format, main); m.Streamable() != nil {
		return fmt.Errorf("stream: rar %s: %w", main.Name, m.Streamable())
	}
	return nil
}

// readCachedRARVolume parses the RAR headers present in a volume's already
// fetched first article without issuing any network reads.
func (b *builder) readCachedRARVolume(ctx context.Context, pf *postedFile, head []byte) (*archive.RARVolume, error) {
	volSize := max(pf.Size(), int64(len(head)))
	r := bytes.NewReader(head)
	var last *archive.RARVolume
	for maxFiles := 1; maxFiles <= 8; maxFiles++ {
		v, err := archive.ReadRARVolume(r, volSize, b.rarOptions(ctx, maxFiles))
		if err != nil {
			if last != nil {
				return last, nil
			}
			return nil, err
		}
		last = v
		if pickRARFile(v.Files, v.Format) != nil || len(v.Files) < maxFiles {
			return v, nil
		}
		f := v.Files[len(v.Files)-1]
		if f.SplitAfter || f.DataOffset+f.PackedSize >= int64(len(head)) {
			return v, nil
		}
	}
	return last, nil
}

// checkCachedRAR inspects any already-cached volume heads of a RAR candidate
// and fails fast when they show the main video cannot be streamed.
func (b *builder) checkCachedRAR(ctx context.Context, c candidate) error {
	if c.kind != nzb.KindRAR {
		return nil
	}
	for _, pf := range c.files {
		seg, ok := b.e.hot.get(segKey{b.rel.digest, pf.segBase})
		if !ok || seg == nil || seg.begin != 0 {
			continue
		}
		v, err := b.readCachedRARVolume(ctx, pf, seg.data)
		if err != nil {
			if errors.Is(err, archive.ErrEncrypted) {
				return fmt.Errorf("stream: rar %s: %w", pf.Name(), err)
			}
			continue
		}
		if err := b.checkRARVolume(v); err != nil {
			return err
		}
	}
	return nil
}

// chooseMember picks the main video of an archive: the largest video that
// is not a sample or extra; failing that (obfuscated inner names), the
// largest file without a known non-video type if it dominates the archive.
func chooseMember(members []archive.Member) *archive.Member {
	var best, anon *archive.Member
	var total int64
	for i := range members {
		m := &members[i]
		if m.Dir {
			continue
		}
		total += max(m.Size, 0)
		info := nzb.Classify(m.Name)
		switch {
		case info.Kind == nzb.KindVideo && !info.Sample && !info.Extra:
			if best == nil || m.Size > best.Size {
				best = m
			}
		case info.Kind == nzb.KindUnknown:
			if anon == nil || m.Size > anon.Size {
				anon = m
			}
		}
	}
	if best != nil {
		return best
	}
	if anon != nil && (anon.Size*2 >= total || (anon.Size < 0 && total == 0)) {
		return anon
	}
	return nil
}

// shouldProbe: unidentifiable files outweigh every recognised candidate.
func (b *builder) shouldProbe(cands []candidate) bool {
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()
	var unknown int64
	for _, pf := range b.rel.files {
		if pf.file.Info.Kind == nzb.KindUnknown {
			unknown += pf.file.Bytes()
		}
	}
	var best int64
	if len(cands) > 0 {
		best = cands[0].bytes
	}
	return unknown > best
}

// maxProbeRounds bounds identification past MaxProbes: when PAR2 or the
// archive's own volume numbers prove the leading set has volumes the first
// probes did not find, at most this many times MaxProbes heads are read.
const maxProbeRounds = 4

// errUnidentifiedVolumes reports a leading archive set with volumes that
// files left unprobed (the probe budget) or unreadable (a transient fetch
// error) may hold. It is neither a rejection nor a missing article: nothing
// proves those volumes absent.
var errUnidentifiedVolumes = errors.New("stream: archive volumes not identified")

// unknownFiles lists the unidentified files, largest first, at most
// MaxProbes of them.
func (b *builder) unknownFiles() []*postedFile {
	return b.unknownFilesExcept(nil, b.e.opts.MaxProbes)
}

// unknownFilesExcept lists unidentified files not in skip, largest first,
// at most limit of them.
func (b *builder) unknownFilesExcept(skip map[*postedFile]bool, limit int) []*postedFile {
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()
	var unknown []*postedFile
	for _, pf := range b.rel.files {
		if pf.file.Info.Kind == nzb.KindUnknown && !skip[pf] {
			unknown = append(unknown, pf)
		}
	}
	sort.SliceStable(unknown, func(i, j int) bool { return unknown[i].file.Bytes() > unknown[j].file.Bytes() })
	if len(unknown) > limit {
		unknown = unknown[:max(limit, 0)]
	}
	return unknown
}

// identifyObfuscated identifies unidentified files from their first
// articles. Without a PAR2 index every candidate's head is probed at once.
// With one, the index is fetched in parallel with the heads: when one
// unknown file dominates the post (a plain video beside small extras),
// only its head is fetched first, and if that names it a video the other
// probes are skipped; otherwise the leading pair of heads is fetched with
// the index first so non-streamable or unrecognisable multi-part posts fail
// fast without downloading every volume's first segment.
func (b *builder) identifyObfuscated(ctx context.Context) error {
	unknown := b.unknownFiles()
	if len(unknown) == 0 {
		return nil
	}
	idx := b.rel.par2IndexFile()
	if idx == nil && len(unknown) <= 4 {
		return b.probeAll(ctx, unknown, nil)
	}
	wave := unknown
	if idx == nil {
		wave = []*postedFile{unknown[0], unknown[1], unknown[len(unknown)-1]}
	} else if len(unknown) > 1 && unknown[0].file.Bytes() >= 2*unknown[1].file.Bytes() {
		wave = unknown[:1]
	} else if len(unknown) > 2 {
		wave = unknown[:2]
	}
	var set *par2.Set
	if idx != nil {
		setCh := make(chan *par2.Set, 1)
		go func() { setCh <- b.rel.loadPar2(ctx, b.prio, b.fo, idx) }()
		_ = b.parallel(ctx, len(wave), func(ctx context.Context, i int) error {
			_, _ = b.e.segment(ctx, b.rel, wave[i], 0, b.prio, b.fo)
			return nil
		})
		set = <-setCh
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(wave) < len(unknown) {
		named, unreadable, err := b.probeObfuscated(ctx, wave, set)
		if err != nil {
			return err
		}
		if set == nil {
			set = b.rel.par2Ready()
		}
		if cands := b.candidates(b.rel.files); len(cands) > 0 {
			if err := b.checkCachedRAR(ctx, cands[0]); err != nil {
				return err
			}
		}
		info := wave[0].file.Info
		if info.Kind == nzb.KindVideo && !info.Sample && !info.Extra && !b.shouldProbe(b.candidates(b.rel.files)) {
			if named[wave[0]] {
				b.e.par2Saved.Add(int64(len(unknown) - len(wave)))
			}
			return nil
		}
		if len(wave) >= 2 && wave[0].file.Info.Kind == nzb.KindUnknown && wave[1].file.Info.Kind == nzb.KindUnknown &&
			!unreadable[wave[0]] && !unreadable[wave[1]] {
			if !wave[0].SizeKnown() && !wave[1].SizeKnown() {
				return incompleteError("leading files %s and %s are missing", wave[0].Name(), wave[1].Name())
			}
			return nil
		}
	}
	return b.probeAll(ctx, unknown, set)
}

// probeAll probes the unknown files, then once more with the rest of the
// unidentified files (up to maxProbeRounds*MaxProbes in total) when, with
// PAR2 loaded, the leading archive set is short of volumes no probed file
// turned out to be. Re-probing the first files reads their heads from the caches and
// keeps fallback volume numbering consistent across the whole list. A
// leading set still short of volumes that unprobed or unreadable files may
// hold is reported as errUnidentifiedVolumes, never as missing articles.
func (b *builder) probeAll(ctx context.Context, unknown []*postedFile, set *par2.Set) error {
	_, unreadable, err := b.probeObfuscated(ctx, unknown, set)
	if err != nil {
		return err
	}
	if set == nil {
		set = b.rel.par2Ready() // an obfuscated index found among the heads
	}
	if cands := b.candidates(b.rel.files); len(cands) > 0 {
		if err := b.checkCachedRAR(ctx, cands[0]); err != nil {
			return err
		}
	}
	probed := make(map[*postedFile]bool, len(unknown))
	for _, pf := range unknown {
		probed[pf] = true
	}
	if _, short := b.leadingSetShort(set); short && set != nil {
		if more := b.unknownFilesExcept(probed, maxProbeRounds*b.e.opts.MaxProbes-len(unknown)); len(more) > 0 {
			unknown = append(append([]*postedFile(nil), unknown...), more...)
			for _, pf := range more {
				probed[pf] = true
			}
			if _, unreadable, err = b.probeObfuscated(ctx, unknown, set); err != nil {
				return err
			}
		}
	}
	lead, short := b.leadingSetShort(set)
	if !short {
		return nil
	}
	unprobed := len(b.unknownFilesExcept(probed, len(b.rel.files)))
	stillUnknown := 0
	b.rel.infoMu.RLock()
	for pf := range unreadable {
		if pf.file.Info.Kind == nzb.KindUnknown {
			stillUnknown++
		}
	}
	b.rel.infoMu.RUnlock()
	if unprobed == 0 && stillUnknown == 0 {
		return nil // every file was read: planning reports the gaps as missing
	}
	return fmt.Errorf("%w: %s set %q is short of volumes; %d files beyond the probe budget, %d unreadable",
		errUnidentifiedVolumes, lead.kind, lead.set.Name, unprobed, stillUnknown)
}

// leadingSetShort reports whether the set planning would try first has
// volumes still unaccounted for: gaps in its volume numbers, or volumes
// PAR2 names that no posted file carries.
func (b *builder) leadingSetShort(set *par2.Set) (candidate, bool) {
	cands := b.candidates(b.rel.files)
	if len(cands) == 0 || cands[0].set == nil {
		return candidate{}, false
	}
	lead := cands[0]
	if len(lead.set.Missing) > 0 {
		return lead, true
	}
	if set == nil {
		return lead, false
	}
	posted := map[string]bool{}
	b.rel.infoMu.RLock()
	for _, pf := range b.rel.files {
		posted[pf.file.Name] = true
	}
	b.rel.infoMu.RUnlock()
	for _, f := range set.Files {
		name := f.BaseName()
		if posted[name] {
			continue
		}
		if info := nzb.Classify(name); info.Kind == lead.kind && info.SetName == lead.set.Name {
			return lead, true
		}
	}
	return lead, false
}

// probeObfuscated fetches the first article of the given unidentified files
// and re-classifies them: from the PAR2 description their head matches
// (set may be nil; a PAR2 index found among the heads is used too), else
// from the yEnc name or the content's magic bytes. It returns the files
// named from PAR2 and those whose head failed other than as missing.
func (b *builder) probeObfuscated(ctx context.Context, unknown []*postedFile, set *par2.Set) (map[*postedFile]bool, map[*postedFile]bool, error) {
	type probe struct {
		seg *segData
		err error
	}
	probes := make([]probe, len(unknown))
	_ = b.parallel(ctx, len(unknown), func(ctx context.Context, i int) error {
		seg, err := b.e.segment(ctx, b.rel, unknown[i], 0, b.prio, b.fo)
		probes[i] = probe{seg, err}
		return nil // one dead file must not stop the others
	})
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	unreadable := map[*postedFile]bool{}
	for i, pr := range probes {
		if pr.err != nil && !isMissing(pr.err) {
			unreadable[unknown[i]] = true
		}
	}
	if set == nil {
		// An obfuscated PAR2 index among the heads: the smallest readable
		// file starting with a PAR2 packet.
		var idx *postedFile
		for i, pf := range unknown {
			if pr := probes[i]; pr.err == nil && pr.seg != nil && pr.seg.begin == 0 && isPar2Head(pr.seg.data) &&
				par2Readable(pf) && (idx == nil || pf.file.Bytes() < idx.file.Bytes()) {
				idx = pf
			}
		}
		if idx != nil {
			set = b.rel.loadPar2(ctx, b.prio, b.fo, idx)
		}
	}
	named := map[*postedFile]bool{}
	type rarVol struct {
		pf     *postedFile
		number int // -1 unknown
	}
	var rars []rarVol
	for i, pf := range unknown {
		pr := probes[i]
		if pr.err != nil || pr.seg == nil {
			continue
		}
		if pr.seg.begin == 0 && isPar2Head(pr.seg.data) {
			b.rel.setFileInfo(func() {
				pf.file.Info = nzb.FileInfo{Kind: nzb.KindPar2, Ext: "par2", Volume: -1, SetName: pf.file.Info.SetName}
			})
			continue
		}
		if f := identifyByPar2(set, pf, pr.seg); f != nil && b.nameFromPar2(pf, f) {
			named[pf] = true
			continue
		}
		if yn := path.Base(strings.ReplaceAll(pr.seg.name, `\`, "/")); yn != "" && yn != "." {
			if info := nzb.Classify(yn); info.Kind != nzb.KindUnknown && !nzb.IsObfuscated(yn) {
				b.rel.setFileInfo(func() { pf.file.Name, pf.file.Info, pf.file.Obfuscated = yn, info, false })
				pf.setName(yn)
				continue
			}
		}
		if pr.seg.begin != 0 {
			continue
		}
		head := pr.seg.data
		switch archive.Detect(head) {
		case archive.FormatRAR4, archive.FormatRAR5:
			num := -1
			if v, err := b.readCachedRARVolume(ctx, pf, head); err == nil {
				if v.Number >= 0 {
					num = v.Number
				} else if v.First {
					num = 0
				}
			}
			rars = append(rars, rarVol{pf, num})
		case archive.Format7z:
			b.rel.setFileInfo(func() {
				pf.file.Info = nzb.FileInfo{Kind: nzb.KindSevenZip, Ext: "7z", Volume: 0, SetName: fmt.Sprintf("\x00obfuscated-7z-%d", pf.idx)}
			})
		default:
			if looksLikeVideo(head) {
				b.rel.setFileInfo(func() {
					pf.file.Info = nzb.FileInfo{Kind: nzb.KindVideo, Ext: "", Volume: -1, SetName: pf.file.Info.SetName}
				})
			}
		}
	}
	if len(rars) == 0 {
		return named, unreadable, nil
	}
	allNumbered := true
	for _, r := range rars {
		if r.number < 0 {
			allNumbered = false
		}
	}
	sort.SliceStable(rars, func(i, j int) bool {
		a, c := rars[i], rars[j]
		if allNumbered {
			return a.number < c.number
		}
		if (a.number == 0) != (c.number == 0) {
			return a.number == 0 // the first volume leads
		}
		if a.pf.file.SubjectIndex != c.pf.file.SubjectIndex {
			return a.pf.file.SubjectIndex < c.pf.file.SubjectIndex
		}
		return a.pf.idx < c.pf.idx
	})
	for i, r := range rars {
		vol := i
		if allNumbered {
			vol = r.number
		}
		b.rel.setFileInfo(func() {
			r.pf.file.Info = nzb.FileInfo{Kind: nzb.KindRAR, Ext: "rar", Volume: vol, SetName: "\x00obfuscated-rar", NewStyle: true}
		})
	}
	return named, unreadable, nil
}

// looksLikeVideo recognises common container signatures.
func looksLikeVideo(b []byte) bool {
	switch {
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}): // Matroska/WebM (EBML)
		return true
	case len(b) >= 8 && (string(b[4:8]) == "ftyp" || string(b[4:8]) == "moov" || string(b[4:8]) == "mdat" || string(b[4:8]) == "free"):
		return true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI ":
		return true
	case len(b) >= 4 && (bytes.Equal(b[:4], []byte{0x00, 0x00, 0x01, 0xba}) || bytes.Equal(b[:4], []byte{0x00, 0x00, 0x01, 0xb3})): // MPEG program/elementary video stream
		return true
	case len(b) >= 9 && string(b[:3]) == "FLV" && b[3] == 1 && b[4]&^byte(5) == 0 && binary.BigEndian.Uint32(b[5:9]) >= 9:
		return true
	case len(b) >= 27 && string(b[:4]) == "OggS" && b[4] == 0 && b[5]&^byte(7) == 0:
		return true
	case len(b) >= 16 && bytes.Equal(b[:8], []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11}): // ASF/WMV
		return true
	case len(b) > 376 && b[0] == 0x47 && b[188] == 0x47 && b[376] == 0x47: // MPEG-TS
		return true
	case len(b) > 392 && b[4] == 0x47 && b[196] == 0x47 && b[388] == 0x47: // M2TS (192-byte packets)
		return true
	}
	return false
}

func vintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
