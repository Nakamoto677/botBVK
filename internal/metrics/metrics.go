// Package metrics collects bounded, lock-free-enough timing summaries for the
// stages that decide how fast the bot reacts: rate-limiter waiting, Telegram HTTP
// calls and whole outbox operations. It deliberately keeps only fixed bounds per
// unit, so memory never grows with traffic and no unbounded label can leak.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Histogram bounds in milliseconds for waiting and for a single HTTP call, and in
// whole seconds for one outbox operation. The interesting region is well below a
// second for calls and a few seconds for operations, so the bounds are denser there.
var (
	msBounds = []int64{5, 10, 25, 50, 100, 250, 500, 1000, 2000, 3100, 5000}
	sBounds  = []int64{1, 2, 3, 5, 8, 10, 15, 20, 30, 60, 120}
)

type series struct {
	bounds []int64
	counts []int64
	total  int64
	max    int64
}

func newSeries(bounds []int64) *series {
	return &series{bounds: bounds, counts: make([]int64, len(bounds)+1)}
}

func (s *series) add(value int64) {
	if value < 0 {
		value = 0
	}
	index := sort.Search(len(s.bounds), func(i int) bool { return value <= s.bounds[i] })
	s.counts[index]++
	s.total += value
	if value > s.max {
		s.max = value
	}
}

type group struct {
	unit   string
	bounds []int64
	series map[string]*series
}

// Metrics is safe for concurrent use: several outbox workers and the poller all
// record into it at the same time. It never allocates per observation beyond the
// first call for a label.
type Metrics struct {
	mu     sync.Mutex
	groups map[string]*group
}

func New() *Metrics {
	return &Metrics{groups: map[string]*group{}}
}

func (m *Metrics) group(name, unit string, bounds []int64) *group {
	g, ok := m.groups[name]
	if !ok {
		g = &group{unit: unit, bounds: bounds, series: map[string]*series{}}
		m.groups[name] = g
	}
	return g
}

func (m *Metrics) record(groupName, unit string, bounds []int64, label string, value int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.group(groupName, unit, bounds)
	s, ok := g.series[label]
	if !ok {
		s = newSeries(bounds)
		g.series[label] = s
	}
	s.add(value)
}

// ObserveMilliseconds records one duration in milliseconds, for example the wait
// imposed by the per-chat rate limiter.
func (m *Metrics) ObserveMilliseconds(name, label string, d time.Duration) {
	m.record(name, "ms", msBounds, label, d.Milliseconds())
}

// ObserveCall records one Telegram HTTP call, labelled by API method.
func (m *Metrics) ObserveCall(method string, d time.Duration) {
	m.record("bvk_telegram_call", "ms", msBounds, method, d.Milliseconds())
}

// ObserveOperation records one whole outbox operation, labelled by operation kind.
func (m *Metrics) ObserveOperation(kind string, d time.Duration) {
	m.record("bvk_operation", "s", sBounds, kind, int64(d.Seconds()))
}

// Render writes the Prometheus text exposition format. Buckets are cumulative, so
// each line counts observations at or below its bound. Reading is rare, so the
// format is built directly instead of cached.
func (m *Metrics) Render() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.groups))
	for name := range m.groups {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		g := m.groups[name]
		labels := make([]string, 0, len(g.series))
		for label := range g.series {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			s := g.series[label]
			cumulative := int64(0)
			for i, bound := range s.bounds {
				cumulative += s.counts[i]
				fmt.Fprintf(&b, "%s_%s_bucket{label=%q,le=%q} %d\n", name, g.unit, label, fmt.Sprint(bound), cumulative)
			}
			cumulative += s.counts[len(s.bounds)]
			fmt.Fprintf(&b, "%s_%s_bucket{label=%q,le=\"+Inf\"} %d\n", name, g.unit, label, cumulative)
			mean := int64(0)
			if cumulative > 0 {
				mean = s.total / cumulative
			}
			fmt.Fprintf(&b, "%s_%s_count{label=%q} %d\n", name, g.unit, label, cumulative)
			fmt.Fprintf(&b, "%s_%s_sum{label=%q} %d\n", name, g.unit, label, s.total)
			fmt.Fprintf(&b, "%s_%s_max{label=%q} %d\n", name, g.unit, label, s.max)
			fmt.Fprintf(&b, "%s_%s_mean{label=%q} %d\n", name, g.unit, label, mean)
		}
	}
	return b.String()
}
