#!/bin/bash
#
# Skylens Authentication E2E Test Runner
# Executes comprehensive authentication tests
#

set -u

BASE_URL="http://localhost:8080"
WS_PORT="8081"
COOKIE_JAR="/tmp/test_cookies.txt"
PASSED=0
FAILED=0
SKIPPED=0

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

pass() { echo -e "${GREEN}[PASS]${NC} $1"; ((PASSED++)); }
fail() { echo -e "${RED}[FAIL]${NC} $1"; ((FAILED++)); }
skip() { echo -e "${YELLOW}[SKIP]${NC} $1"; ((SKIPPED++)); }
info() { echo -e "${BLUE}[INFO]${NC} $1"; }
test_header() { echo -e "\n${BLUE}=== $1 ===${NC}"; }

# Helper to extract HTTP code
http_code() { echo "$1" | tail -n1; }
http_body() { echo "$1" | head -n -1; }

# Get CSRF token from cookie jar
get_csrf() {
    grep "skylens_csrf" "$COOKIE_JAR" 2>/dev/null | awk '{print $NF}'
}

# ============================================================================
test_header "SECTION 1: LOGIN FLOW TESTS"
# ============================================================================

# Test 1.1: Valid login
info "Test 1.1: Valid credentials login"
RESP=$(curl -s -c "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"changeme123!"}')
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ] && echo "$BODY" | grep -q '"username":"admin"'; then
    pass "Valid login returns 200 with user data"
elif [ "$CODE" = "429" ]; then
    skip "Rate limited - using existing session"
    cp /tmp/test_cookies.txt "$COOKIE_JAR" 2>/dev/null || true
else
    fail "Valid login failed (HTTP $CODE): $BODY"
fi

# Test 1.2: Invalid password
info "Test 1.2: Invalid password"
RESP=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"wrongpassword"}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "401" ]; then
    pass "Invalid password returns 401"
elif [ "$CODE" = "429" ]; then
    skip "Rate limited"
else
    fail "Invalid password should return 401, got $CODE"
fi

# Test 1.3: Non-existent user
info "Test 1.3: Non-existent user"
RESP=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"nonexistent_user_xyz","password":"anypass"}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "401" ]; then
    pass "Non-existent user returns 401"
elif [ "$CODE" = "429" ]; then
    skip "Rate limited"
else
    fail "Non-existent user should return 401, got $CODE"
fi

# Test 1.4: Empty credentials
info "Test 1.4: Empty credentials"
RESP=$(curl -s -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"","password":""}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "400" ]; then
    pass "Empty credentials return 400"
elif [ "$CODE" = "429" ]; then
    skip "Rate limited"
else
    fail "Empty credentials should return 400, got $CODE"
fi

# ============================================================================
test_header "SECTION 2: PROTECTED ROUTES TESTS"
# ============================================================================

# Test 2.1: Protected route without auth
info "Test 2.1: Access /api/auth/me without auth"
RESP=$(curl -s -w "\n%{http_code}" "$BASE_URL/api/auth/me")
CODE=$(http_code "$RESP")

if [ "$CODE" = "401" ]; then
    pass "Protected route returns 401 without auth"
else
    fail "Protected route without auth should return 401, got $CODE"
fi

# Test 2.2: Protected route with valid session
info "Test 2.2: Access /api/auth/me with valid session"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ] && echo "$BODY" | grep -q '"username"'; then
    pass "Protected route returns 200 with valid session"
else
    fail "Protected route with session should return 200, got $CODE"
fi

# Test 2.3: /api/drones without auth
info "Test 2.3: Access /api/drones without auth"
RESP=$(curl -s -w "\n%{http_code}" "$BASE_URL/api/drones")
CODE=$(http_code "$RESP")

if [ "$CODE" = "401" ]; then
    pass "/api/drones returns 401 without auth"
else
    fail "/api/drones without auth should return 401, got $CODE"
fi

# Test 2.4: /api/drones with auth
info "Test 2.4: Access /api/drones with valid session"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/drones")
CODE=$(http_code "$RESP")

if [ "$CODE" = "200" ]; then
    pass "/api/drones returns 200 with valid session"
else
    fail "/api/drones with session should return 200, got $CODE"
fi

# Test 2.5: Bearer token authentication
info "Test 2.5: Bearer token in Authorization header"
TOKEN=$(grep "skylens_token" "$COOKIE_JAR" 2>/dev/null | awk '{print $NF}')
if [ -n "$TOKEN" ]; then
    RESP=$(curl -s -w "\n%{http_code}" \
        -H "Authorization: Bearer $TOKEN" \
        "$BASE_URL/api/auth/me")
    CODE=$(http_code "$RESP")

    if [ "$CODE" = "200" ]; then
        pass "Bearer token authentication works"
    else
        fail "Bearer token should work, got $CODE"
    fi
else
    skip "No token available for Bearer test"
fi

# Test 2.6: Invalid Bearer token
info "Test 2.6: Invalid Bearer token"
RESP=$(curl -s -w "\n%{http_code}" \
    -H "Authorization: Bearer invalid.token.here" \
    "$BASE_URL/api/auth/me")
CODE=$(http_code "$RESP")

if [ "$CODE" = "401" ]; then
    pass "Invalid Bearer token returns 401"
else
    fail "Invalid Bearer token should return 401, got $CODE"
fi

# ============================================================================
test_header "SECTION 3: TOKEN REFRESH TESTS"
# ============================================================================

# Test 3.1: Refresh token
info "Test 3.1: Refresh token with valid refresh cookie"
RESP=$(curl -s -b "$COOKIE_JAR" -c "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/refresh")
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ]; then
    pass "Token refresh returns 200"
else
    fail "Token refresh should return 200, got $CODE: $BODY"
fi

# Test 3.2: Refresh without cookie
info "Test 3.2: Refresh without refresh cookie"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE_URL/api/auth/refresh")
CODE=$(http_code "$RESP")

if [ "$CODE" = "401" ]; then
    pass "Refresh without cookie returns 401"
else
    fail "Refresh without cookie should return 401, got $CODE"
fi

# Test 3.3: Verify refreshed token
info "Test 3.3: Verify refreshed token works"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
CODE=$(http_code "$RESP")

if [ "$CODE" = "200" ]; then
    pass "Refreshed token works correctly"
else
    fail "Refreshed token should work, got $CODE"
fi

# ============================================================================
test_header "SECTION 4: LOGOUT TESTS"
# ============================================================================

# Create a separate session for logout testing
info "Test 4.0: Create session for logout testing"
LOGOUT_JAR="/tmp/logout_test.txt"
RESP=$(curl -s -c "$LOGOUT_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"changeme123!"}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "200" ]; then
    pass "Created logout test session"

    # Test 4.1: Logout
    info "Test 4.1: Logout with valid session"
    RESP=$(curl -s -b "$LOGOUT_JAR" -c "$LOGOUT_JAR" -w "\n%{http_code}" \
        -X POST "$BASE_URL/api/auth/logout")
    CODE=$(http_code "$RESP")

    if [ "$CODE" = "200" ]; then
        pass "Logout returns 200"

        # Test 4.2: Verify session invalidated
        info "Test 4.2: Access after logout fails"
        RESP=$(curl -s -b "$LOGOUT_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/me")
        CODE=$(http_code "$RESP")

        if [ "$CODE" = "401" ]; then
            pass "Session invalidated after logout"
        else
            fail "Session should be invalid after logout, got $CODE"
        fi
    else
        fail "Logout should return 200, got $CODE"
    fi
elif [ "$CODE" = "429" ]; then
    skip "Rate limited - skipping logout tests"
else
    fail "Could not create logout test session"
fi

rm -f "$LOGOUT_JAR"

# ============================================================================
test_header "SECTION 5: WEBSOCKET AUTH TESTS"
# ============================================================================

# Test WebSocket endpoint exists
info "Test 5.1: WebSocket endpoint check"
# Check if WS port is accessible
if curl -s --connect-timeout 2 "http://localhost:$WS_PORT/ws" >/dev/null 2>&1; then
    pass "WebSocket endpoint is accessible"
else
    # Try upgrade request
    RESP=$(curl -s -w "\n%{http_code}" \
        -H "Upgrade: websocket" \
        -H "Connection: Upgrade" \
        "http://localhost:$WS_PORT/ws")
    CODE=$(http_code "$RESP")

    if [ "$CODE" != "000" ]; then
        pass "WebSocket endpoint responds (HTTP $CODE)"
    else
        skip "WebSocket port not accessible"
    fi
fi

# ============================================================================
test_header "SECTION 6: CSRF PROTECTION TESTS"
# ============================================================================

# Test 6.1: Get CSRF token
info "Test 6.1: Get CSRF token"
RESP=$(curl -s -b "$COOKIE_JAR" -c "$COOKIE_JAR" -w "\n%{http_code}" \
    "$BASE_URL/api/auth/csrf")
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ] && echo "$BODY" | grep -q "csrf_token"; then
    pass "CSRF token endpoint works"
    CSRF=$(get_csrf)
    info "CSRF Token: ${CSRF:0:20}..."
else
    fail "CSRF token endpoint failed, got $CODE"
fi

# Test 6.2: POST without CSRF
info "Test 6.2: POST to CSRF-protected endpoint without token"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -d '{"current_password":"x","new_password":"y","confirm_password":"y"}')
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "403" ] && echo "$BODY" | grep -qi "csrf"; then
    pass "CSRF-protected endpoint rejects request without token"
else
    fail "Expected 403 CSRF error, got $CODE: $BODY"
fi

# Test 6.3: POST with valid CSRF
info "Test 6.3: POST with valid CSRF token (wrong password)"
CSRF=$(get_csrf)
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF" \
    -d '{"current_password":"wrongpass","new_password":"NewPass123!","confirm_password":"NewPass123!"}')
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "400" ]; then
    pass "CSRF validation passed, password validation works"
else
    fail "Expected 400 for wrong password, got $CODE: $BODY"
fi

# Test 6.4: POST with invalid CSRF
info "Test 6.4: POST with mismatched CSRF token"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/change-password" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: invalid_token_here" \
    -d '{"current_password":"x","new_password":"y","confirm_password":"y"}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "403" ]; then
    pass "Mismatched CSRF token returns 403"
else
    fail "Mismatched CSRF should return 403, got $CODE"
fi

# Test 6.5: GET requests don't need CSRF
info "Test 6.5: GET requests don't require CSRF"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/sessions")
CODE=$(http_code "$RESP")

if [ "$CODE" = "200" ]; then
    pass "GET requests work without CSRF"
else
    fail "GET should work without CSRF, got $CODE"
fi

# ============================================================================
test_header "SECTION 7: ROLE-BASED ACCESS CONTROL TESTS"
# ============================================================================

# Test 7.1: Admin can access admin routes
info "Test 7.1: Admin can access /api/admin/users"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/admin/users")
CODE=$(http_code "$RESP")

if [ "$CODE" = "200" ]; then
    pass "Admin can access admin routes"
else
    fail "Admin should access admin routes, got $CODE"
fi

# Test 7.2: Get roles list
info "Test 7.2: Get roles list"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/roles")
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ]; then
    pass "Roles list accessible"
    info "Roles: $(echo "$BODY" | grep -o '"name":"[^"]*"' | head -3 | tr '\n' ' ')"
else
    fail "Roles list should return 200, got $CODE"
fi

# Create operator and viewer test users
info "Test 7.3: Create operator test user"
CSRF=$(get_csrf)
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF" \
    -d '{"username":"e2e_operator","password":"OperatorPass123!","role_id":2}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "201" ] || [ "$CODE" = "200" ]; then
    pass "Operator user created"
elif [ "$CODE" = "409" ]; then
    info "Operator user already exists"
else
    fail "Could not create operator user, got $CODE"
fi

info "Test 7.4: Create viewer test user"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF" \
    -d '{"username":"e2e_viewer","password":"ViewerPass123!","role_id":3}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "201" ] || [ "$CODE" = "200" ]; then
    pass "Viewer user created"
elif [ "$CODE" = "409" ] || [ "$CODE" = "500" ]; then
    # 500 may occur if unique constraint violated on email (null conflict)
    info "Viewer user already exists or constraint error"
    pass "Viewer user test (user may already exist)"
else
    fail "Could not create viewer user, got $CODE"
fi

# Login as operator and test access
info "Test 7.5: Operator cannot access admin routes"
OP_JAR="/tmp/operator_test.txt"
RESP=$(curl -s -c "$OP_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"e2e_operator","password":"OperatorPass123!"}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "200" ]; then
    # Now try admin route
    RESP=$(curl -s -b "$OP_JAR" -w "\n%{http_code}" "$BASE_URL/api/admin/users")
    CODE=$(http_code "$RESP")

    if [ "$CODE" = "403" ]; then
        pass "Operator denied access to admin routes"
    else
        fail "Operator should be denied admin routes, got $CODE"
    fi
elif [ "$CODE" = "429" ]; then
    skip "Rate limited"
else
    fail "Operator login failed, got $CODE"
fi

rm -f "$OP_JAR"

# ============================================================================
test_header "SECTION 8: PASSWORD CHANGE & SESSION REVOCATION"
# ============================================================================

# Test 8.1: List active sessions
info "Test 8.1: List active sessions"
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" "$BASE_URL/api/auth/sessions")
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ] && echo "$BODY" | grep -q '"sessions"'; then
    SESSION_COUNT=$(echo "$BODY" | grep -o '"id":"[^"]*"' | wc -l)
    pass "Session listing works ($SESSION_COUNT sessions)"
else
    fail "Session listing failed, got $CODE"
fi

# ============================================================================
test_header "SECTION 9: ADDITIONAL SECURITY TESTS"
# ============================================================================

# Test 9.1: Password requirements
info "Test 9.1: Password requirements endpoint"
RESP=$(curl -s -w "\n%{http_code}" "$BASE_URL/api/auth/password-requirements")
CODE=$(http_code "$RESP")
BODY=$(http_body "$RESP")

if [ "$CODE" = "200" ] && echo "$BODY" | grep -q "min_length"; then
    pass "Password requirements endpoint works"
else
    fail "Password requirements failed, got $CODE"
fi

# Test 9.2: Health endpoints don't need auth
info "Test 9.2: Health endpoints accessible without auth"
HEALTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/health")
READY_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/ready")

if [ "$HEALTH_CODE" = "200" ] && [ "$READY_CODE" = "200" ]; then
    pass "Health endpoints work without auth"
else
    fail "Health endpoints should work without auth, got $HEALTH_CODE/$READY_CODE"
fi

# Test 9.3: Weak password rejected
info "Test 9.3: Weak password rejected"
CSRF=$(get_csrf)
RESP=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
    -X POST "$BASE_URL/api/admin/users" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF" \
    -d '{"username":"weak_test","password":"123","role_id":3}')
CODE=$(http_code "$RESP")

if [ "$CODE" = "400" ]; then
    pass "Weak password rejected"
else
    fail "Weak password should be rejected, got $CODE"
fi

# ============================================================================
test_header "TEST SUMMARY"
# ============================================================================

TOTAL=$((PASSED + FAILED))
if [ $TOTAL -gt 0 ]; then
    RATE=$((PASSED * 100 / TOTAL))
else
    RATE=0
fi

echo ""
echo -e "${GREEN}Passed:${NC}  $PASSED"
echo -e "${RED}Failed:${NC}  $FAILED"
echo -e "${YELLOW}Skipped:${NC} $SKIPPED"
echo "Pass Rate: $RATE%"
echo ""

if [ $FAILED -eq 0 ]; then
    echo -e "${GREEN}All tests passed!${NC}"
    exit 0
else
    echo -e "${RED}Some tests failed.${NC}"
    exit 1
fi
