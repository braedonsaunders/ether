package personalize

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Training: a pooled, L2-regularised logistic regression on the labelled
// serve log, fitted by Newton's method (IRLS) on standardised features,
// with balanced class weights and a prior correction of the intercept so
// probabilities stay calibrated. Deterministic for a given log.

// Position features: one-hot slots (row 2…8+, item 2…12+; the first row
// and item are the reference), present during training and fixed at their
// training mean at inference (position debiasing). One-hot slots let the
// model learn any shape of position bias, so none of it leaks into
// position-correlated features.
const (
	posRowSlots  = 8
	posItemSlots = 12
)

func posRowName(slot int) string  { return "pos:row:" + strconv.Itoa(slot) }
func posItemName(slot int) string { return "pos:item:" + strconv.Itoa(slot) }

func isPosFeature(name string) bool { return strings.HasPrefix(name, "pos:") }

func posNames() []string {
	var out []string
	for s := 1; s < posRowSlots; s++ {
		out = append(out, posRowName(s))
	}
	for s := 1; s < posItemSlots; s++ {
		out = append(out, posItemName(s))
	}
	return out
}

// Training tunables.
const (
	rankL2           = 2.0     // ridge on standardised weights
	rankHoldout      = 0.2     // last share of examples (by time) held out
	rankMaxExamples  = 150_000 // most recent labelled examples trained on
	rankMinTrain     = 100     // fewer labelled examples: nothing to fit
	rankMinTrainPos  = 5
	rankMinTestPos   = 5
	rankMinTestNeg   = 20
	rankMinAUC       = 0.55
	rankLiftFull     = 0.08 // AUC lift at which the blend weight is full
	rankScoreSharp   = 1.7  // logistic approximation of the normal CDF
	rankMinLogitStd  = 1.0  // floor of the logit spread used for model scores
	rankMaxIters     = 40
	rankConvergeStep = 1e-7
)

// rankExample is one labelled serve item.
type rankExample struct {
	at       time.Time
	user     int64
	raw      [numRaw]float32
	kind     string
	surface  string
	rowPos   int
	itemPos  int
	baseline float64
	y        float64 // target: graded play reward, 0 when not played
}

func (x *rankExample) positive() bool { return x.y >= 0.5 }

// designNames is the model's feature layout: snapshot features, one-hot
// row kind and surface, then the two position features.
func learnedRowKind(kind, source string) bool {
	return !skipKind(kind) && behaviorForRowSource(source).learnsKind
}

func designNames() []string {
	names := make([]string, 0, int(numRaw)+len(kinds)+8)
	for _, f := range rawFeatures {
		names = append(names, f.name)
	}
	for _, k := range kinds {
		if learnedRowKind(k.kind, k.source) {
			names = append(names, "kind:"+k.kind)
		}
	}
	names = append(names, "kind:collection", "kind:other", "surface:home", "surface:movies", "surface:shows")
	names = append(names, posNames()...)
	return names
}

// kindFeature is the one-hot feature name of a row kind.
func kindFeature(kind string) string {
	if k, ok := kindByName[kind]; ok && learnedRowKind(k.kind, k.source) {
		return "kind:" + kind
	}
	if strings.HasPrefix(kind, KindCollectionPrefix) {
		return "kind:collection"
	}
	return "kind:other"
}

// sparseRow is one example's standardised design row: continuous
// features (snapshot and positions) are standardised, one-hots stay 0/1,
// the bias column is last.
type sparseRow struct {
	idx []int
	val []float64
}

// layout locates a design's columns.
type layout struct {
	names []string
	index map[string]int
	cont  []int // standardised columns: raw features then position slots
	nRow  int   // position row slots in cont (after the raw features)
}

func newLayout(names []string) *layout {
	l := &layout{names: names, index: make(map[string]int, len(names))}
	for i, n := range names {
		l.index[n] = i
	}
	for i := 0; i < int(numRaw); i++ {
		l.cont = append(l.cont, i)
	}
	for s := 1; s < posRowSlots; s++ {
		l.cont = append(l.cont, l.index[posRowName(s)])
	}
	l.nRow = posRowSlots - 1
	for s := 1; s < posItemSlots; s++ {
		l.cont = append(l.cont, l.index[posItemName(s)])
	}
	return l
}

// continuous returns an example's continuous feature values (same order
// as layout.cont).
func (l *layout) continuous(x *rankExample, out []float64) {
	for i := 0; i < int(numRaw); i++ {
		out[i] = float64(x.raw[i])
	}
	for k := int(numRaw); k < len(out); k++ {
		out[k] = 0
	}
	if s := min(x.rowPos, posRowSlots-1); s >= 1 {
		out[int(numRaw)+s-1] = 1
	}
	if s := min(x.itemPos, posItemSlots-1); s >= 1 {
		out[int(numRaw)+l.nRow+s-1] = 1
	}
}

// row builds the standardised sparse row of an example into r.
func (l *layout) row(x *rankExample, mean, std []float64, cont []float64, r *sparseRow) {
	r.idx, r.val = r.idx[:0], r.val[:0]
	l.continuous(x, cont)
	for k, j := range l.cont {
		r.idx = append(r.idx, j)
		r.val = append(r.val, (cont[k]-mean[j])/std[j])
	}
	if j, ok := l.index[kindFeature(x.kind)]; ok {
		r.idx = append(r.idx, j)
		r.val = append(r.val, 1)
	}
	if j, ok := l.index["surface:"+x.surface]; ok {
		r.idx = append(r.idx, j)
		r.val = append(r.val, 1)
	}
	r.idx = append(r.idx, len(l.names))
	r.val = append(r.val, 1)
}

// rankModel is a trained model with its evaluation; persisted as JSON.
type rankModel struct {
	TrainedAt   time.Time `json:"trainedAt"`
	Examples    int       `json:"examples"`
	Positives   int       `json:"positives"`
	Labels      int       `json:"labels"` // labelled serve items when trained
	AUC         float64   `json:"auc"`
	BaselineAUC float64   `json:"baselineAuc"`
	Active      bool      `json:"active"`
	// Names/Mean/Std/Weights describe the standardised linear model;
	// Bias is calibrated (class-weight prior correction applied).
	Names   []string  `json:"names"`
	Mean    []float64 `json:"mean"`
	Std     []float64 `json:"std"`
	Weights []float64 `json:"weights"`
	Bias    float64   `json:"bias"`
	// LogitMean/LogitStd are the inference-mode logits' spread on the
	// training data; they map a logit onto a 0..1 model score.
	LogitMean float64 `json:"logitMean"`
	LogitStd  float64 `json:"logitStd"`
	// Blend is the model's share of an item score; RowWeight scales the
	// row multiplier. Both grow with data volume and lift.
	Blend     float64 `json:"blend"`
	RowWeight float64 `json:"rowWeight"`

	// compiled inference form
	cConst float64
	cRaw   [numRaw]float64
	cOne   map[string]float64
}

// compile folds standardisation into per-feature weights, mapping by name
// so a model trained before the feature layout changed still applies.
// Positions contribute nothing (they sit at their training mean).
func (m *rankModel) compile() {
	m.cConst = m.Bias
	m.cOne = map[string]float64{}
	m.cRaw = [numRaw]float64{}
	raw := map[string]int{}
	for i, f := range rawFeatures {
		raw[f.name] = i
	}
	for j, name := range m.Names {
		if j >= len(m.Weights) || j >= len(m.Mean) || j >= len(m.Std) || isPosFeature(name) {
			continue
		}
		std := m.Std[j]
		if std <= 0 {
			std = 1
		}
		w := m.Weights[j] / std
		m.cConst -= w * m.Mean[j]
		if i, ok := raw[name]; ok {
			m.cRaw[i] = w
		} else {
			m.cOne[name] = w
		}
	}
	if m.LogitStd <= 0 {
		m.LogitStd = 1
	}
}

// logit is the calibrated log-odds of a play at inference (positions
// fixed).
func (m *rankModel) logit(raw []float32, kind, surface string) float64 {
	z := m.cConst + m.cOne[kindFeature(kind)] + m.cOne["surface:"+surface]
	for i, w := range m.cRaw {
		z += w * float64(raw[i])
	}
	return z
}

// prob is the calibrated play probability of a served title.
func (m *rankModel) prob(raw []float32, kind, surface string) float64 {
	return sigmoid(m.logit(raw, kind, surface))
}

// modelScore maps a title's logit onto 0..1 relative to the titles the
// model trained on (0.5 = typical).
func (m *rankModel) modelScore(raw []float32, kind, surface string) float64 {
	return sigmoid(rankScoreSharp * (m.logit(raw, kind, surface) - m.LogitMean) / m.LogitStd)
}

// itemDelta blends the model score into a hand-tuned item score:
// Blend × (model − score), bounded by maxItemDelta.
func (m *rankModel) itemDelta(raw []float32, kind, surface string, score float64) float64 {
	return clamp(m.Blend*(m.modelScore(raw, kind, surface)-clamp(score, 0, 1)), -maxItemDelta, maxItemDelta)
}

// rowLift is the bounded row multiplier from the mean model score of a
// row's top titles.
func (m *rankModel) rowLift(mean float64) float64 {
	return clamp(1+m.RowWeight*(2*mean-1), 1-rowLiftBound, 1+rowLiftBound)
}

func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	ez := math.Exp(z)
	return ez / (1 + ez)
}

// ---- training ----------------------------------------------------------------

// fitRanker trains on time-ordered examples: fit on the first 80%,
// evaluate model and baseline AUC on the last 20%, gate, then refit on
// everything. It returns nil when there is too little data to fit.
func fitRanker(xs []rankExample, o RankerOptions, now time.Time) *rankModel {
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].at.Before(xs[j].at) })
	pos := 0
	for i := range xs {
		if xs[i].positive() {
			pos++
		}
	}
	if len(xs) < rankMinTrain || pos < rankMinTrainPos || pos == len(xs) {
		return nil
	}
	names := designNames()
	split := int(float64(len(xs)) * (1 - rankHoldout))
	train, test := xs[:split], xs[split:]
	m := &rankModel{TrainedAt: now, Examples: len(xs), Positives: pos}
	if fit := fitLogistic(train, names); fit != nil {
		fit.compile()
		scores := make([]float64, len(test))
		base := make([]float64, len(test))
		labels := make([]bool, len(test))
		testPos := 0
		for i := range test {
			scores[i] = fit.logit(test[i].raw[:], test[i].kind, test[i].surface)
			base[i] = test[i].baseline
			labels[i] = test[i].positive()
			if labels[i] {
				testPos++
			}
		}
		m.AUC, m.BaselineAUC = auc(scores, labels), auc(base, labels)
		m.Active = len(xs) >= o.MinExamples && pos >= o.MinPositives &&
			testPos >= rankMinTestPos && len(test)-testPos >= rankMinTestNeg &&
			m.AUC >= rankMinAUC && m.AUC >= m.BaselineAUC+o.MinLift
	}
	full := fitLogistic(xs, names)
	if full == nil {
		return nil
	}
	m.Names, m.Mean, m.Std, m.Weights, m.Bias = full.Names, full.Mean, full.Std, full.Weights, full.Bias
	m.compile()
	// Spread of inference-mode logits, to map logits onto 0..1.
	var sum, sq float64
	for i := range xs {
		z := m.logit(xs[i].raw[:], xs[i].kind, xs[i].surface)
		sum += z
		sq += z * z
	}
	n := float64(len(xs))
	m.LogitMean = sum / n
	// The spread is floored at one logit: a model whose predictions barely
	// differ must not have that difference stretched over 0..1.
	m.LogitStd = math.Max(math.Sqrt(math.Max(sq/n-m.LogitMean*m.LogitMean, 0)), rankMinLogitStd)
	if m.Active {
		volume := clamp(math.Log(float64(pos)/float64(o.MinPositives))/math.Log(30), 0, 1)
		lift := clamp((m.AUC-m.BaselineAUC)/rankLiftFull, 0.25, 1)
		conf := (0.3 + 0.7*volume) * lift
		m.Blend = o.MaxBlend * conf
		m.RowWeight = rowLiftBound * conf
	}
	return m
}

// fitLogistic fits standardised weights by Newton/IRLS with an L2 ridge
// and balanced class weights; the returned Bias is prior-corrected.
func fitLogistic(xs []rankExample, names []string) *rankModel {
	n, d := len(xs), len(names)
	if n == 0 {
		return nil
	}
	l := newLayout(names)
	cols := d + 1
	// Standardise continuous columns; one-hot columns keep mean 0, std 1.
	mean := make([]float64, d)
	std := make([]float64, d)
	for j := range std {
		std[j] = 1
	}
	cont := make([]float64, len(l.cont))
	sum := make([]float64, len(l.cont))
	sq := make([]float64, len(l.cont))
	for i := range xs {
		l.continuous(&xs[i], cont)
		for k, v := range cont {
			sum[k] += v
			sq[k] += v * v
		}
	}
	for k, j := range l.cont {
		mean[j] = sum[k] / float64(n)
		s := math.Sqrt(math.Max(sq[k]/float64(n)-mean[j]*mean[j], 0))
		if s < 1e-9 {
			s = 1
		}
		std[j] = s
	}
	// Balanced class weights.
	pos := 0
	for i := range xs {
		if xs[i].positive() {
			pos++
		}
	}
	neg := n - pos
	if pos == 0 || neg == 0 {
		return nil
	}
	wPos, wNeg := float64(n)/(2*float64(pos)), float64(n)/(2*float64(neg))
	w := make([]float64, n)
	y := make([]float64, n)
	ySum, wSum := 0.0, 0.0
	for i := range xs {
		w[i] = wNeg
		if xs[i].positive() {
			w[i] = wPos
		}
		y[i] = clamp(xs[i].y, 0, 1)
		ySum += w[i] * y[i]
		wSum += w[i]
	}
	beta := make([]float64, cols)
	py := clamp(ySum/wSum, 1e-4, 1-1e-4)
	beta[d] = math.Log(py / (1 - py))
	var r sparseRow
	eta := func(b []float64) float64 {
		e := 0.0
		for k, j := range r.idx {
			e += b[j] * r.val[k]
		}
		return e
	}
	loss := func(b []float64) float64 {
		total := 0.0
		for i := range xs {
			l.row(&xs[i], mean, std, cont, &r)
			e := eta(b)
			sp := math.Max(e, 0) + math.Log1p(math.Exp(-math.Abs(e))) // log(1+e^eta), stably
			total += w[i] * (sp - y[i]*e)
		}
		for j := 0; j < d; j++ {
			total += 0.5 * rankL2 * b[j] * b[j]
		}
		return total
	}
	g := make([]float64, cols)
	h := make([]float64, cols*cols)
	next := make([]float64, cols)
	cur := loss(beta)
	for iter := 0; iter < rankMaxIters; iter++ {
		clear(g)
		clear(h)
		for i := range xs {
			l.row(&xs[i], mean, std, cont, &r)
			p := sigmoid(eta(beta))
			gr := w[i] * (p - y[i])
			s := w[i] * p * (1 - p)
			for a, ja := range r.idx {
				va := r.val[a]
				g[ja] += gr * va
				sv := s * va
				hj := h[ja*cols:]
				for b, jb := range r.idx {
					hj[jb] += sv * r.val[b]
				}
			}
		}
		for j := 0; j < d; j++ {
			g[j] += rankL2 * beta[j]
			h[j*cols+j] += rankL2
		}
		h[d*cols+d] += 1e-6
		step, ok := solveCholesky(h, g, cols)
		if !ok {
			break
		}
		// Step halving keeps Newton monotone.
		accepted := false
		for t := 1.0; t > 1e-4; t /= 2 {
			for j := range next {
				next[j] = beta[j] - t*step[j]
			}
			if lv := loss(next); lv <= cur+1e-12 {
				cur = lv
				accepted = true
				break
			}
		}
		if !accepted {
			break
		}
		maxStep := 0.0
		for j := range beta {
			maxStep = math.Max(maxStep, math.Abs(next[j]-beta[j]))
		}
		copy(beta, next)
		if maxStep < rankConvergeStep {
			break
		}
	}
	return &rankModel{Names: append([]string(nil), names...), Mean: mean, Std: std,
		Weights: append([]float64(nil), beta[:d]...), Bias: beta[d] - math.Log(wPos/wNeg)}
}

// solveCholesky solves A x = b for a symmetric positive definite A (n×n,
// row-major).
func solveCholesky(a, b []float64, n int) ([]float64, bool) {
	l := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			s := a[i*n+j]
			for k := 0; k < j; k++ {
				s -= l[i*n+k] * l[j*n+k]
			}
			if i == j {
				if s <= 0 {
					return nil, false
				}
				l[i*n+i] = math.Sqrt(s)
			} else {
				l[i*n+j] = s / l[j*n+j]
			}
		}
	}
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= l[i*n+k] * y[k]
		}
		y[i] = s / l[i*n+i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := y[i]
		for k := i + 1; k < n; k++ {
			s -= l[k*n+i] * x[k]
		}
		x[i] = s / l[i*n+i]
	}
	return x, true
}

// auc is the area under the ROC curve (Mann-Whitney, ties averaged); 0
// when either class is empty.
func auc(scores []float64, positive []bool) float64 {
	n := len(scores)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return scores[order[i]] < scores[order[j]] })
	var np, nn, rankSum float64
	for i := 0; i < n; {
		j := i
		for j < n && scores[order[j]] == scores[order[i]] {
			j++
		}
		avg := float64(i+j+1) / 2 // 1-based average rank of the tie group
		for k := i; k < j; k++ {
			if positive[order[k]] {
				rankSum += avg
				np++
			} else {
				nn++
			}
		}
		i = j
	}
	if np == 0 || nn == 0 {
		return 0
	}
	return (rankSum - np*(np+1)/2) / (np * nn)
}

// ---- data and persistence ---------------------------------------------------------

// loadExamples reads the most recent labelled serve items, oldest first.
func loadExamples(ctx context.Context, db *sql.DB, limit int) ([]rankExample, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.first_at, i.user_id, s.surface, i.kind, i.row_pos, i.item_pos, i.baseline, i.fv, i.features,
		i.label, COALESCE(p.completed, 0), COALESCE(p.abandoned, 0), COALESCE(p.max_fraction, 0), p.id IS NOT NULL
		FROM personalize_serve_items i JOIN personalize_serves s ON s.id = i.serve_id
		LEFT JOIN personalize_plays p ON p.id = i.play_id AND i.label = 1
		WHERE i.label IN (0, 1)
		ORDER BY s.first_at DESC, i.serve_id DESC, i.row_pos, i.item_pos LIMIT ?`, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []rankExample
	total := 0
	strs := map[string]string{}
	intern := func(v string) string {
		if s, ok := strs[v]; ok {
			return s
		}
		strs[v] = v
		return v
	}
	for rows.Next() {
		var x rankExample
		var at int64
		var fv, label, completed, abandoned int
		var maxFrac float64
		var hasPlay bool
		var blob []byte
		if err := rows.Scan(&at, &x.user, &x.surface, &x.kind, &x.rowPos, &x.itemPos, &x.baseline, &fv, &blob,
			&label, &completed, &abandoned, &maxFrac, &hasPlay); err != nil {
			return nil, 0, err
		}
		total++
		if !decodeSnapshot(fv, blob, &x.raw) {
			continue
		}
		x.kind, x.surface = intern(x.kind), intern(x.surface)
		x.at = fromMS(at)
		if label == labelPlayed {
			x.y = 0.6
			if hasPlay {
				x.y = playRecord{completed: completed != 0, abandoned: abandoned != 0, maxFraction: maxFrac}.reward()
			}
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, total, nil
}

func countLabels(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_serve_items WHERE label IN (0, 1)`).Scan(&n)
	return n, err
}

func saveRankModel(ctx context.Context, db *sql.DB, m *rankModel) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO personalize_ranker (id, trained_at, model) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET trained_at = excluded.trained_at, model = excluded.model`, ms(m.TrainedAt), string(raw))
	return err
}

func loadRankModel(ctx context.Context, db *sql.DB) (*rankModel, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT model FROM personalize_ranker WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m rankModel
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("ranker model: %w", err)
	}
	return &m, nil
}

// TrainRanker labels closed serves, retrains the learned ranker on the
// serve log now and installs the result (active only when it passed the
// gate). It returns the new insight.
func (e *Engine) TrainRanker(ctx context.Context) (core.RankerInsight, error) {
	rs := e.rk
	if rs == nil {
		return core.RankerInsight{Features: []core.RankerFeature{}}, nil
	}
	rs.trainMu.Lock()
	defer rs.trainMu.Unlock()
	rs.writer.flush(ctx)
	now := e.now()
	if _, err := finalizeNegatives(ctx, rs.db, now); err != nil {
		return core.RankerInsight{}, err
	}
	labels, err := countLabels(ctx, rs.db)
	if err != nil {
		return core.RankerInsight{}, err
	}
	xs, _, err := loadExamples(ctx, rs.db, rankMaxExamples)
	if err != nil {
		return core.RankerInsight{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.RankerInsight{}, err
	}
	m := fitRanker(xs, rs.opts, now)
	rs.lastLabels = labels
	if m == nil {
		return e.RankerInsight(ctx)
	}
	m.Labels = labels
	if err := saveRankModel(ctx, rs.db, m); err != nil {
		return core.RankerInsight{}, err
	}
	rs.model.Store(m)
	e.log.Info("personalize: ranker trained", "examples", m.Examples, "positives", m.Positives,
		"auc", round3(m.AUC), "baselineAuc", round3(m.BaselineAUC), "active", m.Active, "blend", round3(m.Blend))
	return e.RankerInsight(ctx)
}
