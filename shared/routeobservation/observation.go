// Package routeobservation is the edge's report of what it serves for each route, posted to the control plane
// (POST /v1/edge/routes/observations) under the edge's node credential and decoded strictly there: a field added
// here is added on both sides at once. Counters are int64, the control plane's database type.
package routeobservation

// RouteObservation is one route's served state at one edge process.
type RouteObservation struct {
	RouteID                    string `json:"route_id"`
	RouteRevision              int64  `json:"route_revision"`
	AssignmentID               string `json:"assignment_id,omitempty"`
	AssignmentGeneration       int64  `json:"assignment_generation,omitempty"`
	EdgeNodeID                 string `json:"edge_node_id"`
	EdgeProcessEpoch           string `json:"edge_process_epoch,omitempty"`
	ConnectorID                string `json:"connector_id,omitempty"`
	HostID                     string `json:"host_id,omitempty"`
	ConnectorGeneration        int64  `json:"connector_generation"`
	ConnectorSessionID         string `json:"connector_session_id,omitempty"`
	ConnectorProcessGeneration int64  `json:"connector_process_generation,omitempty"`
	ConfigGeneration           int64  `json:"config_generation,omitempty"`
	ConfigContentHash          string `json:"config_content_hash,omitempty"`
	AccessMode                 string `json:"access_mode,omitempty"`
	ViewerPolicyGeneration     int64  `json:"viewer_policy_generation,omitempty"`
	State                      string `json:"state,omitempty"`
	ObservedState              string `json:"observed_state,omitempty"`
}
