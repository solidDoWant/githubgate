package fakegithub

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// clockSkew is GitHub's tolerance on JWT times.
const clockSkew = 60 * time.Second

// maxJWTLifetime is the longest a JWT may be valid.
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app
const maxJWTLifetime = 10 * time.Minute

func writeJSONError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{
		"message":           message,
		"documentation_url": "https://docs.github.com/rest",
	})
}

func bearer(request *http.Request) (string, bool) {
	header := request.Header.Get("Authorization")
	for _, prefix := range []string{"Bearer ", "bearer ", "token "} {
		if value, ok := strings.CutPrefix(header, prefix); ok && value != "" {
			return value, true
		}
	}

	return "", false
}

// authenticateApp validates the App JWT of a request and returns its App. On
// failure it has already answered 401, like GitHub:
// https://docs.github.com/en/rest/authentication/authenticating-to-the-rest-api
func (s *Server) authenticateApp(writer http.ResponseWriter, request *http.Request) (*App, bool) {
	raw, ok := bearer(request)
	if !ok {
		writeJSONError(writer, http.StatusUnauthorized, "A JSON web token could not be decoded")

		return nil, false
	}

	s.mu.Lock()
	s.jwts = append(s.jwts, raw)
	now := s.nowLocked()
	s.mu.Unlock()

	var app *App

	// RS256 only; time claims are checked below against the fake's clock.
	_, err := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithoutClaimsValidation(),
	).Parse(raw, func(token *jwt.Token) (any, error) {
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			return nil, errors.New("bad claims")
		}

		issuer, err := claims.GetIssuer()
		if err != nil {
			return nil, err
		}

		if issuer == "" {
			// GitHub also accepts a numeric iss.
			if number, ok := claims["iss"].(float64); ok {
				issuer = strconv.FormatInt(int64(number), 10)
			}
		}

		id, err := strconv.ParseInt(issuer, 10, 64)
		if err != nil {
			return nil, errors.New("bad iss")
		}

		s.mu.Lock()
		app = s.apps[id]
		s.mu.Unlock()

		if app == nil {
			return nil, fmt.Errorf("unknown app %d", id)
		}

		return app.PublicKey, nil
	})
	if err != nil || app == nil {
		writeJSONError(writer, http.StatusUnauthorized, "A JSON web token could not be decoded")

		return nil, false
	}

	claims := jwtTimes(raw)
	if claims == nil || claims.iat.After(now.Add(clockSkew)) {
		writeJSONError(writer, http.StatusUnauthorized, "'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued")

		return nil, false
	}

	if !claims.exp.After(now.Add(-clockSkew)) || claims.exp.After(now.Add(maxJWTLifetime+clockSkew)) {
		writeJSONError(writer, http.StatusUnauthorized, "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires")

		return nil, false
	}

	return app, true
}

type jwtTimeClaims struct{ iat, exp time.Time }

// jwtTimes reads iat and exp from an already verified token.
func jwtTimes(raw string) *jwtTimeClaims {
	claims := jwt.MapClaims{}

	if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil {
		return nil
	}

	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return nil
	}

	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return nil
	}

	return &jwtTimeClaims{iat: iat.Time, exp: exp.Time}
}
