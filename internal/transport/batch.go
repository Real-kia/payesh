package transport

import (
	"errors"

	"github.com/Real-kia/payesh/internal/contracts"
)

// BatchEpoch returns the collector epoch a sample batch belongs to.
func BatchEpoch(batch contracts.SampleBatch) contracts.CollectorEpoch {
	if len(batch.Samples) > 0 {
		return batch.Samples[0].CollectorEpoch
	}
	if len(batch.Gaps) > 0 {
		return batch.Gaps[0].CollectorEpoch
	}
	return ""
}

// ValidateBatchEpoch keeps the acknowledgement unambiguous. ThroughSequence
// has no epoch field, so acknowledging a mixed-epoch batch could advance the
// wrong spool after an agent restart.
func ValidateBatchEpoch(batch contracts.SampleBatch) error {
	var epoch contracts.CollectorEpoch
	for _, sample := range batch.Samples {
		if epoch == "" {
			epoch = sample.CollectorEpoch
		} else if sample.CollectorEpoch != epoch {
			return errors.New("sample batch contains multiple collector epochs")
		}
	}
	for _, gap := range batch.Gaps {
		if epoch == "" {
			epoch = gap.CollectorEpoch
		} else if gap.CollectorEpoch != epoch {
			return errors.New("sample batch contains multiple collector epochs")
		}
	}
	return nil
}

// MaxBootstrapToken bounds the pairing token accepted on the bootstrap channel.
const MaxBootstrapToken = 256

// MigrationCapability is advertised by agents that follow hub-directed endpoint changes.
const MigrationCapability = "transport-migration"
