// Package auth implements registration, login, session, and invite code
// management for PulseGuard tenants.
package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword applies bcrypt at the configured cost and returns the hash
// bytes. cost must be within [bcrypt.MinCost, bcrypt.MaxCost]; 0 means
// the bcrypt default.
func HashPassword(password string, cost int) ([]byte, error) {
	if password == "" {
		return nil, fmt.Errorf("password is empty")
	}
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return nil, fmt.Errorf("bcrypt hash: %w", err)
	}
	return h, nil
}

// CompareHashAndPassword returns nil iff the password matches the hash.
func CompareHashAndPassword(hash []byte, password string) error {
	return bcrypt.CompareHashAndPassword(hash, []byte(password))
}
