// Command billing-compare compares Payesh's exact period byte total with a
// provider-authoritative total. Decimal strings remain arbitrary precision so
// counters above JavaScript's safe integer range are never rounded.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
)

type report struct {
	Format          string `json:"format"`
	PayeshBytes     string `json:"payesh_bytes"`
	ProviderBytes   string `json:"provider_bytes"`
	AbsoluteDelta   string `json:"absolute_delta_bytes"`
	DeltaDirection  string `json:"delta_direction"`
	DeltaPercent    string `json:"delta_percent"`
	Tolerance       string `json:"tolerance_percent"`
	WithinTolerance bool   `json:"within_tolerance"`
}

func main() {
	payesh := flag.String("payesh-bytes", "", "exact Payesh counted bytes for one matching period")
	provider := flag.String("provider-bytes", "", "exact provider-authoritative bytes for the same period/direction")
	tolerance := flag.String("tolerance-percent", "5", "maximum accepted absolute percentage difference")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("positional arguments are not accepted"))
	}
	r, err := compare(*payesh, *provider, *tolerance)
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fatal(err)
	}
	if !r.WithinTolerance {
		os.Exit(1)
	}
}

func compare(payeshText, providerText, toleranceText string) (report, error) {
	payesh, err := decimalUint(payeshText)
	if err != nil {
		return report{}, fmt.Errorf("payesh bytes: %w", err)
	}
	provider, err := decimalUint(providerText)
	if err != nil {
		return report{}, fmt.Errorf("provider bytes: %w", err)
	}
	tolerance, ok := new(big.Rat).SetString(strings.TrimSpace(toleranceText))
	if !ok || tolerance.Sign() < 0 || tolerance.Cmp(big.NewRat(100, 1)) > 0 {
		return report{}, errors.New("tolerance percent must be a decimal from 0 through 100")
	}
	delta := new(big.Int).Sub(payesh, provider)
	direction := "equal"
	if delta.Sign() < 0 {
		direction = "payesh-lower"
		delta.Abs(delta)
	} else if delta.Sign() > 0 {
		direction = "payesh-higher"
	}
	percent := new(big.Rat)
	within := delta.Sign() == 0
	if provider.Sign() > 0 {
		percent.SetFrac(delta, provider)
		percent.Mul(percent, big.NewRat(100, 1))
		within = percent.Cmp(tolerance) <= 0
	} else if payesh.Sign() == 0 {
		percent.SetInt64(0)
	}
	return report{
		Format: "payesh.billing-comparison.v1", PayeshBytes: payesh.String(), ProviderBytes: provider.String(),
		AbsoluteDelta: delta.String(), DeltaDirection: direction, DeltaPercent: percent.FloatString(6),
		Tolerance: tolerance.FloatString(6), WithinTolerance: within,
	}, nil
}

func decimalUint(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") || (len(value) > 1 && value[0] == '0') {
		return nil, errors.New("value must be a canonical unsigned decimal integer")
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok || parsed.Sign() < 0 {
		return nil, errors.New("value must be a canonical unsigned decimal integer")
	}
	return parsed, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "billing-compare:", err)
	os.Exit(2)
}
