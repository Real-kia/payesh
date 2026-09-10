package contracts

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

// Contract fixtures are examples for later package tests, not production data.
//
//go:embed testdata/*.json
var fixtures embed.FS

func TestMetricSampleKeepsLargeCountersAsStrings(t *testing.T) {
	const total = "9007199254740993"
	sample := MetricSample{
		ServerID:       "server-0123456789",
		CollectorEpoch: "epoch-0123456789",
		Sequence:       1,
		ObservedAt:     time.Unix(0, 0).UTC(),
		ReceivedAt:     time.Unix(0, 0).UTC(),
		Values:         map[string]float64{"cpu.utilization": 0.25},
		Counters:       map[string]string{"net.eth0.rx_bytes": total},
	}
	b, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MetricSample
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Counters["net.eth0.rx_bytes"] != total {
		t.Fatalf("counter changed during JSON round trip: %q", decoded.Counters["net.eth0.rx_bytes"])
	}
}

func TestFoundationFixtures(t *testing.T) {
	tests := []struct {
		name  string
		check func([]byte) error
	}{
		{"large-counter", func(data []byte) error {
			var sample MetricSample
			if err := json.Unmarshal(data, &sample); err != nil {
				return err
			}
			return sample.Validate()
		}},
		{"gap", func(data []byte) error {
			var gap CoverageGap
			if err := json.Unmarshal(data, &gap); err != nil {
				return err
			}
			return gap.Validate()
		}},
		{"expired-job", func(data []byte) error {
			var job JobRequest
			if err := json.Unmarshal(data, &job); err != nil {
				return err
			}
			if err := job.Validate(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)); err == nil {
				return fmt.Errorf("expired job accepted")
			}
			return nil
		}},
		{"partial-fleet", func(data []byte) error {
			var result FleetResult[Server]
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			if !result.Partial || len(result.Succeeded) != 1 || len(result.Failed) != 1 {
				return fmt.Errorf("partial result not represented")
			}
			return nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := fixtures.ReadFile("testdata/" + tc.name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.check(data); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWire64BitFieldsAreStrings(t *testing.T) {
	server := Server{ID: "server-0123456789", ConfigurationRevision: 9007199254740993}
	gap := CoverageGap{CollectorEpoch: "epoch-0123456789", FromSequence: 9007199254740993, ToSequence: 9007199254740995, Reason: "transport-disconnect"}
	serverJSON, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	gapJSON, err := json.Marshal(gap)
	if err != nil {
		t.Fatal(err)
	}
	if string(serverJSON) == "" || string(gapJSON) == "" {
		t.Fatal("empty JSON")
	}
	if !bytes.Contains(serverJSON, []byte(`"configuration_revision":"9007199254740993"`)) {
		t.Fatalf("revision is not a decimal string: %s", serverJSON)
	}
	if !bytes.Contains(gapJSON, []byte(`"from_sequence":"9007199254740993"`)) {
		t.Fatalf("gap sequence is not a decimal string: %s", gapJSON)
	}
}

func TestClockSkewIsPreserved(t *testing.T) {
	sample := MetricSample{ServerID: "server-0123456789", CollectorEpoch: "epoch-0123456789", ObservedAt: time.Unix(20, 0).UTC(), ReceivedAt: time.Unix(10, 0).UTC(), Values: map[string]float64{}}
	if err := sample.Validate(); err != nil {
		t.Fatal(err)
	}
	if sample.ClockSkewCode() != "source-clock-ahead" {
		t.Fatalf("unexpected skew code: %s", sample.ClockSkewCode())
	}
}

func TestNodeSampleIsStampedOnlyByTheHub(t *testing.T) {
	nodeSample := NodeMetricSample{
		ServerID: "server-0123456789", CollectorEpoch: "epoch-0123456789",
		ObservedAt: time.Unix(20, 0).UTC(), Values: map[string]float64{},
	}
	if err := nodeSample.Validate(); err != nil {
		t.Fatal(err)
	}
	nodeJSON, err := json.Marshal(nodeSample)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(nodeJSON, []byte(`received_at`)) {
		t.Fatalf("node wire sample included hub timestamp: %s", nodeJSON)
	}
	stored, err := nodeSample.WithReceivedAt(time.Unix(10, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.Validate(); err != nil {
		t.Fatal(err)
	}
	if stored.TimestampUncertainty != "source-clock-ahead" {
		t.Fatalf("hub did not stamp source clock uncertainty: %s", stored.TimestampUncertainty)
	}
	if stored.ClockSkewCode() != "source-clock-ahead" {
		t.Fatalf("unexpected skew code: %s", stored.ClockSkewCode())
	}
	if _, err := nodeSample.WithReceivedAt(time.Time{}); err == nil {
		t.Fatal("zero hub receipt time accepted")
	}
}

func TestSampleBatchValidatesCoverageGaps(t *testing.T) {
	valid := CoverageGap{CollectorEpoch: "epoch-0123456789", FromSequence: 4, ToSequence: 8, Reason: "spool-eviction"}
	if err := (SampleBatch{Gaps: []CoverageGap{valid}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, gap := range []CoverageGap{
		{FromSequence: 4, ToSequence: 8, Reason: "spool-eviction"},
		{CollectorEpoch: "epoch-0123456789", FromSequence: 8, ToSequence: 8, Reason: "spool-eviction"},
		{CollectorEpoch: "epoch-0123456789", FromSequence: 8, ToSequence: 4, Reason: "spool-eviction"},
		{CollectorEpoch: "epoch-0123456789", FromSequence: 4, ToSequence: 8, Reason: "unknown"},
	} {
		if err := (SampleBatch{Gaps: []CoverageGap{gap}}).Validate(); err == nil {
			t.Fatalf("invalid gap accepted: %#v", gap)
		}
	}
}

func TestHelloAndModuleInvocationProtocols(t *testing.T) {
	hello := Hello{Version: "0.1.0", ProtocolMin: "v1", ProtocolMax: "v1", Architecture: "amd64", Platform: "linux", InstalledModules: []InstalledModule{{ID: "port-traffic", Version: "1.0.0"}}}
	if err := hello.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"installed_modules":[{"id":"port-traffic","version":"1.0.0"}]`)) {
		t.Fatalf("installed modules not serialized: %s", b)
	}
	request := ModuleInvocationRequest{Protocol: ModuleProtocol, ModuleID: "port-traffic", ModuleVersion: "1.0.0", RequestID: "req-1", Operation: "status"}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"protocol":"payesh.module.v1"`)) {
		t.Fatalf("module protocol not serialized: %s", b)
	}
	request.Protocol = NodeProtocol
	if err := request.Validate(); err == nil {
		t.Fatal("wrong module protocol accepted")
	}
}

func TestHelloRejectsIncompatibleProtocolRange(t *testing.T) {
	base := Hello{Version: "0.1.0", ProtocolMin: "v1", ProtocolMax: "v1", Architecture: "amd64", Platform: "linux"}
	for _, invalid := range []Hello{
		{Version: base.Version, ProtocolMin: "v2", ProtocolMax: "v2", Architecture: base.Architecture, Platform: base.Platform},
		{Version: base.Version, ProtocolMin: "v2", ProtocolMax: "v1", Architecture: base.Architecture, Platform: base.Platform},
		{Version: base.Version, ProtocolMin: "vfoo", ProtocolMax: "vfoo", Architecture: base.Architecture, Platform: base.Platform},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("incompatible hello accepted: %+v", invalid)
		}
	}
	for _, valid := range []Hello{
		base,
		{Version: base.Version, ProtocolMin: "v0", ProtocolMax: "v1", Architecture: base.Architecture, Platform: base.Platform},
		{Version: base.Version, ProtocolMin: "v1", ProtocolMax: "v2", Architecture: base.Architecture, Platform: base.Platform},
	} {
		if err := valid.Validate(); err != nil {
			t.Fatalf("compatible hello rejected: %+v: %v", valid, err)
		}
	}
}

func TestHelperActionTargetIsDistinctFromServerTarget(t *testing.T) {
	action := ActionRequest{Protocol: HelperProtocol, RequestID: "req-1", Action: "service.install", Target: "payesh-agent", TargetServerID: "server-0123456789"}
	b, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"target":"payesh-agent"`)) || !bytes.Contains(b, []byte(`"target_server_id":"server-0123456789"`)) {
		t.Fatalf("helper target fields not preserved: %s", b)
	}
}

func TestEnvelopeAndBatchBounds(t *testing.T) {
	envelope := Envelope{Protocol: NodeProtocol, Message: "sample", SentAt: time.Now().UTC(), Body: bytes.Repeat([]byte{'x'}, MaxEnvelopeBytes+1)}
	if err := envelope.Validate(); err == nil {
		t.Fatal("oversized envelope accepted")
	}
	batch := SampleBatch{Samples: make([]NodeMetricSample, MaxBatchSamples+1)}
	if err := batch.Validate(); err == nil {
		t.Fatal("oversized sample batch accepted")
	}
}

func TestExpectedRevisionZeroIsSerialized(t *testing.T) {
	b, err := json.Marshal(JobRequest{JobID: "job-1", IdempotencyKey: "retry-1", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"expected_revision":"0"`)) {
		t.Fatalf("zero revision omitted: %s", b)
	}
}

func TestAcknowledgementAndActionResponseZeroAreSerialized(t *testing.T) {
	ack, err := json.Marshal(Acknowledgement{RequestID: "req-1", Accepted: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(ActionResponse{RequestID: "req-1", Accepted: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(ack, []byte(`"through_sequence":"0"`)) || !bytes.Contains(response, []byte(`"revision":"0"`)) {
		t.Fatalf("zero progress/revision omitted: %s / %s", ack, response)
	}
}

func TestMetricValidationBoundsAndValues(t *testing.T) {
	base := MetricSample{ServerID: "server-0123456789", CollectorEpoch: "epoch-0123456789", ObservedAt: time.Unix(1, 0).UTC(), ReceivedAt: time.Unix(1, 0).UTC(), Values: map[string]float64{}}
	tooLarge := base
	tooLarge.Counters = map[string]string{"bytes": "18446744073709551616"}
	if err := tooLarge.Validate(); err == nil {
		t.Fatal("overflowing counter accepted")
	}
	max := base
	max.Counters = map[string]string{"bytes": "18446744073709551615"}
	if err := max.Validate(); err != nil {
		t.Fatalf("maximum uint64 rejected: %v", err)
	}
	missingValues := base
	missingValues.Values = nil
	if err := missingValues.Validate(); err == nil {
		t.Fatal("nil values accepted")
	}
	nonFinite := base
	nonFinite.Values = map[string]float64{"cpu": math.NaN()}
	if err := nonFinite.Validate(); err == nil {
		t.Fatal("non-finite metric accepted")
	}
	tooMany := base
	tooMany.Values = make(map[string]float64, MaxMetricFields+1)
	for index := 0; index <= MaxMetricFields; index++ {
		tooMany.Values[fmt.Sprintf("metric.%d", index)] = 1
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("unbounded metric field collection accepted")
	}
	badName := base
	badName.Values = map[string]float64{"metric value": 1}
	if err := badName.Validate(); err == nil {
		t.Fatal("invalid metric name accepted")
	}
}

func TestTrafficPeriodPreservesDecimalByteTotals(t *testing.T) {
	period := TrafficPeriod{
		Scope:    "host",
		From:     time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Timezone: "UTC", AllowanceBytes: 18446744073709551615,
		Direction: "combined", CountedBytes: 9007199254740993, Continuity: "complete",
	}
	if err := period.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(period)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"allowance_bytes":"18446744073709551615"`)) || !bytes.Contains(b, []byte(`"counted_bytes":"9007199254740993"`)) {
		t.Fatalf("traffic totals were not decimal strings: %s", b)
	}
}

func TestTrafficAllowanceRejectsReservedBillingInterface(t *testing.T) {
	allowance := TrafficAllowance{Scope: "host", Interfaces: []string{"billing"}, Direction: "combined", AllowanceBytes: 1, ResetDay: 1, Timezone: "UTC"}
	if err := allowance.Validate(); err == nil {
		t.Fatal("reserved aggregate interface was accepted")
	}
}

func TestAlertRuleRecoveryThresholdFollowsOperatorDirection(t *testing.T) {
	base := AlertRule{ID: "alert-hysteresis-01", Name: "Hysteresis", ServerID: "server-0123456789", Enabled: true, DurationSeconds: 60, ReminderSeconds: 60, IdempotencyKey: "alert-hysteresis-idem"}
	tests := []struct {
		name       string
		expression string
		recovery   float64
		valid      bool
	}{
		{name: "greater-than below", expression: "cpu.utilization > 90", recovery: 89, valid: true},
		{name: "greater-than at threshold", expression: "cpu.utilization > 90", recovery: 90, valid: false},
		{name: "greater-than above", expression: "cpu.utilization > 90", recovery: 91, valid: false},
		{name: "greater-equal below", expression: "cpu.utilization >= 90", recovery: 89, valid: true},
		{name: "less-than above", expression: "temperature < 10", recovery: 11, valid: true},
		{name: "less-than at threshold", expression: "temperature < 10", recovery: 10, valid: false},
		{name: "less-equal above", expression: "temperature <= 10", recovery: 11, valid: true},
		{name: "less-equal below", expression: "temperature <= 10", recovery: 9, valid: false},
		{name: "equal same threshold", expression: "probe == 1", recovery: 1, valid: true},
		{name: "equal different threshold", expression: "probe == 1", recovery: 0, valid: false},
		{name: "not-equal same threshold", expression: "probe != 1", recovery: 1, valid: true},
		{name: "not-equal different threshold", expression: "probe != 1", recovery: 2, valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rule := base
			rule.Expression = test.expression
			recovery := test.recovery
			rule.RecoveryThreshold = &recovery
			err := rule.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid recovery threshold rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid recovery threshold accepted")
			}
		})
	}
}
