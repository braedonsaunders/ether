package frontend

// PublicViewer is the minimal identifier displayed by an owner-enabled picker.
type PublicViewer struct {
	ID   int64
	Name string
	// HasConfiguredPassword is known only when the identity service can
	// establish it from its active local credential record.
	HasConfiguredPassword *bool
}
