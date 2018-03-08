package iotservice

// Result is a direct-method call result.
type Result struct {
	Status  int                    `json:"status,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

type Device struct {
	DeviceID                   string                 `json:"deviceId,omitempty"`
	GenerationID               string                 `json:"generationId,omitempty"`
	ETag                       string                 `json:"etag,omitempty"`
	ConnectionState            string                 `json:"connectionState,omitempty"`
	Status                     string                 `json:"status,omitempty"`
	StatusReason               string                 `json:"statusReason,omitempty"`
	ConnectionStateUpdatedTime string                 `json:"connectionStateUpdatedTime,omitempty"`
	StatusUpdatedTime          string                 `json:"statusUpdatedTime,omitempty"`
	LastActivityTime           string                 `json:"lastActivityTime,omitempty"`
	CloudToDeviceMessageCount  int                    `json:"cloudToDeviceMessageCount,omitempty"`
	Authentication             *Authentication        `json:"authentication,omitempty"`
	Capabilities               map[string]interface{} `json:"capabilities,omitempty"`
}

type Authentication struct {
	SymmetricKey   *SymmetricKey   `json:"symmetricKey,omitempty"`
	X509Thumbprint *X509Thumbprint `json:"x509Thumbprint,omitempty"`
	Type           string          `json:"type,omitempty"`
}

type X509Thumbprint struct {
	PrimaryThumbprint   string `json:"primaryThumbprint,omitempty"`
	SecondaryThumbprint string `json:"secondaryThumbprint,omitempty"`
}

type SymmetricKey struct {
	PrimaryKey   string `json:"primaryKey,omitempty"`
	SecondaryKey string `json:"secondaryKey,omitempty"`
}

type Twin struct {
	DeviceID                  string          `json:"deviceId,omitempty"`
	ETag                      string          `json:"etag,omitempty"`
	DeviceETag                string          `json:"deviceEtag,omitempty"`
	Status                    string          `json:"status,omitempty"`
	StatusReason              string          `json:"statusReason,omitempty"`
	StatusUpdateTime          string          `json:"statusUpdateTime,omitempty"`
	ConnectionState           string          `json:"connectionState,omitempty"`
	LastActivityTime          string          `json:"lastActivityTime,omitempty"`
	CloudToDeviceMessageCount int             `json:"cloudToDeviceMessageCount,omitempty"`
	AuthenticationType        string          `json:"authenticationType,omitempty"`
	X509Thumbprint            *X509Thumbprint `json:"x509Thumbprint,omitempty"`
	Version                   int             `json:"version,omitempty"`
	// TODO: "tags": {
	//        "$etag": "123",
	//        "deploymentLocation": {
	//            "building": "43",
	//            "floor": "1"
	//        }
	//    },
	Properties   *Properties            `json:"properties,omitempty"`
	Capabilities map[string]interface{} `json:"capabilities,omitempty"`
}

type Properties struct {
	Desired  map[string]interface{} `json:"desired,omitempty"`
	Reported map[string]interface{} `json:"reported,omitempty"`
}

type Stats struct {
	DisabledDeviceCount int `json:"disabledDeviceCount,omitempty"`
	EnabledDeviceCount  int `json:"enabledDeviceCount,omitempty"`
	TotalDeviceCount    int `json:"totalDeviceCount,omitempty"`
}