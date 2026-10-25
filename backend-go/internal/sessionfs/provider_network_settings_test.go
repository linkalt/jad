package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestNormalizeProviderNetworkSettings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "absent key takes the default", raw: "", want: "tcp"},
		{name: "empty object takes the default", raw: `{}`, want: "tcp"},
		{name: "tcp", raw: `{"network":"tcp"}`, want: "tcp"},
		{name: "tcp4", raw: `{"network":"tcp4"}`, want: "tcp4"},
		{name: "tcp6", raw: `{"network":"tcp6"}`, want: "tcp6"},
		{name: "null does not pick a default", raw: `{"network":null}`, wantErr: "must be one of tcp, tcp4, tcp6"},
		{name: "unknown value", raw: `{"network":"udp"}`, wantErr: "provider_network.network must be one of tcp, tcp4, tcp6"},
		{name: "not a string", raw: `{"network":4}`, wantErr: "must be one of tcp, tcp4, tcp6"},
		{name: "unknown key", raw: `{"family":"tcp4"}`, wantErr: "family"},
		{name: "not an object", raw: `"tcp4"`, wantErr: "provider_network must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProviderNetworkSettings(json.RawMessage(tc.raw), "session.json")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Network != tc.want {
				t.Fatalf("got %q, %v; want %q", got.Network, err, tc.want)
			}
		})
	}
}

func TestProviderNetworkSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			ProviderNetwork struct {
				AdditionalProperties bool `json:"additionalProperties"`
				Properties           map[string]struct {
					Enum    []string `json:"enum"`
					Default string   `json:"default"`
				} `json:"properties"`
			} `json:"provider_network"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	node := schema.Properties.ProviderNetwork
	var keys []string
	for key := range node.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, providerNetworkKeys) || node.AdditionalProperties {
		t.Fatalf("schema keys %v (additionalProperties %v), loader accepts %v", keys, node.AdditionalProperties, providerNetworkKeys)
	}
	network := node.Properties["network"]
	if !reflect.DeepEqual(network.Enum, providerNetworkValues) || network.Default != defaultProviderNetwork {
		t.Fatalf("schema enum %v default %q, loader %v %q", network.Enum, network.Default, providerNetworkValues, defaultProviderNetwork)
	}
	if !strings.Contains(string(data), `"#/properties/provider_network"`) {
		t.Fatal("schema does not reference provider_network from the settings object")
	}
}

func TestLoadedSessionConfigCarriesProviderNetwork(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"provider_network": map[string]any{"network": "tcp4"}})
	if err != nil || config.ProviderNetwork.Network != "tcp4" {
		t.Fatalf("network = %q, err %v", config.ProviderNetwork.Network, err)
	}
	if config, err = loadSetting(t, map[string]any{}); err != nil || config.ProviderNetwork.Network != "tcp" {
		t.Fatalf("default network = %q, err %v", config.ProviderNetwork.Network, err)
	}
}
