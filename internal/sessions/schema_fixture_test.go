package sessions_test

// Version admission registers capability ticket foreign keys too. Match the
// application's migration set so package tests exercise those keys on PostgreSQL.
import (
	_ "github.com/braedonsaunders/ether/internal/auth"
	_ "github.com/braedonsaunders/ether/internal/identity"
)
