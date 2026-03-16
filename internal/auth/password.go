package auth

import (
	"errors"
	"fmt"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// Password validation constants
const (
	MinPasswordLength = 8
	MaxPasswordLength = 128
	BcryptCost        = 12 // Good balance of security and performance
)

// Password validation errors
var (
	ErrPasswordTooShort    = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong     = fmt.Errorf("password cannot exceed %d characters", MaxPasswordLength)
	ErrPasswordNoUpper     = errors.New("password must contain at least one uppercase letter")
	ErrPasswordNoLower     = errors.New("password must contain at least one lowercase letter")
	ErrPasswordNoDigit     = errors.New("password must contain at least one digit")
	ErrPasswordNoSpecial   = errors.New("password must contain at least one special character")
	ErrPasswordMismatch    = errors.New("passwords do not match")
	ErrPasswordSameAsOld   = errors.New("new password must be different from current password")
)

// PasswordRequirements describes what makes a valid password
type PasswordRequirements struct {
	MinLength    int    `json:"min_length"`
	MaxLength    int    `json:"max_length"`
	RequireUpper bool   `json:"require_upper"`
	RequireLower bool   `json:"require_lower"`
	RequireDigit bool   `json:"require_digit"`
	RequireSpecial bool `json:"require_special"`
	Description  string `json:"description"`
}

// GetPasswordRequirements returns the current password policy
func GetPasswordRequirements() PasswordRequirements {
	return PasswordRequirements{
		MinLength:    MinPasswordLength,
		MaxLength:    MaxPasswordLength,
		RequireUpper: true,
		RequireLower: true,
		RequireDigit: true,
		RequireSpecial: true,
		Description: fmt.Sprintf(
			"Password must be %d-%d characters with uppercase, lowercase, digit, and special character",
			MinPasswordLength, MaxPasswordLength,
		),
	}
}

// ValidatePassword checks if a password meets all requirements
func ValidatePassword(password string) error {
	// Length check
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordLength {
		return ErrPasswordTooLong
	}

	var hasUpper, hasLower, hasDigit, hasSpecial bool

	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			hasSpecial = true
		}
	}

	if !hasUpper {
		return ErrPasswordNoUpper
	}
	if !hasLower {
		return ErrPasswordNoLower
	}
	if !hasDigit {
		return ErrPasswordNoDigit
	}
	if !hasSpecial {
		return ErrPasswordNoSpecial
	}

	return nil
}

// HashPassword creates a bcrypt hash of the password
func HashPassword(password string) (string, error) {
	// Validate password first
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	// Generate hash
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	return string(hash), nil
}

// VerifyPassword checks if a password matches a hash
func VerifyPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// PasswordStrength calculates a rough password strength score (0-100)
func PasswordStrength(password string) int {
	if len(password) == 0 {
		return 0
	}

	score := 0
	length := len(password)

	// Length contribution (up to 40 points)
	if length >= MinPasswordLength {
		score += 20
	}
	if length >= 16 {
		score += 10
	}
	if length >= 20 {
		score += 10
	}

	// Character variety (up to 40 points)
	var hasUpper, hasLower, hasDigit, hasSpecial bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			hasSpecial = true
		}
	}

	if hasUpper {
		score += 10
	}
	if hasLower {
		score += 10
	}
	if hasDigit {
		score += 10
	}
	if hasSpecial {
		score += 10
	}

	// Entropy bonus (up to 20 points)
	uniqueChars := make(map[rune]bool)
	for _, r := range password {
		uniqueChars[r] = true
	}
	uniqueRatio := float64(len(uniqueChars)) / float64(length)
	if uniqueRatio > 0.8 {
		score += 20
	} else if uniqueRatio > 0.6 {
		score += 15
	} else if uniqueRatio > 0.4 {
		score += 10
	} else if uniqueRatio > 0.2 {
		score += 5
	}

	// Cap at 100
	if score > 100 {
		score = 100
	}

	return score
}

// PasswordStrengthLabel returns a human-readable strength label
func PasswordStrengthLabel(score int) string {
	switch {
	case score >= 80:
		return "Strong"
	case score >= 60:
		return "Good"
	case score >= 40:
		return "Fair"
	case score >= 20:
		return "Weak"
	default:
		return "Very Weak"
	}
}
