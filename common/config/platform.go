package config

import (
	"fmt"
	"net/url"
)

// Platform describes the OpenCloud instance both services talk to. Variables
// defined by the platform are consumed under their own names; the rest carry
// the PLATFORM_ prefix.
type Platform struct {
	EventsEndpoint       string `json:"events_endpoint" env:"OC_EVENTS_ENDPOINT" desc:"Address of the NATS of the platform."`
	EventsCluster        string `json:"events_cluster" env:"OC_EVENTS_CLUSTER" desc:"Cluster ID of the NATS of the platform."`
	EventsEnableTLS      bool   `json:"events_enable_tls" env:"OC_EVENTS_ENABLE_TLS" desc:"Connect to the NATS over TLS."`
	EventsTLSInsecure    bool   `json:"events_tls_insecure" env:"OC_EVENTS_TLS_INSECURE" desc:"Do not verify the TLS certificate of the NATS."`
	EventsAuthUsername   string `json:"events_auth_username" env:"OC_EVENTS_AUTH_USERNAME" desc:"User name for the NATS, empty when it needs none."`
	EventsAuthPassword   Secret `json:"events_auth_password" env:"OC_EVENTS_AUTH_PASSWORD" desc:"Password for the NATS, empty when it needs none."`
	GatewayGRPCAddr      string `json:"gateway_grpc_addr" env:"OC_GATEWAY_GRPC_ADDR" desc:"Address of the CS3 gateway of the platform."`
	ServiceAccountID     string `json:"service_account_id" env:"OC_SERVICE_ACCOUNT_ID,required" desc:"Service account the platform is configured with."`
	ServiceAccountSecret Secret `json:"service_account_secret" env:"OC_SERVICE_ACCOUNT_SECRET,required" desc:"Secret of the service account."`
	InternalURL          string `json:"internal_url" env:"PLATFORM_INTERNAL_URL" desc:"URL of the proxy of the platform, used for PROPFIND with the headers of the client."`
	Insecure             bool   `json:"insecure" env:"OC_INSECURE" desc:"Do not verify the TLS certificates of the platform."`
}

// DefaultPlatform returns the platform block as the stand is wired: every
// service of the platform runs in the container named opencloud.
func DefaultPlatform() Platform {
	return Platform{
		EventsEndpoint:  "opencloud:9233",
		EventsCluster:   "opencloud-cluster",
		GatewayGRPCAddr: "opencloud:9142",
		InternalURL:     "https://opencloud:9200",
	}
}

// Validate reports whether the block can be used to reach the platform.
func (p Platform) Validate() error {
	if p.EventsEndpoint == "" {
		return fmt.Errorf("%w: %s", ErrMissing, "OC_EVENTS_ENDPOINT")
	}
	if p.GatewayGRPCAddr == "" {
		return fmt.Errorf("%w: %s", ErrMissing, "OC_GATEWAY_GRPC_ADDR")
	}
	return ValidateURL("PLATFORM_INTERNAL_URL", p.InternalURL)
}

// ValidateURL reports whether value is an absolute http or https URL.
func ValidateURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("config: %s=%q: must be an absolute http or https URL", name, value)
	}
	return nil
}
