package ovn

// WatchNetworkPolicy starts the watching of network policy resource and calls
// back the appropriate handler logic
func (oc *DefaultNetworkController) WatchNetworkPolicy() error {
	_, err := oc.retryNetworkPolicies.WatchResource()
	return err
}
