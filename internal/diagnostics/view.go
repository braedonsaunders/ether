package diagnostics

import "time"

// Observation is one fact shown to the owner. It is not a diagnosis.
type Observation struct {
	At     time.Time `json:"at"`
	Kind   Kind      `json:"kind"`
	Count  int       `json:"count,omitempty"`
	Status int       `json:"status,omitempty"`
	Route  string    `json:"route,omitempty"`
	Bytes  int64     `json:"bytes,omitempty"`
	Device string    `json:"device,omitempty"`
}

// Hypothesis is an inference kept separate from observations.
type Hypothesis struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Gap is something the package does not contain, and why.
type Gap struct {
	Collector string `json:"collector"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
}

// IdentityPreview states exactly which usernames and addresses the ZIP
// will contain. Values are present only when that inclusion flag is on.
type IdentityPreview struct {
	UsernamesIncluded bool     `json:"usernamesIncluded"`
	AddressesIncluded bool     `json:"addressesIncluded"`
	Usernames         []string `json:"usernames,omitempty"`
	Addresses         []string `json:"addresses,omitempty"`
	Titles            []string `json:"titles,omitempty"`
	TitlesIncluded    bool     `json:"titlesIncluded"`
	Explanation       string   `json:"explanation"`
}

// Preview is the inclusion list shown before download.
// Files is the sealed archive inventory. It is empty until Finish freezes
// the ZIP, and then it matches that ZIP's members in order.
type Preview struct {
	Files     []string        `json:"files"`
	Omitted   []string        `json:"omitted,omitempty"`
	Identity  IdentityPreview `json:"identity"`
	Note      string          `json:"note,omitempty"`
	Redaction string          `json:"redaction"`
}

// CaptureView is the owner-console state of one capture.
type CaptureView struct {
	ID               string        `json:"id"`
	State            string        `json:"state"`
	DeviceConfirmed  bool          `json:"deviceConfirmed"`
	StartedAt        time.Time     `json:"startedAt"`
	Deadline         time.Time     `json:"deadline"`
	FinishedAt       *time.Time    `json:"finishedAt,omitempty"`
	Category         string        `json:"category"`
	Note             string        `json:"note,omitempty"`
	IncludeTitles    bool          `json:"includeTitles"`
	IncludeUsernames bool          `json:"includeUsernames"`
	IncludeAddresses bool          `json:"includeAddresses"`
	Device           *DeviceInfo   `json:"device,omitempty"`
	Discovery        []DeviceInfo  `json:"discovery,omitempty"`
	Observations     []Observation `json:"observations"`
	Hypotheses       []Hypothesis  `json:"hypotheses"`
	Coverage         []Gap         `json:"coverage"`
	Preview          *Preview      `json:"preview,omitempty"`
}
