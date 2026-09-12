// Package bandwidth implements the policy and safety boundary for the
// separately installed Bandwidth Controls module.
package bandwidth

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

const (
	ModuleID   = "bandwidth-controls"
	PolicyKind = "bandwidth-limit"
)

type Action string

const (
	WarnOnly Action = "warn"
	Throttle Action = "throttle"
	Block    Action = "block"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type Scope struct {
	TargetKind string                `json:"target_kind"`
	Interface  string                `json:"interface"`
	Protocol   porttraffic.Protocol  `json:"protocol,omitempty"`
	LocalPort  uint16                `json:"local_port,omitempty"`
	Direction  porttraffic.Direction `json:"direction"`
}

func (s Scope) TargetName() string {
	if s.TargetKind == "interface" {
		return s.Interface + ":" + string(s.Direction)
	}
	return s.Interface + ":" + string(s.Protocol) + ":" + strconv.Itoa(int(s.LocalPort)) + ":" + string(s.Direction)
}

func (s Scope) Validate() error {
	if !safeName.MatchString(s.Interface) {
		return errors.New("interface must be a safe 1..128 character name")
	}
	if s.Direction != porttraffic.Inbound && s.Direction != porttraffic.Outbound {
		return errors.New("direction must be inbound or outbound")
	}
	switch s.TargetKind {
	case "interface":
		if s.Protocol != "" || s.LocalPort != 0 {
			return errors.New("interface scope cannot include protocol or local_port")
		}
	case "local-port":
		if s.Protocol != porttraffic.TCP && s.Protocol != porttraffic.UDP {
			return errors.New("local-port protocol must be tcp or udp")
		}
		if s.LocalPort == 0 {
			return errors.New("local_port must be 1..65535")
		}
	default:
		return errors.New("target_kind must be interface or local-port")
	}
	return nil
}

type ManagementFlow struct {
	Name       string               `json:"name"`
	Interface  string               `json:"interface"`
	Protocol   porttraffic.Protocol `json:"protocol"`
	LocalPort  uint16               `json:"local_port"`
	PortRole   string               `json:"port_role"`
	SharedPort bool                 `json:"shared_port,omitempty"`
}

type Request struct {
	Scope          Scope  `json:"scope"`
	Action         Action `json:"action"`
	BitsPerSecond  uint64 `json:"bits_per_second,string,omitempty"`
	QuotaBytes     uint64 `json:"quota_bytes,string,omitempty"`
	UsageScope     string `json:"usage_scope,omitempty"`
	UsageDirection string `json:"usage_direction,omitempty"`
}

type Preview struct {
	Request        Request          `json:"request"`
	KernelPath     string           `json:"kernel_path"`
	Exclusions     []ManagementFlow `json:"management_exclusions,omitempty"`
	AffectsNetwork bool             `json:"affects_network"`
	Warnings       []string         `json:"warnings,omitempty"`
}

func BuildPreview(request Request, management []ManagementFlow) (Preview, error) {
	if err := request.Scope.Validate(); err != nil {
		return Preview{}, err
	}
	if len(management) > 32 {
		return Preview{}, errors.New("management flow count exceeds limit")
	}
	switch request.Action {
	case WarnOnly:
		if request.BitsPerSecond != 0 {
			return Preview{}, errors.New("warn action cannot set a throttle speed")
		}
	case Throttle:
		if request.BitsPerSecond < 8_000 || request.BitsPerSecond > 100_000_000_000 {
			return Preview{}, errors.New("throttle speed must be 8000..100000000000 bits/second")
		}
	case Block:
		if request.BitsPerSecond != 0 {
			return Preview{}, errors.New("block action cannot set a throttle speed")
		}
	default:
		return Preview{}, errors.New("action must be warn, throttle, or block")
	}
	preview := Preview{Request: request, KernelPath: "egress-shaper", AffectsNetwork: request.Action != WarnOnly}
	if request.Scope.Direction == porttraffic.Inbound {
		preview.KernelPath = "ingress-policer-or-ifb"
	}
	if request.QuotaBytes == 0 {
		preview.Warnings = append(preview.Warnings, "no quota action threshold; policy applies immediately")
	} else if !safeName.MatchString(request.UsageScope) {
		return Preview{}, errors.New("usage_scope is required when quota_bytes is set")
	} else if request.UsageDirection != "" && request.UsageDirection != "inbound" && request.UsageDirection != "outbound" && request.UsageDirection != "combined" {
		return Preview{}, errors.New("usage_direction must be inbound, outbound, or combined")
	}
	for _, flow := range management {
		if !safeName.MatchString(flow.Name) || !safeName.MatchString(flow.Interface) || (flow.Protocol != porttraffic.TCP && flow.Protocol != porttraffic.UDP) || flow.LocalPort == 0 || (flow.PortRole != "local-service" && flow.PortRole != "remote-service") {
			return Preview{}, errors.New("invalid management flow")
		}
		if flow.Interface != request.Scope.Interface {
			continue
		}
		if request.Scope.TargetKind == "local-port" && flow.PortRole == "local-service" && flow.Protocol == request.Scope.Protocol && flow.LocalPort == request.Scope.LocalPort {
			if flow.SharedPort {
				return Preview{}, fmt.Errorf("management flow %q shares the selected port and cannot be separated safely", flow.Name)
			}
			return Preview{}, fmt.Errorf("selected local port is required by management flow %q", flow.Name)
		}
		if request.Scope.TargetKind == "interface" {
			if flow.SharedPort {
				return Preview{}, fmt.Errorf("management flow %q uses a shared proxy port and cannot be exempted safely", flow.Name)
			}
			preview.Exclusions = append(preview.Exclusions, flow)
		}
	}
	return preview, nil
}
