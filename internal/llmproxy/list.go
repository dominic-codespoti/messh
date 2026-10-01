package llmproxy

// ServicesPath is the control API route that lists the mesh's model
// services; ?device= narrows it to one device.
const ServicesPath = "/v1/llm/services"

// Listing is one row of ServicesPath: an openai-kind service on a device and
// the local base URL that reaches it, or (with Service empty and Error set)
// a device that could not be asked.
type Listing struct {
	Device      string `json:"device"`
	Handle      string `json:"handle"`
	DeviceID    string `json:"device_id"`
	Self        bool   `json:"self,omitempty"`
	Online      bool   `json:"online"`
	Service     string `json:"service,omitempty"`
	Description string `json:"description,omitempty"`
	Up          bool   `json:"up"`
	BaseURL     string `json:"base_url,omitempty"`
	Error       string `json:"error,omitempty"`
}
