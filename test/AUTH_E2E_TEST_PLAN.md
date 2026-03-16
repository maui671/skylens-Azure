# Skylens Authentication E2E Test Plan

## Overview

This document describes comprehensive end-to-end tests for the Skylens authentication system. The tests validate login flows, protected routes, token management, CSRF protection, role-based access control, and session management.

## Test Environment

- **Base URL**: `http://localhost:8080`
- **WebSocket Port**: `8081`
- **Default Admin**: `admin` / `changeme123!`
- **Cookie Files**: `/tmp/test_cookies.txt`

## Test Categories

### 1. Login Flow Tests

| Test ID | Description | Expected Result | curl Command |
|---------|-------------|-----------------|--------------|
| 1.1 | Valid credentials | HTTP 200, user data in response | `curl -X POST http://localhost:8080/api/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"changeme123!"}'` |
| 1.2 | Invalid password | HTTP 401, "invalid username or password" | `curl -X POST http://localhost:8080/api/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"wrongpassword"}'` |
| 1.3 | Non-existent user | HTTP 401 (same message as invalid password) | `curl -X POST http://localhost:8080/api/auth/login -H "Content-Type: application/json" -d '{"username":"nonexistent","password":"anypass"}'` |
| 1.4 | Empty credentials | HTTP 400, "username and password are required" | `curl -X POST http://localhost:8080/api/auth/login -H "Content-Type: application/json" -d '{"username":"","password":""}'` |
| 1.5 | Malformed JSON | HTTP 400, "invalid request body" | `curl -X POST http://localhost:8080/api/auth/login -H "Content-Type: application/json" -d 'not valid json'` |

### 2. Protected Routes Tests

| Test ID | Description | Expected Result | curl Command |
|---------|-------------|-----------------|--------------|
| 2.1 | Access /api/auth/me without auth | HTTP 401 | `curl http://localhost:8080/api/auth/me` |
| 2.2 | Access /api/auth/me with valid cookie | HTTP 200, user data | `curl -b /tmp/test_cookies.txt http://localhost:8080/api/auth/me` |
| 2.3 | Access /api/drones without auth | HTTP 401 | `curl http://localhost:8080/api/drones` |
| 2.4 | Access /api/drones with valid cookie | HTTP 200 | `curl -b /tmp/test_cookies.txt http://localhost:8080/api/drones` |
| 2.5 | Bearer token authentication | HTTP 200 | `curl -H "Authorization: Bearer <token>" http://localhost:8080/api/auth/me` |
| 2.6 | Invalid Bearer token | HTTP 401 | `curl -H "Authorization: Bearer invalid.token" http://localhost:8080/api/auth/me` |

### 3. Token Refresh Tests

| Test ID | Description | Expected Result | curl Command |
|---------|-------------|-----------------|--------------|
| 3.1 | Refresh with valid refresh cookie | HTTP 200, "token refreshed" | `curl -X POST -b /tmp/test_cookies.txt -c /tmp/test_cookies.txt http://localhost:8080/api/auth/refresh` |
| 3.2 | Refresh without cookie | HTTP 401 | `curl -X POST http://localhost:8080/api/auth/refresh` |
| 3.3 | Verify refreshed token | HTTP 200 on protected route | `curl -b /tmp/test_cookies.txt http://localhost:8080/api/auth/me` |

### 4. Logout Tests

| Test ID | Description | Expected Result | curl Command |
|---------|-------------|-----------------|--------------|
| 4.1 | Logout with valid session | HTTP 200, "logged out successfully" | `curl -X POST -b /tmp/test_cookies.txt http://localhost:8080/api/auth/logout` |
| 4.2 | Access after logout | HTTP 401 | `curl -b /tmp/test_cookies.txt http://localhost:8080/api/auth/me` |
| 4.3 | Logout without session | HTTP 401 | `curl -X POST http://localhost:8080/api/auth/logout` |

### 5. WebSocket Authentication Tests

| Test ID | Description | Expected Result |
|---------|-------------|-----------------|
| 5.1 | WebSocket without auth | Connection rejected with 401 (if auth enabled) |
| 5.2 | WebSocket with token param | Connection accepted |
| 5.3 | WebSocket with cookie | Connection accepted |

**Example WebSocket Connection with Token**:
```bash
# Using websocat
websocat "ws://localhost:8081/ws?token=<access_token>"
```

### 6. CSRF Protection Tests

| Test ID | Description | Expected Result | curl Command |
|---------|-------------|-----------------|--------------|
| 6.1 | Get CSRF token | HTTP 200, csrf_token in response | `curl -b /tmp/test_cookies.txt http://localhost:8080/api/auth/csrf` |
| 6.2 | POST without CSRF | HTTP 403, "CSRF token missing" | `curl -X POST -b /tmp/test_cookies.txt http://localhost:8080/api/auth/change-password -H "Content-Type: application/json" -d '{...}'` |
| 6.3 | POST with valid CSRF | Request processed | `curl -X POST -b /tmp/test_cookies.txt -H "X-CSRF-Token: <token>" http://localhost:8080/api/auth/change-password -H "Content-Type: application/json" -d '{...}'` |
| 6.4 | POST with mismatched CSRF | HTTP 403, "CSRF token invalid" | `curl -X POST -b /tmp/test_cookies.txt -H "X-CSRF-Token: wrong_token" http://localhost:8080/api/auth/change-password -H "Content-Type: application/json" -d '{...}'` |
| 6.5 | GET requests | No CSRF required | `curl -b /tmp/test_cookies.txt http://localhost:8080/api/auth/sessions` |

**CSRF Protected Endpoints**:
- `POST /api/auth/change-password`
- `DELETE /api/auth/sessions/{id}`
- All admin user management endpoints

### 7. Password Change & Session Revocation Tests

| Test ID | Description | Expected Result |
|---------|-------------|-----------------|
| 7.1 | Create multiple sessions | Both sessions active |
| 7.2 | Change password | HTTP 200, success message |
| 7.3 | Current session still works | HTTP 200 on protected route |
| 7.4 | Other sessions revoked | HTTP 401 on protected route |

**Test Scenario**:
```bash
# Create two sessions for same user
curl -c /tmp/session1.txt -X POST http://localhost:8080/api/auth/login -d '{"username":"test","password":"pass"}'
curl -c /tmp/session2.txt -X POST http://localhost:8080/api/auth/login -d '{"username":"test","password":"pass"}'

# Change password from session1
curl -b /tmp/session1.txt -H "X-CSRF-Token: <csrf>" -X POST http://localhost:8080/api/auth/change-password -d '{"current_password":"pass","new_password":"NewPass123!","confirm_password":"NewPass123!"}'

# Session1 should still work
curl -b /tmp/session1.txt http://localhost:8080/api/auth/me  # 200

# Session2 should be revoked
curl -b /tmp/session2.txt http://localhost:8080/api/auth/me  # 401
```

### 8. Role-Based Access Control Tests

| Role | Permissions | Admin Routes | Data Routes |
|------|-------------|--------------|-------------|
| admin | All | Full access | Full access |
| operator | View, tag, classify, alerts | Denied (403) | Full access |
| viewer | Read-only | Denied (403) | Read-only |

| Test ID | Description | Expected Result |
|---------|-------------|-----------------|
| 8.1 | Admin accesses /api/admin/users | HTTP 200 |
| 8.2 | Operator accesses /api/admin/users | HTTP 403 |
| 8.3 | Viewer accesses /api/admin/users | HTTP 403 |
| 8.4 | All roles access /api/drones | HTTP 200 |

**Test Commands**:
```bash
# Admin access to admin routes
curl -b /tmp/admin_cookies.txt http://localhost:8080/api/admin/users  # 200

# Operator denied admin routes
curl -b /tmp/operator_cookies.txt http://localhost:8080/api/admin/users  # 403

# Viewer can read drones
curl -b /tmp/viewer_cookies.txt http://localhost:8080/api/drones  # 200
```

### 9. Account Lockout Tests

| Test ID | Description | Expected Result |
|---------|-------------|-----------------|
| 9.1 | 5 failed login attempts | Account locked |
| 9.2 | Login with correct password while locked | HTTP 403, "account is locked" |
| 9.3 | Login after lockout expires | HTTP 200 |

**Configuration**:
- `max_failed_attempts`: 5
- `lockout_duration`: 15 minutes

### 10. Additional Security Tests

| Test ID | Description | Expected Result |
|---------|-------------|-----------------|
| 10.1 | Password requirements endpoint | HTTP 200, requirements object |
| 10.2 | Weak password rejection | HTTP 400 |
| 10.3 | Rate limiting on login | HTTP 429 after threshold |
| 10.4 | Health endpoints without auth | HTTP 200 |
| 10.5 | Session listing | HTTP 200, sessions array |

**Password Requirements**:
```json
{
  "min_length": 12,
  "require_uppercase": true,
  "require_lowercase": true,
  "require_digit": true,
  "require_special": true
}
```

## Running Tests

### Automated Test Suite

```bash
# Run full E2E test suite
bash /home/node/skylens-node/test/auth_e2e_runner.sh
```

### Manual Testing

```bash
# 1. Login and save cookies
curl -c /tmp/test_cookies.txt -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"changeme123!"}'

# 2. Access protected route
curl -b /tmp/test_cookies.txt http://localhost:8080/api/auth/me

# 3. Get CSRF token
curl -b /tmp/test_cookies.txt -c /tmp/test_cookies.txt http://localhost:8080/api/auth/csrf

# 4. Extract CSRF from cookies
CSRF=$(grep skylens_csrf /tmp/test_cookies.txt | awk '{print $NF}')

# 5. Make state-changing request with CSRF
curl -b /tmp/test_cookies.txt -X POST http://localhost:8080/api/auth/change-password \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"current_password":"changeme123!","new_password":"NewSecure123!","confirm_password":"NewSecure123!"}'
```

## API Reference

### Authentication Endpoints

| Method | Endpoint | Auth Required | CSRF Required | Description |
|--------|----------|---------------|---------------|-------------|
| POST | /api/auth/login | No | No | Authenticate user |
| POST | /api/auth/logout | Yes | No | End session |
| POST | /api/auth/refresh | Cookie | No | Refresh access token |
| GET | /api/auth/me | Yes | No | Get current user |
| GET | /api/auth/csrf | Yes | No | Get CSRF token |
| POST | /api/auth/change-password | Yes | Yes | Change password |
| GET | /api/auth/sessions | Yes | No | List active sessions |
| DELETE | /api/auth/sessions/{id} | Yes | Yes | Revoke session |
| GET | /api/auth/roles | Yes | No | List roles |
| GET | /api/auth/password-requirements | No | No | Get password policy |

### Admin Endpoints

| Method | Endpoint | Auth Required | Role Required |
|--------|----------|---------------|---------------|
| GET | /api/admin/users | Yes | admin |
| POST | /api/admin/users | Yes | admin |
| GET | /api/admin/users/{id} | Yes | admin |
| PUT | /api/admin/users/{id} | Yes | admin |
| DELETE | /api/admin/users/{id} | Yes | admin |
| POST | /api/admin/users/{id}/reset-password | Yes | admin |

## Expected Test Results

When all tests pass:
```
=== TEST SUMMARY ===

Passed:  26+
Failed:  0
Skipped: 0-2 (rate limiting may cause skips)
Pass Rate: 100%

All tests passed!
```

## Troubleshooting

### Rate Limiting
If tests fail due to rate limiting (HTTP 429):
- Wait 60 seconds between test runs
- Rate limit: 10 requests per minute per IP

### Session Expired
If tests fail due to expired session:
- Re-run login test to get fresh cookies
- Access token expires in 15 minutes
- Refresh token expires in 7 days

### CSRF Token Mismatch
If CSRF tests fail:
- Ensure CSRF cookie and header match
- Get fresh CSRF token before each POST

## Security Assertions

1. **Authentication**: All API routes (except health and public auth) return 401 without valid credentials
2. **Authorization**: Role-based access enforced; operators/viewers cannot access admin routes
3. **Session Security**: Sessions are properly invalidated on logout and password change
4. **CSRF**: State-changing requests require valid CSRF token
5. **Password Policy**: Weak passwords are rejected
6. **Account Lockout**: Accounts lock after 5 failed attempts
7. **Rate Limiting**: Login endpoint is rate-limited
