package traffic

import (
	"sort"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// trafficObservations converts the durable collector samples into the small
// counter series consumed by Forecast. It deliberately marks sequence/epoch
// discontinuities and invalid selected counters as coverage breaks instead of
// bridging them as zero traffic.
func trafficObservations(samples []contracts.MetricSample, allowance contracts.TrafficAllowance) []contracts.TrafficObservation {
	ordered := append([]contracts.MetricSample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			if ordered[i].CollectorEpoch == ordered[j].CollectorEpoch {
				return ordered[i].Sequence < ordered[j].Sequence
			}
			return ordered[i].CollectorEpoch < ordered[j].CollectorEpoch
		}
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})
	result := make([]contracts.TrafficObservation, 0, len(ordered))
	for index, sample := range ordered {
		observation := trafficObservation(sample, allowance)
		if index > 0 {
			previous := ordered[index-1]
			contiguous := previous.CollectorEpoch == sample.CollectorEpoch && previous.Sequence != ^uint64(0) && sample.Sequence == previous.Sequence+1
			if !contiguous {
				observation.Valid = false
				observation.Coverage = 0
			}
		}
		result = append(result, observation)
	}
	return result
}

// trafficObservationsForPeriods converts samples with the interface snapshot
// belonging to the period active at each sample's timestamp. The current
// allowance is intentionally not consulted here: an owner can change the
// selected interfaces while historical periods retain their original set.
// A sample at an interface transition is not a usable bridge between the two
// counter series, because the counters do not identify the hand-off baseline.
func trafficObservationsForPeriods(samples []contracts.MetricSample, periods []contracts.TrafficPeriod) []contracts.TrafficObservation {
	ordered := append([]contracts.MetricSample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			if ordered[i].CollectorEpoch == ordered[j].CollectorEpoch {
				return ordered[i].Sequence < ordered[j].Sequence
			}
			return ordered[i].CollectorEpoch < ordered[j].CollectorEpoch
		}
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})

	// Explicit periods may overlap their predecessor for a hand-off. The
	// latest period start is authoritative at a timestamp, matching the
	// durable active-period lookup used by ingestion.
	orderedPeriods := append([]contracts.TrafficPeriod(nil), periods...)
	sort.SliceStable(orderedPeriods, func(i, j int) bool {
		if orderedPeriods[i].From.Equal(orderedPeriods[j].From) {
			return orderedPeriods[i].To.Before(orderedPeriods[j].To)
		}
		return orderedPeriods[i].From.Before(orderedPeriods[j].From)
	})
	periodAt := func(at time.Time) (contracts.TrafficPeriod, bool) {
		var active contracts.TrafficPeriod
		found := false
		for _, period := range orderedPeriods {
			if at.Before(period.From) || !at.Before(period.To) {
				continue
			}
			if !found || period.From.After(active.From) {
				active, found = period, true
			}
		}
		return active, found
	}

	result := make([]contracts.TrafficObservation, 0, len(ordered))
	var previousPeriod contracts.TrafficPeriod
	previousPeriodFound := false
	for index, sample := range ordered {
		period, periodFound := periodAt(sample.ObservedAt.UTC())
		var observation contracts.TrafficObservation
		if periodFound {
			// trafficObservation only needs the selected counters and direction;
			// these values come from the historical period snapshot.
			periodAllowance := contracts.TrafficAllowance{Interfaces: append([]string(nil), period.Interfaces...), Direction: period.Direction}
			observation = trafficObservation(sample, periodAllowance)
			if previousPeriodFound && (!sameInterfaces(previousPeriod.Interfaces, period.Interfaces) || previousPeriod.Direction != period.Direction) {
				observation.Valid = false
				observation.Coverage = 0
			}
			previousPeriod = period
			previousPeriodFound = true
		} else {
			observation = contracts.TrafficObservation{ObservedAt: sample.ObservedAt.UTC(), Valid: false, Coverage: 0}
			previousPeriodFound = false
		}
		if index > 0 {
			previous := ordered[index-1]
			contiguous := previous.CollectorEpoch == sample.CollectorEpoch && previous.Sequence != ^uint64(0) && sample.Sequence == previous.Sequence+1
			if !contiguous {
				observation.Valid = false
				observation.Coverage = 0
			}
		}
		result = append(result, observation)
	}
	return result
}

func trafficObservation(sample contracts.MetricSample, allowance contracts.TrafficAllowance) contracts.TrafficObservation {
	observation := contracts.TrafficObservation{ObservedAt: sample.ObservedAt.UTC(), Valid: true, Coverage: 1}
	if sample.ObservedAt.IsZero() || (sample.TimestampUncertainty != "" && sample.TimestampUncertainty != "none") {
		observation.Valid = false
		observation.Coverage = 0
		return observation
	}
	keys := allowance.Interfaces
	if len(keys) == 0 {
		keys = []string{"billing"}
	}
	var inbound, outbound uint64
	for _, iface := range keys {
		prefix := "net.billing."
		if iface != "billing" {
			prefix = "net." + sanitizeInterface(iface) + "."
		}
		for _, direction := range []string{"rx_bytes", "tx_bytes"} {
			if allowance.Direction == "inbound" && direction != "rx_bytes" || allowance.Direction == "outbound" && direction != "tx_bytes" {
				continue
			}
			key := prefix + direction
			encoded, present := sample.Counters[key]
			if !present || sample.Validity[key] != "valid" {
				observation.Valid = false
				observation.Coverage = 0
				return observation
			}
			value, err := parseCanonicalUint64(encoded)
			if err != nil {
				observation.Valid = false
				observation.Coverage = 0
				return observation
			}
			if direction == "rx_bytes" {
				var ok bool
				inbound, ok = addUint64(inbound, value)
				if !ok {
					observation.Valid = false
					observation.Coverage = 0
					return observation
				}
			} else {
				var ok bool
				outbound, ok = addUint64(outbound, value)
				if !ok {
					observation.Valid = false
					observation.Coverage = 0
					return observation
				}
			}
		}
	}
	observation.InboundBytes, observation.OutboundBytes = inbound, outbound
	return observation
}
