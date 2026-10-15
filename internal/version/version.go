package version

// Value is stamped into release binaries by scripts/release-package.
// Source builds report dev until a release version is supplied.
var Value = "dev"

// Node is the version of the node agent itself. It moves only when node-side
// behaviour changes, so a hub-only release does not make every node look
// outdated. scripts/release-package stamps it from the NODE_VERSION file.
// Unstamped builds report Value.
var Node = ""

// NodeVersion returns the agent's own version.
func NodeVersion() string {
	if Node != "" {
		return Node
	}
	return Value
}

// NodeCapabilityPrefix prefixes the capability a node uses to report NodeVersion.
// Capabilities are free-form strings, so hubs that predate it ignore it.
const NodeCapabilityPrefix = "node-version:"
