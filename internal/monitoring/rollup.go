package monitoring

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

type Rollup struct {
	ServerID        contracts.ServerID `json:"server_id"`
	Metric          string             `json:"metric"`
	BucketStart     time.Time          `json:"bucket_start"`
	BucketSeconds   int                `json:"bucket_seconds"`
	SampleCount     int                `json:"sample_count"`
	ObservedSeconds float64            `json:"observed_seconds"`
	Minimum         *float64           `json:"minimum,omitempty"`
	Maximum         *float64           `json:"maximum,omitempty"`
	WeightedMean    *float64           `json:"weighted_mean,omitempty"`
	CounterDelta    string             `json:"counter_delta,omitempty"`
	Coverage        string             `json:"coverage"`
}

func (r Rollup) Validate() error {
	if r.ServerID == "" || r.Metric == "" || r.BucketStart.IsZero() {
		return errors.New("rollup identity is required")
	}
	if r.BucketSeconds != 60 && r.BucketSeconds != 3600 {
		return errors.New("rollup bucket must be minute or hour")
	}
	maxDuration := float64(r.BucketSeconds) + 1e-3
	if r.SampleCount < 1 || r.SampleCount > 1000000 || r.ObservedSeconds < 0 || r.ObservedSeconds > maxDuration || math.IsNaN(r.ObservedSeconds) || math.IsInf(r.ObservedSeconds, 0) {
		return errors.New("invalid rollup sample count or duration")
	}
	if r.Coverage != "complete" && r.Coverage != "gap" && r.Coverage != "uncertain" {
		return errors.New("invalid rollup coverage")
	}
	if r.CounterDelta != "" {
		value, err := strconv.ParseUint(r.CounterDelta, 10, 64)
		if err != nil || strconv.FormatUint(value, 10) != r.CounterDelta {
			return errors.New("invalid rollup counter delta")
		}
	}
	for _, value := range []*float64{r.Minimum, r.Maximum, r.WeightedMean} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return errors.New("rollup values must be finite")
		}
	}
	return nil
}

// BuildRollups aggregates only samples that actually exist. It never creates
// an empty bucket and never turns an unavailable/uncertain value into zero.
func BuildRollups(serverID contracts.ServerID, samples []contracts.MetricSample, bucketSeconds int) []Rollup {
	return BuildRollupsWithGaps(serverID, samples, nil, bucketSeconds)
}

// BuildRollupsWithGaps carries sequence gaps into the bucket's continuity
// state. Gauge durations are clipped at bucket edges. Counter intervals are
// assigned to the bucket containing their ending sample, so an interval that
// crosses a boundary is retained exactly once rather than dropped from both
// buckets. A counter delta may be numerically monotonic and still be uncertain
// when samples were missing between its endpoints.
func BuildRollupsWithGaps(serverID contracts.ServerID, samples []contracts.MetricSample, gaps []contracts.CoverageGap, bucketSeconds int) []Rollup {
	if len(samples) == 0 || (bucketSeconds != 60 && bucketSeconds != 3600) {
		return []Rollup{}
	}
	ordered := append([]contracts.MetricSample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			return ordered[i].Sequence < ordered[j].Sequence
		}
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})
	type bucketKey struct {
		metric string
		start  time.Time
	}
	values := make(map[bucketKey][]point)
	counters := make(map[bucketKey]*counterBucket)
	type counterState struct {
		previous counterPoint
		hasPrev  bool
		missing  bool
	}
	states := make(map[string]counterState)
	bucketDuration := time.Duration(bucketSeconds) * time.Second
	// Gauge values describe the interval until the next observation. Split an
	// interval at an adjacent bucket boundary so the last value in one bucket
	// contributes to the beginning of the next bucket as well. Do not fill
	// arbitrary empty buckets across a long outage: only the immediately next
	// bucket is carried, and its coverage is marked uncertain when continuity is
	// not provable.
	appendGauge := func(index int, metric string, value float64, state string) {
		sample := ordered[index]
		coverage := "complete"
		if state == "stale" || state == "uncertain" || timestampUncertain(sample.TimestampUncertainty) {
			coverage = "uncertain"
		}
		next := sample.ObservedAt
		if index+1 < len(ordered) {
			next = ordered[index+1].ObservedAt
			following := ordered[index+1]
			if following.CollectorEpoch != sample.CollectorEpoch || sequenceGap(sample.Sequence, following.Sequence) || following.TimestampUncertainty != "" && following.TimestampUncertainty != "none" {
				coverage = "uncertain"
			}
			if _, present := following.Values[metric]; !present {
				if _, present := following.Counters[metric]; !present {
					coverage = "uncertain"
				}
			}
			if following.Validity[metric] == "unavailable" || following.Validity[metric] == "stale" || following.Validity[metric] == "uncertain" {
				coverage = "uncertain"
			}
		}
		bucket := sample.ObservedAt.UTC().Truncate(bucketDuration)
		end := bucket.Add(bucketDuration)
		if next.After(end) {
			next = end
		}
		if next.After(sample.ObservedAt) {
			values[bucketKey{metric: metric, start: bucket}] = append(values[bucketKey{metric: metric, start: bucket}], point{value: value, duration: next.Sub(sample.ObservedAt).Seconds(), coverage: coverage, epoch: sample.CollectorEpoch, sequence: sample.Sequence})
		} else if index+1 >= len(ordered) {
			// Keep a final instantaneous observation visible even though it has no
			// following sample from which to derive a duration.
			values[bucketKey{metric: metric, start: bucket}] = append(values[bucketKey{metric: metric, start: bucket}], point{value: value, duration: 0, coverage: coverage, epoch: sample.CollectorEpoch, sequence: sample.Sequence})
		}
		// Carry into exactly one adjacent bucket when the next sample is there.
		// A sample at the boundary belongs to that next bucket already and does
		// not need a zero-duration carry point.
		if index+1 >= len(ordered) || !ordered[index+1].ObservedAt.After(end) || !next.Equal(end) {
			return
		}
		followingBucket := ordered[index+1].ObservedAt.UTC().Truncate(bucketDuration)
		if !followingBucket.Equal(end) {
			return
		}
		carryEnd := ordered[index+1].ObservedAt
		if carryEnd.After(end.Add(bucketDuration)) {
			carryEnd = end.Add(bucketDuration)
			coverage = "uncertain"
		}
		if carryEnd.After(end) {
			values[bucketKey{metric: metric, start: end}] = append(values[bucketKey{metric: metric, start: end}], point{value: value, duration: carryEnd.Sub(end).Seconds(), coverage: coverage, epoch: sample.CollectorEpoch, sequence: sample.Sequence})
		}
	}
	for index, sample := range ordered {
		for metric, value := range sample.Values {
			state := sample.Validity[metric]
			if state == "unavailable" {
				continue
			}
			appendGauge(index, metric, value, state)
		}
		// The node wire format keeps integer-valued gauges in Counters so raw
		// byte/capacity readings retain their exact decimal representation. They
		// are still gauges for aggregation: capacity and current memory values
		// must produce min/max/time-weighted means, never monotonic deltas.
		for metric, raw := range sample.Counters {
			if !isIntegerGaugeMetric(metric) {
				continue
			}
			state := sample.Validity[metric]
			if state == "unavailable" {
				continue
			}
			value, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				continue
			}
			appendGauge(index, metric, float64(value), state)
		}
		// A missing/unavailable counter breaks continuity until the next valid
		// point. We still preserve that next point, but never claim the delta
		// across the missing interval is exact.
		for metric, state := range states {
			if _, present := sample.Counters[metric]; !present || sample.Validity[metric] == "unavailable" {
				state.missing = true
				states[metric] = state
			}
		}
		for metric, raw := range sample.Counters {
			if isIntegerGaugeMetric(metric) {
				continue
			}
			start := sample.ObservedAt.UTC().Truncate(bucketDuration)
			state := states[metric]
			value, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || sample.Validity[metric] == "unavailable" {
				state.missing = true
				states[metric] = state
				continue
			}
			coverage := "complete"
			if sample.Validity[metric] == "stale" || sample.Validity[metric] == "uncertain" || timestampUncertain(sample.TimestampUncertainty) {
				coverage = "uncertain"
			}
			current := counterPoint{value: value, coverage: coverage, epoch: sample.CollectorEpoch, sequence: sample.Sequence, observedAt: sample.ObservedAt}
			key := bucketKey{metric: metric, start: start}
			bucket := counters[key]
			if bucket == nil {
				bucket = &counterBucket{}
				counters[key] = bucket
			}
			bucket.points = append(bucket.points, current)
			if state.hasPrev {
				interval := counterInterval{from: state.previous, to: current}
				if state.missing || state.previous.coverage != "complete" || current.coverage != "complete" || state.previous.epoch != current.epoch || sequenceGap(state.previous.sequence, current.sequence) || current.value < state.previous.value {
					bucket.uncertain = true
				} else {
					interval.delta = current.value - state.previous.value
					bucket.deltas = append(bucket.deltas, interval)
				}
			}
			state.previous, state.hasPrev, state.missing = current, true, false
			states[metric] = state
		}
	}
	result := make([]Rollup, 0, len(values)+len(counters))
	for key, points := range values {
		rollup := Rollup{ServerID: serverID, Metric: key.metric, BucketStart: key.start, BucketSeconds: bucketSeconds, SampleCount: len(points), Coverage: "complete"}
		var weighted, duration float64
		for _, item := range points {
			if rollup.Minimum == nil || item.value < *rollup.Minimum {
				value := item.value
				rollup.Minimum = &value
			}
			if rollup.Maximum == nil || item.value > *rollup.Maximum {
				value := item.value
				rollup.Maximum = &value
			}
			weighted += item.value * item.duration
			duration += item.duration
			if item.coverage != "complete" {
				rollup.Coverage = "uncertain"
			}
		}
		if pointsHaveGap(points, gaps) {
			rollup.Coverage = "uncertain"
		}
		if duration > float64(bucketSeconds) {
			duration = float64(bucketSeconds)
		}
		rollup.ObservedSeconds = duration
		if duration > 0 {
			mean := weighted / duration
			rollup.WeightedMean = &mean
		} else if rollup.Minimum != nil {
			mean := *rollup.Minimum
			rollup.WeightedMean = &mean
		}
		result = append(result, rollup)
	}
	for key, bucket := range counters {
		rollup := Rollup{ServerID: serverID, Metric: key.metric, BucketStart: key.start, BucketSeconds: bucketSeconds, SampleCount: len(bucket.points), Coverage: "complete"}
		var total uint64
		for _, interval := range bucket.deltas {
			if counterIntervalHasGap(interval, gaps) {
				bucket.uncertain = true
				continue
			}
			var ok bool
			total, ok = addRollupCounter(total, interval.delta)
			if !ok {
				bucket.uncertain = true
				break
			}
		}
		if len(bucket.deltas) == 0 || bucket.uncertain {
			rollup.Coverage = "uncertain"
		} else {
			rollup.CounterDelta = strconv.FormatUint(total, 10)
		}
		for _, item := range bucket.points {
			if item.coverage != "complete" {
				rollup.Coverage = "uncertain"
				rollup.CounterDelta = ""
			}
		}
		result = append(result, rollup)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].BucketStart.Equal(result[j].BucketStart) {
			return result[i].Metric < result[j].Metric
		}
		return result[i].BucketStart.Before(result[j].BucketStart)
	})
	return result
}

func timestampUncertain(value string) bool {
	return value != "" && value != "none"
}

func isIntegerGaugeMetric(metric string) bool {
	// These measurements are instantaneous capacities/usage values. They are
	// encoded as decimal strings only to avoid losing precision on the raw wire.
	return strings.HasPrefix(metric, "memory.") || strings.HasPrefix(metric, "disk.root.")
}

func sequenceGap(previous, current uint64) bool {
	if current <= previous {
		return true
	}
	return previous != ^uint64(0) && current != previous+1
}

type point struct {
	value    float64
	duration float64
	coverage string
	epoch    contracts.CollectorEpoch
	sequence uint64
}

type counterPoint struct {
	value      uint64
	coverage   string
	epoch      contracts.CollectorEpoch
	sequence   uint64
	observedAt time.Time
}

type counterBucket struct {
	points    []counterPoint
	deltas    []counterInterval
	uncertain bool
}

type counterInterval struct {
	from  counterPoint
	to    counterPoint
	delta uint64
}

func addRollupCounter(left, right uint64) (uint64, bool) {
	if ^uint64(0)-left < right {
		return 0, false
	}
	return left + right, true
}

func counterIntervalHasGap(interval counterInterval, gaps []contracts.CoverageGap) bool {
	for _, gap := range gaps {
		if gap.CollectorEpoch != interval.to.epoch || interval.from.epoch != interval.to.epoch {
			continue
		}
		// Gaps are half-open [from,to). The interval ending at `to` is
		// uncertain when it crosses any missing sequence in that range.
		if interval.from.sequence < gap.ToSequence && interval.to.sequence >= gap.FromSequence {
			return true
		}
	}
	return false
}

func pointsHaveGap(points []point, gaps []contracts.CoverageGap) bool {
	if len(points) < 2 {
		return false
	}
	for _, gap := range gaps {
		for _, item := range points {
			if item.epoch == gap.CollectorEpoch && item.sequence >= gap.FromSequence && item.sequence < gap.ToSequence {
				return true
			}
			if item.epoch == gap.CollectorEpoch && item.sequence >= gap.ToSequence {
				for _, previous := range points {
					if previous.epoch == item.epoch && previous.sequence < gap.FromSequence {
						return true
					}
				}
			}
		}
	}
	return false
}

func counterPointsHaveGap(points []counterPoint, gaps []contracts.CoverageGap) bool {
	if len(points) < 2 {
		return false
	}
	for _, gap := range gaps {
		var first, last *counterPoint
		for i := range points {
			if points[i].epoch != gap.CollectorEpoch {
				continue
			}
			if points[i].sequence >= gap.FromSequence && points[i].sequence < gap.ToSequence {
				return true
			}
			if first == nil || points[i].sequence < first.sequence {
				first = &points[i]
			}
			if last == nil || points[i].sequence > last.sequence {
				last = &points[i]
			}
		}
		if first != nil && last != nil && first.sequence < gap.FromSequence && last.sequence >= gap.ToSequence {
			return true
		}
	}
	return false
}
