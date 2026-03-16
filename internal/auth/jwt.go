package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JWT errors
var (
	ErrTokenMalformed = errors.New("malformed token")
	ErrTokenExpired   = errors.New("token has expired")
	ErrTokenInvalid   = errors.New("token signature invalid")
)

// JWTManager handles JWT token operations
type JWTManager struct {
	secret        []byte
	accessExpiry  time.Duration
	refreshExpiry time.Duration
}

// NewJWTManager creates a new JWT manager
func NewJWTManager(secret string, accessExpiry, refreshExpiry time.Duration) *JWTManager {
	return &JWTManager{
		secret:        []byte(secret),
		accessExpiry:  accessExpiry,
		refreshExpiry: refreshExpiry,
	}
}

// jwtHeader represents the JWT header
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// GenerateAccessToken creates a new access token for a user
func (m *JWTManager) GenerateAccessToken(user *User, sessionID string) (string, error) {
	now := time.Now()
	claims := &JWTClaims{
		UserID:      user.ID,
		Username:    user.Username,
		RoleID:      user.RoleID,
		RoleName:    user.RoleName,
		SessionID:   sessionID,
		Permissions: user.Permissions,
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(m.accessExpiry).Unix(),
		NotBefore:   now.Unix(),
		TokenType:   "access",
	}

	return m.encode(claims)
}

// GenerateRefreshToken creates a new refresh token for a user
func (m *JWTManager) GenerateRefreshToken(user *User, sessionID string) (string, error) {
	now := time.Now()
	claims := &JWTClaims{
		UserID:    user.ID,
		Username:  user.Username,
		RoleID:    user.RoleID,
		RoleName:  user.RoleName,
		SessionID: sessionID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(m.refreshExpiry).Unix(),
		NotBefore: now.Unix(),
		TokenType: "refresh",
	}

	return m.encode(claims)
}

// ValidateToken validates a token and returns its claims
func (m *JWTManager) ValidateToken(tokenString string) (*JWTClaims, error) {
	claims, err := m.decode(tokenString)
	if err != nil {
		return nil, err
	}

	// Check expiration
	if time.Now().Unix() > claims.ExpiresAt {
		return nil, ErrTokenExpired
	}

	// Check not before
	if claims.NotBefore > 0 && time.Now().Unix() < claims.NotBefore {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}

// encode creates a JWT token from claims
func (m *JWTManager) encode(claims *JWTClaims) (string, error) {
	// Create header
	header := jwtHeader{
		Alg: "HS256",
		Typ: "JWT",
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}

	// Create payload
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	// Encode header and payload
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	// Create signature
	signingInput := headerB64 + "." + payloadB64
	signature := m.sign([]byte(signingInput))
	signatureB64 := base64.RawURLEncoding.EncodeToString(signature)

	return signingInput + "." + signatureB64, nil
}

// decode parses and validates a JWT token
func (m *JWTManager) decode(tokenString string) (*JWTClaims, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}

	headerB64, payloadB64, signatureB64 := parts[0], parts[1], parts[2]

	// Verify signature
	signingInput := headerB64 + "." + payloadB64
	expectedSig := m.sign([]byte(signingInput))
	actualSig, err := base64.RawURLEncoding.DecodeString(signatureB64)
	if err != nil {
		return nil, ErrTokenMalformed
	}

	if !hmac.Equal(expectedSig, actualSig) {
		return nil, ErrTokenInvalid
	}

	// Decode payload
	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrTokenMalformed
	}

	var claims JWTClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, ErrTokenMalformed
	}

	return &claims, nil
}

// sign creates an HMAC-SHA256 signature
func (m *JWTManager) sign(data []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write(data)
	return h.Sum(nil)
}
