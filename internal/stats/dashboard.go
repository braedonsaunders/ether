package stats

import (
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
)

// Dashboard is this node's in-memory Plex Dashboard store: devices,
// per-second bandwidth, and completed-play counts. Clustered nodes each
// keep their own rings; there is no request pin, so a client reads the
// node it connected to. Request paths only walk the requested buckets.
type Dashboard struct {
	now func() time.Time
	loc *time.Location

	devMu    sync.Mutex
	nextDev  int64
	byClient map[string]*Device
	byID     map[int64]*Device
	accounts map[int64]string

	mu        sync.Mutex
	bandwidth map[int]*bwTier
	media     map[int]*mediaTier
}

// Device is one client the Dashboard can name.
type Device struct {
	ID               int64
	ClientIdentifier string
	Name             string
	Platform         string
	CreatedAt        int64
}

// BandwidthRow is one StatisticsBandwidth series point.
type BandwidthRow struct {
	AccountID int64
	DeviceID  int64
	Timespan  int
	At        int64
	Lan       int
	Bytes     int64
}

// MediaRow is one StatisticsMedia series point.
type MediaRow struct {
	AccountID    int64
	DeviceID     int64
	Timespan     int
	At           int64
	MetadataType int
	Count        int64
	Duration     int64
}

// Query is the parsed Dashboard filter. Timespans empty means "every kept
// rollup except per-second" for bandwidth/media (0..4).
type Query struct {
	Timespans                     []int
	AccountID                     int64
	DeviceID                      int64
	HasAccount, HasDevice, HasLan bool
	Lan                           int
	AtMin, AtMax                  int64
	ExactAt                       int64
	HasExact, HasAtMin, HasAtMax  bool
	Offset                        int
	Limit                         int
	HasLimit                      bool
	Sort                          string
	Desc                          bool
}

// Page is one statistics answer: the series window and the Device/Account
// lookup for ids that window names.
type Page struct {
	Rows      []BandwidthRow
	Media     []MediaRow
	Devices   []Device
	Accounts  []AccountRef
	TotalSize int
}

// AccountRef labels an account id on a statistics page.
type AccountRef struct {
	ID   int64
	Name string
}

type bwSeriesKey struct {
	account, device int64
	lan             int
}

type mediaSeriesKey struct {
	account, device int64
	metadataType    int
}

type mediaVal struct {
	count, duration int64
}

type bwBucket struct {
	at   int64
	rows map[bwSeriesKey]int64
}

type mediaBucket struct {
	at   int64
	rows map[mediaSeriesKey]mediaVal
}

type bwTier struct {
	span    int
	buckets []bwBucket
}

type mediaTier struct {
	span    int
	buckets []mediaBucket
}

var dashCaps = map[int]int{
	hoststat.SpanSec:   3600,
	hoststat.SpanHour:  24 * 14,
	hoststat.SpanDay:   90,
	hoststat.SpanWeek:  52,
	hoststat.SpanMonth: 24,
	hoststat.SpanYear:  10,
}

var dashOrder = []int{hoststat.SpanSec, hoststat.SpanHour, hoststat.SpanDay, hoststat.SpanWeek, hoststat.SpanMonth, hoststat.SpanYear}

// DashboardOptions configure the clock (tests).
type DashboardOptions struct {
	Now func() time.Time
	Loc *time.Location
}

// NewDashboard creates the anonymous device (id 1) the way a PMS does at start.
func NewDashboard(opts DashboardOptions) *Dashboard {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Loc == nil {
		opts.Loc = time.Local
	}
	d := &Dashboard{
		now:       opts.Now,
		loc:       opts.Loc,
		nextDev:   1,
		byClient:  map[string]*Device{},
		byID:      map[int64]*Device{},
		accounts:  map[int64]string{},
		bandwidth: map[int]*bwTier{},
		media:     map[int]*mediaTier{},
	}
	for _, span := range dashOrder {
		d.bandwidth[span] = &bwTier{span: span}
		d.media[span] = &mediaTier{span: span}
	}
	anon := &Device{ID: 1, CreatedAt: opts.Now().Unix()}
	d.nextDev = 2
	d.byClient[""] = anon
	d.byID[1] = anon
	return d
}

// SeeDevice records or updates a client. An empty client identifier is device 1.
func (d *Dashboard) SeeDevice(clientID, name, platform string) Device {
	d.devMu.Lock()
	defer d.devMu.Unlock()
	return d.seeLocked(clientID, name, platform)
}

func (d *Dashboard) seeLocked(clientID, name, platform string) Device {
	if dev, ok := d.byClient[clientID]; ok {
		if name != "" {
			dev.Name = name
		}
		if platform != "" {
			dev.Platform = platform
		}
		return *dev
	}
	if len(d.byID) >= 512 {
		return *d.byID[1]
	}
	dev := &Device{ID: d.nextDev, ClientIdentifier: clientID, Name: name, Platform: platform, CreatedAt: d.now().Unix()}
	d.nextDev++
	d.byClient[clientID] = dev
	d.byID[dev.ID] = dev
	return *dev
}

// Devices lists every device this process has seen, id order.
func (d *Dashboard) Devices() []Device {
	d.devMu.Lock()
	defer d.devMu.Unlock()
	out := make([]Device, 0, len(d.byID))
	for _, dev := range d.byID {
		out = append(out, *dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Device returns one row by numeric id.
func (d *Dashboard) Device(id int64) (Device, bool) {
	d.devMu.Lock()
	defer d.devMu.Unlock()
	dev, ok := d.byID[id]
	if !ok {
		return Device{}, false
	}
	return *dev, true
}

// RecordBytes adds client-facing playback bytes to the per-second series.
func (d *Dashboard) RecordBytes(accountID int64, accountName, clientID, deviceName, platform string, lan bool, n int64) {
	if n <= 0 {
		return
	}
	d.devMu.Lock()
	if accountName != "" {
		d.accounts[accountID] = accountName
	}
	dev := d.seeLocked(clientID, deviceName, platform)
	d.devMu.Unlock()
	lanN := 0
	if lan {
		lanN = 1
	}
	now := d.now()
	key := bwSeriesKey{account: accountID, device: dev.ID, lan: lanN}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, span := range []int{hoststat.SpanSec, hoststat.SpanHour, hoststat.SpanDay, hoststat.SpanWeek, hoststat.SpanMonth, hoststat.SpanYear} {
		d.addBWLocked(span, now, key, n)
	}
}

// RecordPlay counts one completed play into the media series.
func (d *Dashboard) RecordPlay(accountID int64, accountName, clientID string, metadataType int, durationMs int64) {
	if metadataType <= 0 {
		return
	}
	d.devMu.Lock()
	if accountName != "" {
		d.accounts[accountID] = accountName
	}
	dev := d.seeLocked(clientID, "", "")
	d.devMu.Unlock()
	now := d.now()
	key := mediaSeriesKey{account: accountID, device: dev.ID, metadataType: metadataType}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Media keeps hour and coarser, not per-second.
	for _, span := range []int{hoststat.SpanHour, hoststat.SpanDay, hoststat.SpanWeek, hoststat.SpanMonth, hoststat.SpanYear} {
		d.addMediaLocked(span, now, key, durationMs)
	}
}

func (d *Dashboard) addBWLocked(span int, now time.Time, key bwSeriesKey, n int64) {
	tier := d.bandwidth[span]
	if tier == nil {
		return
	}
	at := hoststat.BucketStart(now.In(d.loc), span, d.loc).Unix()
	i := sort.Search(len(tier.buckets), func(i int) bool { return tier.buckets[i].at >= at })
	if i < len(tier.buckets) && tier.buckets[i].at == at {
		tier.buckets[i].rows[key] += n
	} else {
		b := bwBucket{at: at, rows: map[bwSeriesKey]int64{key: n}}
		tier.buckets = append(tier.buckets, bwBucket{})
		copy(tier.buckets[i+1:], tier.buckets[i:])
		tier.buckets[i] = b
	}
	d.dropOldBWLocked(span)
}

func (d *Dashboard) dropOldBWLocked(span int) {
	tier := d.bandwidth[span]
	if span == hoststat.SpanSec {
		cutoff := d.now().Add(-time.Hour).Unix()
		for len(tier.buckets) > 0 && tier.buckets[0].at < cutoff {
			tier.buckets = tier.buckets[1:]
		}
		return
	}
	capN := dashCaps[span]
	if len(tier.buckets) > capN {
		tier.buckets = tier.buckets[len(tier.buckets)-capN:]
	}
}

func (d *Dashboard) addMediaLocked(span int, now time.Time, key mediaSeriesKey, durationMs int64) {
	tier := d.media[span]
	if tier == nil {
		return
	}
	at := hoststat.BucketStart(now.In(d.loc), span, d.loc).Unix()
	i := sort.Search(len(tier.buckets), func(i int) bool { return tier.buckets[i].at >= at })
	if i < len(tier.buckets) && tier.buckets[i].at == at {
		v := tier.buckets[i].rows[key]
		v.count++
		if durationMs > 0 {
			v.duration = durationMs
		}
		tier.buckets[i].rows[key] = v
	} else {
		b := mediaBucket{at: at, rows: map[mediaSeriesKey]mediaVal{key: {count: 1, duration: durationMs}}}
		tier.buckets = append(tier.buckets, mediaBucket{})
		copy(tier.buckets[i+1:], tier.buckets[i:])
		tier.buckets[i] = b
	}
	d.dropOldMediaLocked(span)
}

func (d *Dashboard) dropOldMediaLocked(span int) {
	tier := d.media[span]
	capN := dashCaps[span]
	if len(tier.buckets) > capN {
		tier.buckets = tier.buckets[len(tier.buckets)-capN:]
	}
}

// Bandwidth returns the filtered, paged bandwidth series.
func (d *Dashboard) Bandwidth(q Query) Page {
	d.mu.Lock()
	var rows []BandwidthRow
	for _, span := range q.Timespans {
		tier := d.bandwidth[span]
		if tier == nil {
			continue
		}
		for _, b := range tier.buckets {
			if !matchAt(q, b.at) {
				continue
			}
			for key, n := range b.rows {
				if !matchIDs(q, key.account, key.device, key.lan) {
					continue
				}
				rows = append(rows, BandwidthRow{AccountID: key.account, DeviceID: key.device, Timespan: span, At: b.at, Lan: key.lan, Bytes: n})
			}
		}
	}
	d.mu.Unlock()
	sortBandwidth(rows, q.Sort, q.Desc)
	total := len(rows)
	rows = pageRows(rows, q.Offset, q.Limit, q.HasLimit)
	d.devMu.Lock()
	page := Page{Rows: rows, Devices: d.lookupDevices(rows), Accounts: d.lookupAccounts(rows), TotalSize: total}
	d.devMu.Unlock()
	return page
}

// Media returns the filtered, paged completed-play series.
func (d *Dashboard) Media(q Query) Page {
	d.mu.Lock()
	var rows []MediaRow
	for _, span := range q.Timespans {
		tier := d.media[span]
		if tier == nil {
			continue
		}
		for _, b := range tier.buckets {
			if !matchAt(q, b.at) {
				continue
			}
			for key, v := range b.rows {
				if !matchIDs(q, key.account, key.device, -1) {
					continue
				}
				rows = append(rows, MediaRow{AccountID: key.account, DeviceID: key.device, Timespan: span, At: b.at, MetadataType: key.metadataType, Count: v.count, Duration: v.duration})
			}
		}
	}
	d.mu.Unlock()
	sortMedia(rows, q.Sort, q.Desc)
	total := len(rows)
	rows = pageRows(rows, q.Offset, q.Limit, q.HasLimit)
	d.devMu.Lock()
	page := Page{Media: rows, Devices: d.lookupMediaDevices(rows), Accounts: d.lookupMediaAccounts(rows), TotalSize: total}
	d.devMu.Unlock()
	return page
}

func matchAt(q Query, at int64) bool {
	if q.HasExact && at != q.ExactAt {
		return false
	}
	if q.HasAtMin && at < q.AtMin {
		return false
	}
	if q.HasAtMax && at > q.AtMax {
		return false
	}
	return true
}

func matchIDs(q Query, account, device int64, lan int) bool {
	if q.HasAccount && account != q.AccountID {
		return false
	}
	if q.HasDevice && device != q.DeviceID {
		return false
	}
	if q.HasLan && lan >= 0 && lan != q.Lan {
		return false
	}
	return true
}

func (d *Dashboard) lookupDevices(rows []BandwidthRow) []Device {
	seen := map[int64]bool{}
	var out []Device
	for _, row := range rows {
		if seen[row.DeviceID] {
			continue
		}
		seen[row.DeviceID] = true
		if dev, ok := d.byID[row.DeviceID]; ok {
			out = append(out, *dev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (d *Dashboard) lookupMediaDevices(rows []MediaRow) []Device {
	seen := map[int64]bool{}
	var out []Device
	for _, row := range rows {
		if seen[row.DeviceID] {
			continue
		}
		seen[row.DeviceID] = true
		if dev, ok := d.byID[row.DeviceID]; ok {
			out = append(out, *dev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (d *Dashboard) lookupAccounts(rows []BandwidthRow) []AccountRef {
	seen := map[int64]bool{}
	var out []AccountRef
	for _, row := range rows {
		if seen[row.AccountID] {
			continue
		}
		seen[row.AccountID] = true
		out = append(out, AccountRef{ID: row.AccountID, Name: d.accounts[row.AccountID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (d *Dashboard) lookupMediaAccounts(rows []MediaRow) []AccountRef {
	seen := map[int64]bool{}
	var out []AccountRef
	for _, row := range rows {
		if seen[row.AccountID] {
			continue
		}
		seen[row.AccountID] = true
		out = append(out, AccountRef{ID: row.AccountID, Name: d.accounts[row.AccountID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func pageRows[T any](rows []T, offset, limit int, hasLimit bool) []T {
	if offset < 0 {
		offset = 0
	}
	if hasLimit && limit <= 0 {
		return nil
	}
	if offset >= len(rows) {
		return nil
	}
	rows = rows[offset:]
	if hasLimit && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func sortBandwidth(rows []BandwidthRow, key string, desc bool) {
	less := func(i, j int) bool {
		a, b := rows[i], rows[j]
		var cmp int
		switch key {
		case "accountID":
			cmp = cmpI64(a.AccountID, b.AccountID)
		case "deviceID":
			cmp = cmpI64(a.DeviceID, b.DeviceID)
		case "timespan":
			cmp = cmpI64(int64(a.Timespan), int64(b.Timespan))
		case "lan":
			cmp = cmpI64(int64(a.Lan), int64(b.Lan))
		case "bytes":
			cmp = cmpI64(a.Bytes, b.Bytes)
		default:
			cmp = cmpI64(a.At, b.At)
		}
		if cmp == 0 {
			cmp = cmpI64(a.At, b.At)
		}
		if desc {
			return cmp > 0
		}
		return cmp < 0
	}
	sort.SliceStable(rows, less)
}

func sortMedia(rows []MediaRow, key string, desc bool) {
	less := func(i, j int) bool {
		a, b := rows[i], rows[j]
		var cmp int
		switch key {
		case "accountID":
			cmp = cmpI64(a.AccountID, b.AccountID)
		case "deviceID":
			cmp = cmpI64(a.DeviceID, b.DeviceID)
		case "timespan":
			cmp = cmpI64(int64(a.Timespan), int64(b.Timespan))
		case "count":
			cmp = cmpI64(a.Count, b.Count)
		case "duration":
			cmp = cmpI64(a.Duration, b.Duration)
		default:
			cmp = cmpI64(a.At, b.At)
		}
		if cmp == 0 {
			cmp = cmpI64(a.At, b.At)
		}
		if desc {
			return cmp > 0
		}
		return cmp < 0
	}
	sort.SliceStable(rows, less)
}

func cmpI64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
