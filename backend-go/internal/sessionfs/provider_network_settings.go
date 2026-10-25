package sessionfs

import "encoding/json"

const defaultProviderNetwork = "tcp"

var (
	providerNetworkKeys   = []string{"network"}
	providerNetworkValues = []string{"tcp", "tcp4", "tcp6"}
)

// ProviderNetworkSettings chooses the address family of connections to the model provider.
type ProviderNetworkSettings struct {
	// Network is "tcp" (either family, the Go default), "tcp4" or "tcp6".
	Network string
}

// normalizeProviderNetworkSettings reads the `provider_network` node. Only an absent key
// takes a default: a null or misspelt value must not quietly pick one.
func normalizeProviderNetworkSettings(raw json.RawMessage, settingPath string) (ProviderNetworkSettings, error) {
	settings := ProviderNetworkSettings{Network: defaultProviderNetwork}
	if len(raw) == 0 {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": provider_network")
	if err != nil {
		return ProviderNetworkSettings{}, configError("%s: provider_network must be an object", settingPath)
	}
	for key := range fields {
		if !containsString(providerNetworkKeys, key) {
			return ProviderNetworkSettings{}, unsupportedToolKey(settingPath+": provider_network", key, providerNetworkKeys, "")
		}
	}
	if value, exists := fields["network"]; exists {
		var network string
		if json.Unmarshal(value, &network) != nil || !containsString(providerNetworkValues, network) {
			return ProviderNetworkSettings{}, configError("%s: provider_network.network must be one of tcp, tcp4, tcp6", settingPath)
		}
		settings.Network = network
	}
	return settings, nil
}
