package provider

// Placement captures the provider settings that affect a new machine independently
// of its hardware. A non-nil, empty placement explicitly disables Boat environment
// injection, even if the configured default changes before a retry.
type Placement struct {
	Environment string `json:"environment,omitempty"`
	Site        string `json:"site,omitempty"`
}

// PlacementProvider exposes a snapshot of the adapter's configured placement.
// No credentials are resolved and no provider operation is performed here.
type PlacementProvider interface {
	DefaultPlacement() Placement
}
