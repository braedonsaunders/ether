package config

type Markers struct {
	TheIntroDB bool   `json:"theIntroDb"`
	APIKey     string `json:"apiKey"`
	// SkipIntro and SkipCredits turn the skip buttons off.
	// A missing value keeps the button, which is the existing behaviour.
	SkipIntro   *bool `json:"skipIntro,omitempty"`
	SkipCredits *bool `json:"skipCredits,omitempty"`
}

// OfferIntro reports whether intro and recap skip buttons are offered.
func (m Markers) OfferIntro() bool { return m.SkipIntro == nil || *m.SkipIntro }

// OfferCredits reports whether the credits skip button is offered.
func (m Markers) OfferCredits() bool { return m.SkipCredits == nil || *m.SkipCredits }
