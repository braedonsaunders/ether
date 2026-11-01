package stats

import (
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
)

// Dashboard is the in-memory Plex Dashboard store: devices, per-second
// bandwidth, and completed-play counts. Request paths only walk the
// requested buckets. Cluster answers come from this node.
type Dashboard struct {
	now func() time.Time
	loc *time.Location

	mu        sync.Mutex
	nextDev   int64
	byClient  map[string]*Device
	byID      map[int64]*Device
	accounts  map[int64]string
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

type bwKey struct {
	account, device int64
	lan             int
	at              int64
}

type mediaKey struct {
	account, device, at int64
	metadataType        int
}

type bwTier struct {
	span int
	rows map[bwKey]int64
	ats  []int64
}

type mediaTier struct {
	span int
	rows map[mediaKey]int64
	ats  []int64
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
		d.bandwidth[span] = &bwTier{span: span, rows: map[bwKey]int64{}}
		d.media[span] = &mediaTier{span: span, rows: map[mediaKey]int64{}}
	}
	anon := &Device{ID: 1, CreatedAt: opts.Now().Unix()}
	d.nextDev = 2
	d.byClient[""] = anon
	d.byID[1] = anon
	return d
}

// SeeDevice records or updates a client. An empty client identifier is device 1.
func (d *Dashboard) SeeDevice(clientID, name, platform string) Device {
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Device, 0, len(d.byID))
	for _, dev := range d.byID {
		out = append(out, *dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Device returns one row by numeric id.
func (d *Dashboard) Device(id int64) (Device, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
	if accountName != "" {
		d.accounts[accountID] = accountName
	}
	dev := d.seeLocked(clientID, deviceName, platform)
	lanN := 0
	if lan {
		lanN = 1
	}
	now := d.now()
	for _, span := range []int{hoststat.SpanSec, hoststat.SpanHour, hoststat.SpanDay, hoststat.SpanWeek, hoststat.SpanMonth, hoststat.SpanYear} {
		d.addBWLocked(span, bwKey{account: accountID, device: dev.ID, lan: lanN, at: now.Unix()}, n)
	}
}

// RecordPlay counts one completed play into the media series.
func (d *Dashboard) RecordPlay(accountID int64, accountName, clientID string, metadataType int) {
	if metadataType <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if accountName != "" {
		d.accounts[accountID] = accountName
	}
	dev := d.seeLocked(clientID, "", "")
	now := d.now()
	// Media keeps hour and coarser, not per-second.
	for _, span := range []int{hoststat.SpanHour, hoststat.SpanDay, hoststat.SpanWeek, hoststat.SpanMonth, hoststat.SpanYear} {
		d.addMediaLocked(span, mediaKey{account: accountID, device: dev.ID, metadataType: metadataType, at: now.Unix()}, 1)
	}
}

func (d *Dashboard) addBWLocked(span int, key bwKey, n int64) {
	tier := d.bandwidth[span]
	if tier == nil {
		return
	}
	key.at = hoststat.BucketStart(time.Unix(key.at, 0).In(d.loc), span, d.loc).Unix()
	if _, ok := tier.rows[key]; !ok {
		tier.ats = insertAt(tier.ats, key.at)
	}
	tier.rows[key] += n
	d.dropOldBWLocked(span)
}

func (d *Dashboard) dropOldBWLocked(span int) {
	tier := d.bandwidth[span]
	capN := dashCaps[span]
	for len(tier.ats) > capN {
		old := tier.ats[0]
		tier.ats = tier.ats[1:]
		for key := range tier.rows {
			if key.at == old {
				delete(tier.rows, key)
			}
		}
	}
}

func (d *Dashboard) addMediaLocked(span int, key mediaKey, n int64) {
	tier := d.media[span]
	if tier == nil {
		return
	}
	key.at = hoststat.BucketStart(time.Unix(key.at, 0).In(d.loc), span, d.loc).Unix()
	if _, ok := tier.rows[key]; !ok {
		tier.ats = insertAt(tier.ats, key.at)
	}
	tier.rows[key] += n
	d.dropOldMediaLocked(span)
}

func (d *Dashboard) dropOldMediaLocked(span int) {
	tier := d.media[span]
	capN := dashCaps[span]
	for len(tier.ats) > capN {
		old := tier.ats[0]
		tier.ats = tier.ats[1:]
		for key := range tier.rows {
			if key.at == old {
				delete(tier.rows, key)
			}
		}
	}
}

func insertAt(ats []int64, at int64) []int64 {
	i := sort.Search(len(ats), func(i int) bool { return ats[i] >= at })
	if i < len(ats) && ats[i] == at {
		return ats
	}
	ats = append(ats, 0)
	copy(ats[i+1:], ats[i:])
	ats[i] = at
	return ats
}

// Bandwidth returns the filtered, paged bandwidth series.
func (d *Dashboard) Bandwidth(q Query) Page {
	d.mu.Lock()
	defer d.mu.Unlock()
	var rows []BandwidthRow
	for _, span := range q.Timespans {
		tier := d.bandwidth[span]
		if tier == nil {
			continue
		}
		for key, n := range tier.rows {
			if !matchSeries(q, key.account, key.device, key.lan, key.at) {
				continue
			}
			rows = append(rows, BandwidthRow{AccountID: key.account, DeviceID: key.device, Timespan: span, At: key.at, Lan: key.lan, Bytes: n})
		}
	}
	sortBandwidth(rows, q.Sort, q.Desc)
	total := len(rows)
	rows = pageRows(rows, q.Offset, q.Limit, q.HasLimit)
	return Page{Rows: rows, Devices: d.lookupDevices(rows), Accounts: d.lookupAccounts(rows), TotalSize: total}
}

// Media returns the filtered, paged completed-play series.
func (d *Dashboard) Media(q Query) Page {
	d.mu.Lock()
	defer d.mu.Unlock()
	var rows []MediaRow
	for _, span := range q.Timespans {
		tier := d.media[span]
		if tier == nil {
			continue
		}
		for key, n := range tier.rows {
			if !matchSeries(q, key.account, key.device, -1, key.at) {
				continue
			}
			rows = append(rows, MediaRow{AccountID: key.account, DeviceID: key.device, Timespan: span, At: key.at, MetadataType: key.metadataType, Count: n})
		}
	}
	sortMedia(rows, q.Sort, q.Desc)
	total := len(rows)
	rows = pageRows(rows, q.Offset, q.Limit, q.HasLimit)
	return Page{Media: rows, Devices: d.lookupMediaDevices(rows), Accounts: d.lookupMediaAccounts(rows), TotalSize: total}
}

func matchSeries(q Query, account, device int64, lan int, at int64) bool {
	if q.HasAccount && account != q.AccountID {
		return false
	}
	if q.HasDevice && device != q.DeviceID {
		return false
	}
	if q.HasLan && lan >= 0 && lan != q.Lan {
		return false
	}
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
