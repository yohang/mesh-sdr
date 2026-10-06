package templates

// ReceiverBootstrapID is the DOM id of the JSON script that bootstraps the receiver island.
const ReceiverBootstrapID = "receiver-bootstrap"

// ReceiverBootstrap is the server data handed to the receiver JS island through
// templ.JSONScript. It is a dedicated view DTO: only what the browser needs, never secrets.
type ReceiverBootstrap struct {
	Devices []DeviceSummary `json:"devices"`
}

// DeviceSummary is a device entry of the receiver bootstrap.
type DeviceSummary struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Node              string `json:"node"`
	CenterFrequencyHz int64  `json:"centerFrequencyHz"`
}
