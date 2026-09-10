package monitoring

import (
	"github.com/Real-kia/payesh/internal/collector"
	"github.com/Real-kia/payesh/internal/contracts"
)

// These aliases keep package-03 callers source-compatible while the agent
// imports internal/collector directly and therefore does not pull in SQLite.
const (
	DefaultServerID = collector.DefaultServerID
	DefaultEpoch    = collector.DefaultEpoch
)

type Collector = collector.Collector

func NewCollector(root string, serverID contracts.ServerID, epoch contracts.CollectorEpoch) *Collector {
	return collector.NewCollector(root, serverID, epoch)
}
