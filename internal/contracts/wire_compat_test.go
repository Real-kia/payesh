package contracts

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The node protocol is decoded strictly: an unknown field makes the receiver
// reject the whole message. Hubs and nodes are updated independently, so every
// field a release adds to a wire type must be declared here, together with how
// older peers are kept working. Fields may never be removed or renamed.
//
// testdata/wire-baseline.json holds the wire keys of the oldest release whose
// nodes and hubs must keep working with this one (currently v0.6.3). Regenerate
// it only when support for older releases is dropped:
//
//	PAYESH_DUMP_WIRE=testdata/wire-baseline.json go test ./internal/contracts -run TestDumpWireKeys
//
// Run that at the new baseline's release tag, and empty wireAdditions.
var wireTypes = map[string]reflect.Type{
	"Envelope":         reflect.TypeOf(Envelope{}),
	"Hello":            reflect.TypeOf(Hello{}),
	"Heartbeat":        reflect.TypeOf(Heartbeat{}),
	"SampleBatch":      reflect.TypeOf(SampleBatch{}),
	"Acknowledgement":  reflect.TypeOf(Acknowledgement{}),
	"RelayPolicy":      reflect.TypeOf(RelayPolicy{}),
	"NodeAddresses":    reflect.TypeOf(NodeAddresses{}),
	"ActionRequest":    reflect.TypeOf(ActionRequest{}),
	"ActionResponse":   reflect.TypeOf(ActionResponse{}),
	"Cancellation":     reflect.TypeOf(Cancellation{}),
	"CoreUpdateIntent": reflect.TypeOf(CoreUpdateIntent{}),
}

// wireAdditions lists wire keys added since the baseline, with the mechanism
// that keeps older peers working. Format: "Type.path" -> reason.
var wireAdditions = map[string]string{}

func wireKeys(t reflect.Type, prefix string, out map[string]bool, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() == "time" || seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		path := prefix + name
		out[path] = true
		wireKeys(field.Type, path+".", out, seen)
	}
}

func currentWireKeys() map[string][]string {
	result := map[string][]string{}
	for name, typ := range wireTypes {
		keys := map[string]bool{}
		wireKeys(typ, "", keys, map[reflect.Type]bool{})
		list := make([]string, 0, len(keys))
		for key := range keys {
			list = append(list, key)
		}
		sort.Strings(list)
		result[name] = list
	}
	return result
}

func TestDumpWireKeys(t *testing.T) {
	path := os.Getenv("PAYESH_DUMP_WIRE")
	if path == "" {
		t.Skip("set PAYESH_DUMP_WIRE to write the wire key baseline")
	}
	body, err := json.MarshalIndent(currentWireKeys(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWireTypesStayCompatibleWithTheBaselineRelease(t *testing.T) {
	body, err := os.ReadFile("testdata/wire-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	var baseline map[string][]string
	if err := json.Unmarshal(body, &baseline); err != nil {
		t.Fatal(err)
	}
	current := currentWireKeys()
	used := map[string]bool{}
	for name, keys := range current {
		old := map[string]bool{}
		for _, key := range baseline[name] {
			old[key] = true
		}
		now := map[string]bool{}
		for _, key := range keys {
			now[key] = true
			if old[key] {
				continue
			}
			id := name + "." + key
			if _, declared := wireAdditions[id]; !declared {
				t.Errorf("%s is a new wire field. Older peers reject unknown fields, so declare it in wireAdditions with how they are protected (capability gating, or sent only to peers that advertise support)", id)
			}
			used[id] = true
		}
		for key := range old {
			if !now[key] {
				t.Errorf("%s.%s was removed or renamed; older peers still send and expect it", name, key)
			}
		}
	}
	for id := range wireAdditions {
		if !used[id] {
			t.Errorf("wireAdditions lists %s, which is not an addition to the baseline; remove it", id)
		}
	}
}
