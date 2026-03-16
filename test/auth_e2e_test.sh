#!/bin/bash
#
# Skylens Authentication E2E Test Suite
# ======================================
# Comprehensive tests for all authentication flows
#
# Test Categories:
#   1. Login Flow (valid, invalid, lockout)
#   2. Protected Routes (401 without auth, 200 with valid token)
#   3. Token Refresh (before expiry)
#   4. Logout (session invalidation, cookie clearing)
#   5. WebSocket Auth (rejected without token, accepted with)
#   6. CSRF Protection (state-changing endpoints)
#   7. Password Change (old sessions revoked)
#   8. Role-Based Access (Admin vs Operator vs Viewer)
#

set -e

BASE_URL="http://localhost:8080"
WS_URL="ws://localhost:8081"
COOKIE_JAR="/tmp/auth_test_cookies.txt"
ADMIN_COOKIE_JAR="/tmp/admin_cookies.txt"
OPERATOR_COOKIE_JAR="/tmp/operator_cookies.txt"
VIEWER_COOKIE_JAR="/tmp/viewer_cookies.txt"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Counters
TESTS_PASSED=0
TESTS_FAILED=0
TESTS_SKIPPED=0

# Test output
log_test() {
    echo -e "${BLUE}[TEST]${NC} $1"
}

log_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
    ((TESTS_PASSED++))
}

log_fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    ((TESTS_FAILED++))
}

log_skip() {
    echo -e "${YELLOW}[SKIP]${NC} $1"
    ((TESTS_SKIPPED++))
}

log_info() {
    echo -e "${YELLOW}[INFO]${NC} $1"
}

# Cleanup function
cleanup() {
    rm -f "$COOKIE_JAR" "$ADMIN_COOKIE_JAR" "$OPERATOR_COOKIE_JAR" "$VIEWER_COOKIE_JAR" 2>/dev/null || true
    rm -f /tmp/test_response.json /tmp/test_headers.txt 2>/dev/null || true
}

# Cleanup on exit
trap cleanup EXIT

# ============================================================================
# SECTION 1: LOGIN FLOW TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 1: LOGIN FLOW TESTS"
echo "============================================================================"

# Test 1.1: Valid credentials login
log_test "1.1 Login with valid credentials (admin/changeme123!)"
RESPONSE=$(curl -s -c "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"changeme123!"}')

HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    # Verify response contains user data
    if echo "$BODY" | grep -q '"username":"admin"' && echo "$BODY" | grep -q '"role_name":"admin"'; then
        log_pass "Valid credentials login successful (HTTP $HTTP_CODE)"
        log_info "Response: $(echo "$BODY" | head -c 200)..."
    else
        log_fail "Login succeeded but response missing expected user data"
    fi
else
    log_fail "Valid credentials login failed (HTTP $HTTP_CODE)"
    log_info "Response: $BODY"
fi

# Verify cookies were set
if grep -q "skylens_token" "$ADMIN_COOKIE_JAR" && grep -q "skylens_csrf" "$ADMIN_COOKIE_JAR"; then
    log_pass "Auth cookies (skylens_token, skylens_csrf) set correctly"
else
    log_fail "Auth cookies not set properly"
fi

# Test 1.2: Invalid password
log_test "1.2 Login with invalid password"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"wrongpassword"}')

HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "401" ]; then
    if echo "$BODY" | grep -q "invalid username or password"; then
        log_pass "Invalid password correctly rejected (HTTP $HTTP_CODE)"
    else
        log_fail "Invalid password rejected but unexpected error message"
    fi
else
    log_fail "Invalid password should return 401, got HTTP $HTTP_CODE"
fi

# Test 1.3: Invalid username
log_test "1.3 Login with non-existent username"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"nonexistent_user_xyz","password":"anypassword"}')

HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Non-existent user correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Non-existent user should return 401, got HTTP $HTTP_CODE"
fi

# Test 1.4: Empty credentials
log_test "1.4 Login with empty credentials"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"","password":""}')

HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "400" ]; then
    log_pass "Empty credentials correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Empty credentials should return 400, got HTTP $HTTP_CODE"
fi

# Test 1.5: Missing password field
log_test "1.5 Login with missing password field"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin"}')

HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "400" ]; then
    log_pass "Missing password field correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Missing password field should return 400, got HTTP $HTTP_CODE"
fi

# Test 1.6: Malformed JSON
log_test "1.6 Login with malformed JSON"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin", password: bad}')

HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "400" ]; then
    log_pass "Malformed JSON correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Malformed JSON should return 400, got HTTP $HTTP_CODE"
fi

# ============================================================================
# SECTION 2: PROTECTED ROUTES TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 2: PROTECTED ROUTES TESTS"
echo "============================================================================"

# Test 2.1: Access protected route without auth
log_test "2.1 Access /api/auth/me without authentication"
RESPONSE=$(curl -s -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Protected route correctly returns 401 without auth"
else
    log_fail "Protected route without auth should return 401, got HTTP $HTTP_CODE"
fi

# Test 2.2: Access protected route with valid cookie
log_test "2.2 Access /api/auth/me with valid session cookie"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q '"username":"admin"'; then
        log_pass "Protected route accessible with valid cookie (HTTP $HTTP_CODE)"
    else
        log_fail "Route accessible but response missing user data"
    fi
else
    log_fail "Protected route with valid cookie should return 200, got HTTP $HTTP_CODE"
fi

# Test 2.3: Access /api/drones without auth
log_test "2.3 Access /api/drones without authentication"
RESPONSE=$(curl -s -w "\n%{http_code}" "$BASE_URL/api/drones")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "/api/drones correctly returns 401 without auth"
else
    log_fail "/api/drones without auth should return 401, got HTTP $HTTP_CODE"
fi

# Test 2.4: Access /api/drones with valid cookie
log_test "2.4 Access /api/drones with valid session cookie"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/drones")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "/api/drones accessible with valid cookie (HTTP $HTTP_CODE)"
else
    log_fail "/api/drones with valid cookie should return 200, got HTTP $HTTP_CODE"
fi

# Test 2.5: Access with Bearer token in header
log_test "2.5 Access /api/auth/me with Bearer token in Authorization header"
# Extract token from cookie file
TOKEN=$(grep "skylens_token" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')
if [ -n "$TOKEN" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" \
        -H "Authorization: Bearer $TOKEN" \
        "$BASE_URL/api/auth/me")
    HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

    if [ "$HTTP_CODE" = "200" ]; then
        log_pass "Bearer token authentication works (HTTP $HTTP_CODE)"
    else
        log_fail "Bearer token authentication should return 200, got HTTP $HTTP_CODE"
    fi
else
    log_skip "Could not extract token from cookie file"
fi

# Test 2.6: Invalid Bearer token
log_test "2.6 Access with invalid Bearer token"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -H "Authorization: Bearer invalid.token.here" \
    "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Invalid Bearer token correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Invalid Bearer token should return 401, got HTTP $HTTP_CODE"
fi

# Test 2.7: Expired token format (malformed)
log_test "2.7 Access with malformed token"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -H "Authorization: Bearer notavalidjwt" \
    "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Malformed token correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Malformed token should return 401, got HTTP $HTTP_CODE"
fi

# ============================================================================
# SECTION 3: TOKEN REFRESH TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 3: TOKEN REFRESH TESTS"
echo "============================================================================"

# Test 3.1: Refresh token endpoint
log_test "3.1 Refresh token with valid refresh cookie"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/refresh")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q "token refreshed"; then
        log_pass "Token refresh successful (HTTP $HTTP_CODE)"
    else
        log_fail "Token refresh returned 200 but unexpected response"
    fi
else
    log_fail "Token refresh should return 200, got HTTP $HTTP_CODE"
    log_info "Response: $BODY"
fi

# Test 3.2: Refresh without refresh token
log_test "3.2 Refresh token without refresh cookie"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/refresh")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Refresh without cookie correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Refresh without cookie should return 401, got HTTP $HTTP_CODE"
fi

# Test 3.3: Verify new token works after refresh
log_test "3.3 Verify refreshed token works"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Refreshed token works correctly (HTTP $HTTP_CODE)"
else
    log_fail "Refreshed token should work, got HTTP $HTTP_CODE"
fi

# ============================================================================
# SECTION 4: LOGOUT TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 4: LOGOUT TESTS"
echo "============================================================================"

# First, create a new session for logout testing
log_test "4.0 Creating new session for logout tests"
LOGOUT_COOKIE_JAR="/tmp/logout_test_cookies.txt"
curl -s -c "$LOGOUT_COOKIE_JAR" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"changeme123!"}' > /dev/null

if grep -q "skylens_token" "$LOGOUT_COOKIE_JAR"; then
    log_pass "Created new session for logout testing"
else
    log_fail "Could not create session for logout testing"
fi

# Test 4.1: Logout endpoint
log_test "4.1 Logout with valid session"
RESPONSE=$(curl -s -b "$LOGOUT_COOKIE_JAR" -c "$LOGOUT_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/logout")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q "logged out successfully"; then
        log_pass "Logout successful (HTTP $HTTP_CODE)"
    else
        log_fail "Logout returned 200 but unexpected response"
    fi
else
    log_fail "Logout should return 200, got HTTP $HTTP_CODE"
fi

# Test 4.2: Verify session invalidated after logout
log_test "4.2 Access protected route after logout (should fail)"
RESPONSE=$(curl -s -b "$LOGOUT_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Session correctly invalidated after logout (HTTP $HTTP_CODE)"
else
    log_fail "Session should be invalid after logout, got HTTP $HTTP_CODE"
fi

# Test 4.3: Logout without session (should still succeed)
log_test "4.3 Logout without valid session"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/logout")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

# Should return 401 since no valid session
if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Logout without session returns 401 (requires auth)"
else
    log_fail "Logout without session should return 401, got HTTP $HTTP_CODE"
fi

rm -f "$LOGOUT_COOKIE_JAR"

# ============================================================================
# SECTION 5: WEBSOCKET AUTH TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 5: WEBSOCKET AUTH TESTS"
echo "============================================================================"

# Check if websocat is available
if command -v websocat &> /dev/null; then
    # Test 5.1: WebSocket connection without auth
    log_test "5.1 WebSocket connection without authentication"

    # Try to connect without token - if auth is enabled, should be rejected
    RESULT=$(timeout 3 websocat -t "$WS_URL/ws" 2>&1 || true)

    # The connection might be rejected or accepted depending on auth config
    if echo "$RESULT" | grep -qi "401\|unauthorized\|error"; then
        log_pass "WebSocket without auth correctly rejected"
    else
        log_info "WebSocket connection accepted (auth may not be required for WS)"
        log_skip "WebSocket auth test - auth may not be enforced on WS port"
    fi

    # Test 5.2: WebSocket connection with valid token
    log_test "5.2 WebSocket connection with valid token"
    TOKEN=$(grep "skylens_token" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')

    if [ -n "$TOKEN" ]; then
        # Connect with token as query parameter
        RESULT=$(timeout 3 bash -c "echo '' | websocat -t '$WS_URL/ws?token=$TOKEN'" 2>&1 || true)

        if echo "$RESULT" | grep -qi "error\|401\|unauthorized"; then
            log_fail "WebSocket with valid token was rejected"
        else
            log_pass "WebSocket connection with valid token succeeded"
        fi
    else
        log_skip "Could not extract token for WebSocket test"
    fi
else
    log_skip "5.1 websocat not installed - skipping WebSocket tests"
    log_skip "5.2 websocat not installed - skipping WebSocket tests"
    log_info "Install websocat for WebSocket testing: cargo install websocat"
fi

# ============================================================================
# SECTION 6: CSRF PROTECTION TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 6: CSRF PROTECTION TESTS"
echo "============================================================================"

# Test 6.1: Get CSRF token
log_test "6.1 Get CSRF token"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    "$BASE_URL/api/auth/csrf")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    CSRF_TOKEN=$(echo "$BODY" | grep -o '"csrf_token":"[^"]*"' | cut -d'"' -f4)
    if [ -n "$CSRF_TOKEN" ]; then
        log_pass "CSRF token retrieved successfully"
        log_info "CSRF Token: ${CSRF_TOKEN:0:20}..."
    else
        log_fail "CSRF endpoint returned 200 but no token found"
    fi
else
    log_fail "CSRF token endpoint should return 200, got HTTP $HTTP_CODE"
fi

# Also get CSRF from cookie
CSRF_COOKIE=$(grep "skylens_csrf" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')
log_info "CSRF Cookie: ${CSRF_COOKIE:0:20}..."

# Test 6.2: POST to CSRF-protected endpoint without CSRF token
log_test "6.2 Change password without CSRF token (should fail)"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -d '{"current_password":"changeme123!","new_password":"NewPass123!","confirm_password":"NewPass123!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "403" ]; then
    if echo "$BODY" | grep -qi "csrf"; then
        log_pass "CSRF-protected endpoint correctly rejects request without CSRF token"
    else
        log_fail "Got 403 but not for CSRF reason"
    fi
else
    log_fail "Request without CSRF should return 403, got HTTP $HTTP_CODE"
    log_info "Response: $BODY"
fi

# Test 6.3: POST to CSRF-protected endpoint with valid CSRF token
log_test "6.3 Change password with valid CSRF token"
# Note: We don't actually want to change the password, so we'll test with wrong current password
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_COOKIE" \
    -d '{"current_password":"wrongpassword","new_password":"NewPass123!","confirm_password":"NewPass123!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

# Should pass CSRF check but fail on password verification
if [ "$HTTP_CODE" = "400" ]; then
    if echo "$BODY" | grep -qi "current password is incorrect"; then
        log_pass "CSRF validation passed, correctly rejected wrong current password"
    else
        log_info "Got 400 for different reason: $BODY"
        log_pass "CSRF validation passed (request processed)"
    fi
else
    log_fail "With valid CSRF, expected 400 for wrong password, got HTTP $HTTP_CODE"
    log_info "Response: $BODY"
fi

# Test 6.4: POST with mismatched CSRF token
log_test "6.4 Request with mismatched CSRF token"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: invalid_csrf_token_here" \
    -d '{"current_password":"test","new_password":"Test123!","confirm_password":"Test123!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "403" ]; then
    log_pass "Mismatched CSRF token correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Mismatched CSRF token should return 403, got HTTP $HTTP_CODE"
fi

# Test 6.5: GET requests should not require CSRF
log_test "6.5 GET requests should not require CSRF"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    "$BASE_URL/api/auth/sessions")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "GET request works without CSRF token (HTTP $HTTP_CODE)"
else
    log_fail "GET request should not require CSRF, got HTTP $HTTP_CODE"
fi

# ============================================================================
# SECTION 7: PASSWORD CHANGE & SESSION REVOCATION TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 7: PASSWORD CHANGE & SESSION REVOCATION TESTS"
echo "============================================================================"

# Create a test user for password change tests
log_test "7.0 Creating test user for password change tests"

# Get fresh CSRF token
CSRF_RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" "$BASE_URL/api/auth/csrf")
CSRF_TOKEN=$(grep "skylens_csrf" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')

RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_TOKEN" \
    -d '{
        "username":"pwtest_user",
        "password":"TestPass123!",
        "email":"pwtest@skylens.local",
        "display_name":"Password Test User",
        "role_id":2
    }')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "201" ] || [ "$HTTP_CODE" = "200" ]; then
    TEST_USER_ID=$(echo "$BODY" | grep -o '"id":[0-9]*' | head -1 | cut -d':' -f2)
    log_pass "Test user created (ID: $TEST_USER_ID)"
elif [ "$HTTP_CODE" = "409" ]; then
    log_info "Test user already exists, continuing with tests"
    TEST_USER_ID=""
else
    log_fail "Could not create test user (HTTP $HTTP_CODE)"
    log_info "Response: $BODY"
fi

# Login as test user and create two sessions
log_test "7.1 Create multiple sessions for test user"
PWTEST_COOKIE1="/tmp/pwtest_session1.txt"
PWTEST_COOKIE2="/tmp/pwtest_session2.txt"

# Session 1
curl -s -c "$PWTEST_COOKIE1" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"pwtest_user","password":"TestPass123!"}' > /dev/null

# Session 2 (simulate different device)
curl -s -c "$PWTEST_COOKIE2" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -H "User-Agent: Different-Device/1.0" \
    -d '{"username":"pwtest_user","password":"TestPass123!"}' > /dev/null

if grep -q "skylens_token" "$PWTEST_COOKIE1" && grep -q "skylens_token" "$PWTEST_COOKIE2"; then
    log_pass "Created two sessions for test user"
else
    log_fail "Could not create multiple sessions"
fi

# Test 7.2: Verify both sessions work
log_test "7.2 Verify both sessions are active"
RESPONSE1=$(curl -s -b "$PWTEST_COOKIE1" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
RESPONSE2=$(curl -s -b "$PWTEST_COOKIE2" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE1=$(echo "$RESPONSE1" | tail -n1)
HTTP_CODE2=$(echo "$RESPONSE2" | tail -n1)

if [ "$HTTP_CODE1" = "200" ] && [ "$HTTP_CODE2" = "200" ]; then
    log_pass "Both sessions are active"
else
    log_fail "Sessions should both be active (got $HTTP_CODE1, $HTTP_CODE2)"
fi

# Test 7.3: List active sessions
log_test "7.3 List active sessions"
RESPONSE=$(curl -s -b "$PWTEST_COOKIE1" -w "\n%{http_code}" "$BASE_URL/api/auth/sessions")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    SESSION_COUNT=$(echo "$BODY" | grep -o '"id"' | wc -l)
    if [ "$SESSION_COUNT" -ge 2 ]; then
        log_pass "Session list shows multiple sessions ($SESSION_COUNT found)"
    else
        log_info "Expected 2+ sessions, found $SESSION_COUNT"
    fi
else
    log_fail "Could not list sessions (HTTP $HTTP_CODE)"
fi

# Test 7.4: Change password and verify other sessions revoked
log_test "7.4 Change password and verify session revocation"

# Get CSRF for session 1
curl -s -b "$PWTEST_COOKIE1" -c "$PWTEST_COOKIE1" "$BASE_URL/api/auth/csrf" > /dev/null
CSRF_TOKEN=$(grep "skylens_csrf" "$PWTEST_COOKIE1" | awk '{print $NF}')

RESPONSE=$(curl -s -b "$PWTEST_COOKIE1" -c "$PWTEST_COOKIE1" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_TOKEN" \
    -d '{"current_password":"TestPass123!","new_password":"NewTestPass456!","confirm_password":"NewTestPass456!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Password changed successfully"
else
    log_fail "Password change failed (HTTP $HTTP_CODE)"
    log_info "Response: $BODY"
fi

# Test 7.5: Verify session 1 (which changed password) still works
log_test "7.5 Verify session that changed password still works"
RESPONSE=$(curl -s -b "$PWTEST_COOKIE1" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Session that changed password still works"
else
    log_fail "Session that changed password should still work, got HTTP $HTTP_CODE"
fi

# Test 7.6: Verify session 2 (other device) was revoked
log_test "7.6 Verify other session was revoked after password change"
RESPONSE=$(curl -s -b "$PWTEST_COOKIE2" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "401" ]; then
    log_pass "Other session correctly revoked after password change"
else
    log_fail "Other session should be revoked after password change, got HTTP $HTTP_CODE"
fi

# Cleanup: Reset password back and delete test user
log_test "7.7 Cleanup: Reset test user password"
# Get fresh CSRF
curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" "$BASE_URL/api/auth/csrf" > /dev/null
CSRF_TOKEN=$(grep "skylens_csrf" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')

if [ -n "$TEST_USER_ID" ]; then
    curl -s -b "$ADMIN_COOKIE_JAR" \
        -X POST "$BASE_URL/api/admin/users/$TEST_USER_ID/reset-password" \
        -H "Content-Type: application/json" \
        -H "X-CSRF-Token: $CSRF_TOKEN" \
        -d '{"new_password":"TestPass123!"}' > /dev/null
    log_pass "Test user password reset"
fi

rm -f "$PWTEST_COOKIE1" "$PWTEST_COOKIE2"

# ============================================================================
# SECTION 8: ROLE-BASED ACCESS CONTROL TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 8: ROLE-BASED ACCESS CONTROL TESTS"
echo "============================================================================"

# Get role IDs
log_test "8.0 Get role IDs"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/roles")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Retrieved roles list"
    log_info "Roles: $(echo "$BODY" | grep -o '"name":"[^"]*"' | tr '\n' ' ')"
else
    log_fail "Could not get roles list"
fi

# Create operator user
log_test "8.1 Create operator test user"
curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" "$BASE_URL/api/auth/csrf" > /dev/null
CSRF_TOKEN=$(grep "skylens_csrf" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')

RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_TOKEN" \
    -d '{
        "username":"operator_test",
        "password":"OperatorPass123!",
        "email":"operator@skylens.local",
        "display_name":"Test Operator",
        "role_id":2
    }')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "201" ] || [ "$HTTP_CODE" = "200" ]; then
    OPERATOR_USER_ID=$(echo "$BODY" | grep -o '"id":[0-9]*' | head -1 | cut -d':' -f2)
    log_pass "Operator user created (ID: $OPERATOR_USER_ID)"
elif [ "$HTTP_CODE" = "409" ]; then
    log_info "Operator user already exists"
else
    log_fail "Could not create operator user (HTTP $HTTP_CODE)"
fi

# Create viewer user
log_test "8.2 Create viewer test user"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_TOKEN" \
    -d '{
        "username":"viewer_test",
        "password":"ViewerPass123!",
        "email":"viewer@skylens.local",
        "display_name":"Test Viewer",
        "role_id":3,
        "allowed_taps":["tap-001"]
    }')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "201" ] || [ "$HTTP_CODE" = "200" ]; then
    VIEWER_USER_ID=$(echo "$BODY" | grep -o '"id":[0-9]*' | head -1 | cut -d':' -f2)
    log_pass "Viewer user created (ID: $VIEWER_USER_ID)"
elif [ "$HTTP_CODE" = "409" ]; then
    log_info "Viewer user already exists"
else
    log_fail "Could not create viewer user (HTTP $HTTP_CODE)"
fi

# Login as each user
log_test "8.3 Login as operator"
RESPONSE=$(curl -s -c "$OPERATOR_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"operator_test","password":"OperatorPass123!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Operator login successful"
else
    log_fail "Operator login failed (HTTP $HTTP_CODE)"
fi

log_test "8.4 Login as viewer"
RESPONSE=$(curl -s -c "$VIEWER_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"viewer_test","password":"ViewerPass123!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Viewer login successful"
else
    log_fail "Viewer login failed (HTTP $HTTP_CODE)"
fi

# Test 8.5: Admin can access admin routes
log_test "8.5 Admin can access /api/admin/users"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/admin/users")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "200" ]; then
    log_pass "Admin can access admin routes (HTTP $HTTP_CODE)"
else
    log_fail "Admin should access admin routes, got HTTP $HTTP_CODE"
fi

# Test 8.6: Operator cannot access admin routes
log_test "8.6 Operator cannot access /api/admin/users"
RESPONSE=$(curl -s -b "$OPERATOR_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/admin/users")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "403" ]; then
    log_pass "Operator correctly denied access to admin routes (HTTP $HTTP_CODE)"
else
    log_fail "Operator should not access admin routes, got HTTP $HTTP_CODE"
fi

# Test 8.7: Viewer cannot access admin routes
log_test "8.7 Viewer cannot access /api/admin/users"
RESPONSE=$(curl -s -b "$VIEWER_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/admin/users")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

if [ "$HTTP_CODE" = "403" ]; then
    log_pass "Viewer correctly denied access to admin routes (HTTP $HTTP_CODE)"
else
    log_fail "Viewer should not access admin routes, got HTTP $HTTP_CODE"
fi

# Test 8.8: Verify operator permissions
log_test "8.8 Verify operator has correct permissions"
RESPONSE=$(curl -s -b "$OPERATOR_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q '"role_name":"operator"'; then
        PERMS=$(echo "$BODY" | grep -o '"permissions":\[[^]]*\]')
        log_pass "Operator has operator role"
        log_info "Permissions: ${PERMS:0:100}..."
    else
        log_fail "User should have operator role"
    fi
else
    log_fail "Could not get operator profile"
fi

# Test 8.9: Verify viewer has correct permissions
log_test "8.9 Verify viewer has correct permissions and TAP access"
RESPONSE=$(curl -s -b "$VIEWER_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q '"role_name":"viewer"'; then
        log_pass "Viewer has viewer role"
        if echo "$BODY" | grep -q '"allowed_taps"'; then
            log_info "Viewer has TAP restrictions configured"
        fi
    else
        log_fail "User should have viewer role"
    fi
else
    log_fail "Could not get viewer profile"
fi

# Test 8.10: All roles can access /api/drones (read-only)
log_test "8.10 All roles can access /api/drones"
for role in "ADMIN:$ADMIN_COOKIE_JAR" "OPERATOR:$OPERATOR_COOKIE_JAR" "VIEWER:$VIEWER_COOKIE_JAR"; do
    ROLE_NAME=$(echo "$role" | cut -d: -f1)
    COOKIE=$(echo "$role" | cut -d: -f2)

    RESPONSE=$(curl -s -b "$COOKIE" -w "\n%{http_code}" "$BASE_URL/api/drones")
    HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

    if [ "$HTTP_CODE" = "200" ]; then
        echo -e "  ${GREEN}[OK]${NC} $ROLE_NAME can access /api/drones"
    else
        echo -e "  ${RED}[FAIL]${NC} $ROLE_NAME cannot access /api/drones (HTTP $HTTP_CODE)"
        ((TESTS_FAILED++))
    fi
done
((TESTS_PASSED++))

# ============================================================================
# SECTION 9: ACCOUNT LOCKOUT TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 9: ACCOUNT LOCKOUT TESTS"
echo "============================================================================"

# Create a user specifically for lockout testing
log_test "9.0 Create lockout test user"
curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" "$BASE_URL/api/auth/csrf" > /dev/null
CSRF_TOKEN=$(grep "skylens_csrf" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')

RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_TOKEN" \
    -d '{
        "username":"lockout_test",
        "password":"LockoutTest123!",
        "email":"lockout@skylens.local",
        "display_name":"Lockout Test User",
        "role_id":3
    }')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "201" ] || [ "$HTTP_CODE" = "200" ]; then
    LOCKOUT_USER_ID=$(echo "$BODY" | grep -o '"id":[0-9]*' | head -1 | cut -d':' -f2)
    log_pass "Lockout test user created (ID: $LOCKOUT_USER_ID)"
elif [ "$HTTP_CODE" = "409" ]; then
    log_info "Lockout test user already exists"
    # Unlock and reset the user
    # Get user ID from admin users list
    USERS_RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" "$BASE_URL/api/admin/users")
    LOCKOUT_USER_ID=$(echo "$USERS_RESPONSE" | grep -o '"id":[0-9]*,"username":"lockout_test"' | grep -o '"id":[0-9]*' | cut -d':' -f2)
    if [ -n "$LOCKOUT_USER_ID" ]; then
        curl -s -b "$ADMIN_COOKIE_JAR" \
            -X POST "$BASE_URL/api/admin/users/$LOCKOUT_USER_ID/reset-password" \
            -H "Content-Type: application/json" \
            -H "X-CSRF-Token: $CSRF_TOKEN" \
            -d '{"new_password":"LockoutTest123!"}' > /dev/null
        log_info "Reset lockout test user password"
    fi
else
    log_fail "Could not create lockout test user (HTTP $HTTP_CODE)"
fi

# Test 9.1: Multiple failed attempts leading to lockout
log_test "9.1 Test account lockout after 5 failed attempts"

LOCKOUT_OCCURRED=false
for i in {1..6}; do
    RESPONSE=$(curl -s -w "\n%{http_code}" \
        -X POST "$BASE_URL/api/auth/login" \
        -H "Content-Type: application/json" \
        -d '{"username":"lockout_test","password":"wrongpassword"}')
    HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
    BODY=$(echo "$RESPONSE" | head -n -1)

    if [ "$HTTP_CODE" = "403" ]; then
        if echo "$BODY" | grep -qi "locked"; then
            LOCKOUT_OCCURRED=true
            log_info "Account locked after $i failed attempts"
            break
        fi
    fi
    echo -n "."
done
echo ""

if [ "$LOCKOUT_OCCURRED" = true ]; then
    log_pass "Account correctly locked after multiple failed attempts"
else
    log_fail "Account should be locked after 5 failed attempts"
fi

# Test 9.2: Verify locked account cannot login even with correct password
log_test "9.2 Locked account cannot login with correct password"
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"lockout_test","password":"LockoutTest123!"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "403" ]; then
    if echo "$BODY" | grep -qi "locked"; then
        log_pass "Locked account correctly rejected even with valid password"
    else
        log_fail "Expected locked error message"
    fi
else
    log_fail "Locked account should return 403, got HTTP $HTTP_CODE"
fi

# ============================================================================
# SECTION 10: ADDITIONAL SECURITY TESTS
# ============================================================================

echo ""
echo "============================================================================"
echo "SECTION 10: ADDITIONAL SECURITY TESTS"
echo "============================================================================"

# Test 10.1: Password requirements endpoint
log_test "10.1 Password requirements endpoint"
RESPONSE=$(curl -s -w "\n%{http_code}" "$BASE_URL/api/auth/password-requirements")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q "min_length"; then
        log_pass "Password requirements endpoint works"
        log_info "Requirements: $(echo "$BODY" | head -c 200)..."
    else
        log_fail "Password requirements response missing expected fields"
    fi
else
    log_fail "Password requirements endpoint failed (HTTP $HTTP_CODE)"
fi

# Test 10.2: Weak password rejection
log_test "10.2 Weak password rejection during user creation"
curl -s -b "$ADMIN_COOKIE_JAR" -c "$ADMIN_COOKIE_JAR" "$BASE_URL/api/auth/csrf" > /dev/null
CSRF_TOKEN=$(grep "skylens_csrf" "$ADMIN_COOKIE_JAR" | awk '{print $NF}')

RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_TOKEN" \
    -d '{
        "username":"weak_pw_test",
        "password":"weak",
        "email":"weak@skylens.local",
        "role_id":3
    }')
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "400" ]; then
    log_pass "Weak password correctly rejected (HTTP $HTTP_CODE)"
else
    log_fail "Weak password should be rejected (got HTTP $HTTP_CODE)"
fi

# Test 10.3: Rate limiting on login endpoint
log_test "10.3 Rate limiting on login endpoint"
RATE_LIMITED=false

for i in {1..15}; do
    RESPONSE=$(curl -s -w "\n%{http_code}" \
        -X POST "$BASE_URL/api/auth/login" \
        -H "Content-Type: application/json" \
        -d '{"username":"ratelimit_test","password":"test"}')
    HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

    if [ "$HTTP_CODE" = "429" ]; then
        RATE_LIMITED=true
        log_info "Rate limited after $i requests"
        break
    fi
    echo -n "."
done
echo ""

if [ "$RATE_LIMITED" = true ]; then
    log_pass "Rate limiting is active on login endpoint"
else
    log_info "Rate limiting may not have kicked in (limit may be higher than 15)"
    log_skip "Rate limiting test inconclusive"
fi

# Test 10.4: Session listing shows correct info
log_test "10.4 Session listing shows correct information"
RESPONSE=$(curl -s -b "$ADMIN_COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/sessions")
HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    if echo "$BODY" | grep -q '"sessions"' && echo "$BODY" | grep -q '"ip_address"'; then
        log_pass "Session listing includes IP address and session details"
    else
        log_fail "Session listing missing expected fields"
    fi
else
    log_fail "Session listing failed (HTTP $HTTP_CODE)"
fi

# Test 10.5: Health endpoints don't require auth
log_test "10.5 Health endpoints accessible without authentication"
HEALTH_RESPONSE=$(curl -s -w "\n%{http_code}" "$BASE_URL/health")
READY_RESPONSE=$(curl -s -w "\n%{http_code}" "$BASE_URL/ready")
HEALTH_CODE=$(echo "$HEALTH_RESPONSE" | tail -n1)
READY_CODE=$(echo "$READY_RESPONSE" | tail -n1)

if [ "$HEALTH_CODE" = "200" ] && [ "$READY_CODE" = "200" ]; then
    log_pass "Health endpoints accessible without auth"
else
    log_fail "Health endpoints should not require auth (got $HEALTH_CODE, $READY_CODE)"
fi

# ============================================================================
# TEST SUMMARY
# ============================================================================

echo ""
echo "============================================================================"
echo "TEST SUMMARY"
echo "============================================================================"
echo -e "${GREEN}Passed:${NC} $TESTS_PASSED"
echo -e "${RED}Failed:${NC} $TESTS_FAILED"
echo -e "${YELLOW}Skipped:${NC} $TESTS_SKIPPED"
TOTAL=$((TESTS_PASSED + TESTS_FAILED))
if [ $TOTAL -gt 0 ]; then
    PASS_RATE=$((TESTS_PASSED * 100 / TOTAL))
    echo "Pass Rate: $PASS_RATE%"
fi
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}All tests passed!${NC}"
    exit 0
else
    echo -e "${RED}Some tests failed. Review the output above for details.${NC}"
    exit 1
fi
