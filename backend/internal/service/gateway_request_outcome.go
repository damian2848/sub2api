package service

// This completion marker is independent of customer pricing. A free successful
// API call is a success; charged partial output is not. NULL legacy rows remain
// distinguishable from an explicitly observed success/failure.
func gatewayRequestOutcome(success bool) *bool {
	return &success
}
