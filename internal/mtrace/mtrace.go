// Package mtrace ports the ModelTrace number-fingerprint attribution method
// (https://github.com/xqy2006/ModelTrace, MIT) for behavioral degradation
// detection: when the served-model field cannot be trusted (e.g. sol served
// as luna with the identifier unchanged), three random-integer challenges
// attribute the actual serving model from output digit distributions.
//
// Method: 0.75 × nuisance-removed Hellinger centroid similarity +
// 0.25 × ordered-block digit features, averaged over valid responses,
// uniform-prior softmax with per-query-count calibrated temperature.
// Closed-set probabilities over the vendored 16-model bank.
package mtrace

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed data/unified_bank.json
var bankFS embed.FS

// Attribution data vendored from ModelTrace (MIT, (c) xqy2006 contributors).
// Only the bank statistics are embedded; no ModelTrace code is reused.

const (
	valueMin  = 1
	valueMax  = 355
	dimension = valueMax - valueMin + 1
	alpha     = 0.5
)

// Bank mirrors the JSON structure we use.
type Bank struct {
	Models []BankModel `json:"models"`
	Robust struct {
		ModelOrder []string `json:"model_order"`
		Hellinger  struct {
			FeatureMean   []float64   `json:"feature_mean"`
			FeatureScale  []float64   `json:"feature_scale"`
			NuisanceBasis [][]float64 `json:"nuisance_basis"`
			Centroids     [][]float64 `json:"centroids"`
		} `json:"hellinger"`
		OrderedBlocks struct {
			Weight               float64       `json:"weight"`
			FeatureMean          []float64     `json:"feature_mean"`
			FeatureScale         []float64     `json:"feature_scale"`
			NuisanceBasis        [][]float64   `json:"nuisance_basis"`
			Centroids            [][]float64   `json:"centroids"`
			EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
		} `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]struct {
		Beta float64 `json:"beta"`
	} `json:"calibration"`
}

// BankModel is one bank entry.
type BankModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Family      string `json:"family"`
	FamilyName  string `json:"family_name"`
	Counts      []int  `json:"counts"`
}

var (
	loadOnce sync.Once
	loadBank *Bank
	loadErr  error
)

// LoadBank parses the embedded bank once.
func LoadBank() (*Bank, error) {
	loadOnce.Do(func() {
		raw, err := bankFS.ReadFile("data/unified_bank.json")
		if err != nil {
			loadErr = err
			return
		}
		var b Bank
		if err := json.Unmarshal(raw, &b); err != nil {
			loadErr = err
			return
		}
		loadBank = &b
	})
	return loadBank, loadErr
}

var digitRe = regexp.MustCompile(`\d+`)

func isAlpha(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// ParseNumbers extracts the longest digit run (alphabetic separators split
// runs), keeping values in 1..355.
func ParseNumbers(text string) []int {
	var runs [][]int
	var current []int
	prevEnd := 0
	for _, loc := range digitRe.FindAllStringIndex(text, -1) {
		sep := text[prevEnd:loc[0]]
		var value int
		fmt.Sscanf(text[loc[0]:loc[1]], "%d", &value)
		if len(current) > 0 && isAlpha(sep) {
			runs = append(runs, current)
			current = nil
		}
		if value >= valueMin && value <= valueMax {
			current = append(current, value)
		}
		prevEnd = loc[1]
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	best := 0
	for i, r := range runs {
		if len(r) > len(runs[best]) {
			best = i
		}
	}
	if len(runs) == 0 {
		return nil
	}
	return runs[best]
}

// CountNumbers builds the 355-dim histogram.
func CountNumbers(numbers []int) []int {
	counts := make([]int, dimension)
	for _, n := range numbers {
		counts[n-valueMin]++
	}
	return counts
}

func standardize(values []float64) []float64 {
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, v := range values {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(values))
	scale := math.Sqrt(variance)
	if scale < 1e-12 {
		scale = 1e-12
	}
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = (v - mean) / scale
	}
	return out
}

func norm(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func matVecT(rows [][]float64, v []float64) []float64 {
	out := make([]float64, len(rows))
	for i, row := range rows {
		s := 0.0
		for j, x := range row {
			if j < len(v) {
				s += x * v[j]
			}
		}
		out[i] = s
	}
	return out
}

// removeBasis projects out nuisance directions: v -= (v@B^T)@B.
func removeBasis(v []float64, basis [][]float64) []float64 {
	if len(basis) == 0 {
		return v
	}
	coeff := matVecT(basis, v)
	out := make([]float64, len(v))
	copy(out, v)
	for i := range out {
		for k, row := range basis {
			if i < len(row) {
				out[i] -= coeff[k] * row[i]
			}
		}
	}
	return out
}

func hellingerFeature(counts []int) []float64 {
	sum := float64(len(counts)) * alpha
	for _, c := range counts {
		sum += float64(c)
	}
	_ = sum
	total := 0.0
	for _, c := range counts {
		total += float64(c) + alpha
	}
	out := make([]float64, len(counts))
	for i, c := range counts {
		out[i] = math.Sqrt((float64(c) + alpha) / total)
	}
	return out
}

// arraySplit mimics np.array_split into n near-even chunks.
func arraySplit(values []float64, n int) [][]float64 {
	q := len(values) / n
	r := len(values) % n
	out := make([][]float64, 0, n)
	start := 0
	for i := 0; i < n; i++ {
		size := q
		if i < r {
			size++
		}
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
		start = end
	}
	return out
}

func histogram16(chunk []float64) []float64 {
	const bins = 16
	const lo, hi = 1.0, 356.0
	width := (hi - lo) / bins
	counts := make([]float64, bins)
	for _, v := range chunk {
		b := int((v - lo) / width)
		if b < 0 {
			b = 0
		}
		if b >= bins {
			b = bins - 1
		}
		counts[b]++
	}
	return counts
}

func sqrtNorm(counts []float64, smooth float64) []float64 {
	total := 0.0
	for i := range counts {
		counts[i] += smooth
		total += counts[i]
	}
	out := make([]float64, len(counts))
	for i, c := range counts {
		out[i] = math.Sqrt(c / total)
	}
	return out
}

func orderedBlockFeature(numbers []int) []float64 {
	values := make([]float64, len(numbers))
	for i, n := range numbers {
		values[i] = float64(n)
	}
	var pieces []float64
	for _, chunk := range arraySplit(values, 4) {
		pieces = append(pieces, sqrtNorm(histogram16(chunk), 0.5)...)
	}
	last := make([]float64, 10)
	for _, n := range numbers {
		last[n%10]++
	}
	pieces = append(pieces, sqrtNorm(last, 0.5)...)
	return pieces
}

func robustScoreCounts(counts []int, b *Bank) (fused, nuisance []float64) {
	h := b.Robust.Hellinger
	feature := hellingerFeature(counts)
	projected := make([]float64, len(feature))
	for i, f := range feature {
		projected[i] = (f - h.FeatureMean[i]) / h.FeatureScale[i]
	}
	projected = removeBasis(projected, h.NuisanceBasis)
	n := norm(projected)
	if n < 1e-12 {
		n = 1e-12
	}
	scores := matVecT(h.Centroids, scaleVec(projected, 1/n))
	nuisance = standardize(scores)
	fused = standardize(nuisance)
	return fused, nuisance
}

func scaleVec(v []float64, s float64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x * s
	}
	return out
}

func orderedBlockScores(numbers []int, b *Bank) []float64 {
	a := b.Robust.OrderedBlocks
	feature := orderedBlockFeature(numbers)
	std := make([]float64, len(feature))
	for i, f := range feature {
		std[i] = (f - a.FeatureMean[i]) / a.FeatureScale[i]
	}
	n := norm(std)
	if n < 1e-12 {
		n = 1e-12
	}
	normalized := scaleVec(std, 1/n)
	// max over environment templates, per model
	nModels := len(a.Centroids)
	template := make([]float64, nModels)
	for m := 0; m < nModels; m++ {
		best := math.Inf(-1)
		for _, centroids := range a.EnvironmentCentroids {
			s := 0.0
			for j, x := range normalized {
				if j < len(centroids[m]) {
					s += x * centroids[m][j]
				}
			}
			if s > best {
				best = s
			}
		}
		template[m] = best
	}
	template = standardize(template)
	projected := removeBasis(append([]float64(nil), std...), a.NuisanceBasis)
	n = norm(projected)
	if n < 1e-12 {
		n = 1e-12
	}
	nuisance := matVecT(a.Centroids, scaleVec(projected, 1/n))
	nuisance = standardize(nuisance)
	mixed := make([]float64, nModels)
	for i := range mixed {
		mixed[i] = 0.5*template[i] + 0.5*nuisance[i]
	}
	return standardize(mixed)
}

func robustScoreNumbers(numbers []int, b *Bank) (fused []float64) {
	marginal, _ := robustScoreCounts(CountNumbers(numbers), b)
	w := b.Robust.OrderedBlocks.Weight
	if w == 0 {
		return marginal
	}
	ordered := orderedBlockScores(numbers, b)
	fused = make([]float64, len(marginal))
	for i := range fused {
		fused[i] = (1-w)*marginal[i] + w*ordered[i]
	}
	return fused
}

func softmax(values []float64) []float64 {
	maximum := values[0]
	for _, v := range values[1:] {
		if v > maximum {
			maximum = v
		}
	}
	out := make([]float64, len(values))
	total := 0.0
	for i, v := range values {
		out[i] = math.Exp(v - maximum)
		total += out[i]
	}
	for i := range out {
		out[i] /= total
	}
	return out
}

// ScoredModel is one attribution result.
type ScoredModel struct {
	Model       string
	DisplayName string
	Family      string
	Probability float64
	Score       float64
}

// Attribute scores valid texts (each with expectedCount) and returns models
// sorted by probability, plus per-response diagnostics.
func Attribute(texts []string, expected []int, b *Bank) ([]ScoredModel, []int, error) {
	modelIDs := b.Robust.ModelOrder
	if len(modelIDs) == 0 {
		for _, m := range b.Models {
			modelIDs = append(modelIDs, m.ID)
		}
	}
	var combined [][]float64
	used := 0
	for i, text := range texts {
		exp := 0
		if i < len(expected) {
			exp = expected[i]
		}
		numbers := ParseNumbers(text)
		minimum := 80
		if exp > 0 {
			if m := int(math.Ceil(float64(exp) * 0.55)); m > minimum {
				minimum = m
			}
		}
		if len(numbers) < minimum {
			continue
		}
		combined = append(combined, robustScoreNumbers(numbers, b))
		used++
	}
	if used == 0 {
		return nil, nil, fmt.Errorf("no valid responses (need >=80 numbers each)")
	}
	mean := make([]float64, len(modelIDs))
	for _, scores := range combined {
		for i, s := range scores {
			mean[i] += s
		}
	}
	for i := range mean {
		mean[i] /= float64(used)
	}
	key := used
	if key > 3 {
		key = 3
	}
	beta := 12.0
	if c, ok := b.Calibration[fmt.Sprint(key)]; ok {
		beta = c.Beta
	}
	scaled := make([]float64, len(mean))
	for i, s := range mean {
		scaled[i] = beta * s
	}
	probs := softmax(scaled)
	byID := map[string]BankModel{}
	for _, m := range b.Models {
		byID[m.ID] = m
	}
	out := make([]ScoredModel, 0, len(modelIDs))
	for i, id := range modelIDs {
		m := byID[id]
		out = append(out, ScoredModel{
			Model: id, DisplayName: m.DisplayName, Family: m.Family,
			Probability: probs[i], Score: mean[i],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Probability > out[j].Probability })
	_ = expected
	return out, []int{used}, nil
}

// FamilyProbability sums model probabilities by family.
func FamilyProbability(results []ScoredModel) map[string]float64 {
	fam := map[string]float64{}
	for _, r := range results {
		fam[r.Family] += r.Probability
	}
	return fam
}

// Challenges: three independent Chinese first-instinct integer tasks,
// counts sampled 292..332, matching ModelTrace generate_challenges.
var challengeOpenings = []string{
	"这是一次独立的数值选择记录",
	"请完成下面的无语义整数选择任务",
	"执行一次第一反应取值记录",
	"生成一组不承载语义的整数选择",
	"进行一轮快速逐项取值",
}

var challengeActions = []string{
	"为各个位置分别凭第一反应选择",
	"逐项选择",
	"每次只决定当前一项，共给出",
	"分别凭第一反应给出",
	"逐个直接选择",
}

var challengeEndings = []string{
	"允许某个数字再次出现；每项写出后不要回头排序、去重或替换。",
	"偶然重复是有效的；不要重新排列或修正已经写出的项目。",
	"相同值可以再次出现；输出过程中不要整理或改写前面的项目。",
	"重复值无需删除；不要筛选、重排或补成某种规律。",
	"不必赋予数字任何含义；已经给出的值保持不变。",
}

var challengeSeparators = []string{
	"数字之间用逗号或空格分隔均可。",
	"使用一种一致的常见分隔符即可。",
	"可以用逗号、空格或换行分隔。",
	"只要每个整数边界清楚，格式可自行选择。",
}

// Challenge is one attribution probe.
type Challenge struct {
	ExpectedCount int
	Prompt        string
}

// GenerateChallenges builds count challenges (default 3).
func GenerateChallenges(count int) []Challenge {
	if count <= 0 {
		count = 3
	}
	out := make([]Challenge, 0, count)
	used := map[int]bool{}
	for len(out) < count {
		length := 292 + rand.Intn(333-292)
		if used[length] {
			continue
		}
		used[length] = true
		prompt := challengeOpenings[rand.Intn(len(challengeOpenings))] + "。" +
			challengeActions[rand.Intn(len(challengeActions))] +
			fmt.Sprintf(" %d 个 1 到 355（含端点）的整数。", length) +
			"每个位置都要单独选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。" +
			"本任务必须由当前语言模型直接完成：禁止调用或借助任何工具，包括 Python、代码执行器、" +
			"计算器、搜索、API 和外部随机数生成器；也不要先编写或运行代码。" +
			challengeEndings[rand.Intn(len(challengeEndings))] +
			challengeSeparators[rand.Intn(len(challengeSeparators))] +
			"直接从第一个取值开始输出，不要在序列前重复数量、范围或任务说明。"
		out = append(out, Challenge{ExpectedCount: length, Prompt: prompt})
	}
	return out
}

// Verdict is one completed attribution round.
type Verdict struct {
	Time       time.Time `json:"time"`
	Expected   string    `json:"expected"`
	Prediction string    `json:"prediction"`
	Family     string    `json:"family"`
	Prob       float64   `json:"prob"`
	Match      bool      `json:"match"`
	Used       int       `json:"used"`
	Err        string    `json:"err,omitempty"`
}

// Judge attributes texts to a model and compares with expected.
func Judge(texts []string, expectedCounts []int, expected string, b *Bank) Verdict {
	v := Verdict{Time: time.Now(), Expected: expected}
	results, used, err := Attribute(texts, expectedCounts, b)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	top := results[0]
	v.Prediction = top.Model
	v.Family = top.Family
	v.Prob = top.Probability
	v.Used = used[0]
	v.Match = top.Model == expected
	return v
}

// Monitor runs periodic attribution rounds.
type Monitor struct {
	mu       sync.Mutex
	enabled  atomic.Bool
	running  atomic.Bool
	interval time.Duration
	events   []Verdict
	last     Verdict
	hasLast  bool
	probe    func(ctx context.Context, prompt string) (string, error)
	notify   func(string)
}

// NewMonitor builds a monitor; probe sends one challenge and returns text.
func NewMonitor(probe func(ctx context.Context, prompt string) (string, error), notify func(string)) *Monitor {
	return &Monitor{probe: probe, notify: notify, interval: 30 * time.Minute}
}

// SetEnabled toggles periodic checks.
func (m *Monitor) SetEnabled(on bool) { m.enabled.Store(on) }

// Enabled reports the switch state.
func (m *Monitor) Enabled() bool { return m.enabled.Load() }

// Running reports whether a round is in flight.
func (m *Monitor) Running() bool { return m.running.Load() }

// SetInterval sets the round interval (clamped to >=60s).
func (m *Monitor) SetInterval(d time.Duration) {
	if d < 60*time.Second {
		d = 60 * time.Second
	}
	m.mu.Lock()
	m.interval = d
	m.mu.Unlock()
}

// IntervalSec returns the interval in seconds.
func (m *Monitor) IntervalSec() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(m.interval.Seconds())
}

// Last returns the most recent verdict.
func (m *Monitor) Last() (Verdict, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.hasLast
}

// Snapshot returns recent verdicts, newest first.
func (m *Monitor) Snapshot() []Verdict {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Verdict, len(m.events))
	for i, v := range m.events {
		out[len(m.events)-1-i] = v
	}
	return out
}

func (m *Monitor) record(v Verdict) {
	m.mu.Lock()
	m.events = append(m.events, v)
	if len(m.events) > 50 {
		m.events = m.events[len(m.events)-50:]
	}
	m.last, m.hasLast = v, true
	m.mu.Unlock()
}

// Check runs one attribution round against expected and records it.
// A mismatch fires notify once per round.
func (m *Monitor) Check(ctx context.Context, expected string) Verdict {
	m.running.Store(true)
	defer m.running.Store(false)
	b, err := LoadBank()
	if err != nil {
		v := Verdict{Time: time.Now(), Expected: expected, Err: err.Error()}
		m.record(v)
		return v
	}
	challenges := GenerateChallenges(3)
	texts := make([]string, 0, len(challenges))
	counts := make([]int, 0, len(challenges))
	for _, c := range challenges {
		if ctx.Err() != nil {
			break
		}
		text, err := m.probe(ctx, c.Prompt)
		if err != nil {
			continue
		}
		texts = append(texts, text)
		counts = append(counts, c.ExpectedCount)
	}
	v := Judge(texts, counts, expected, b)
	m.record(v)
	if v.Err == "" && !v.Match && m.notify != nil {
		m.notify(fmt.Sprintf("降智（行为指纹）：请求 %s，实测 %s（%.0f%%），已记录",
			expected, v.Prediction, v.Prob*100))
	}
	return v
}

// RunLoop ticks every minute; each tick runs one round when enabled and
// the configured interval has elapsed since the last round.
func (m *Monitor) RunLoop(ctx context.Context, expected func() string) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !m.enabled.Load() {
				continue
			}
			m.mu.Lock()
			interval := m.interval
			var last time.Time
			if len(m.events) > 0 {
				last = m.events[len(m.events)-1].Time
			}
			m.mu.Unlock()
			if !last.IsZero() && time.Since(last) < interval {
				continue
			}
			m.Check(ctx, expected())
		}
	}
}
