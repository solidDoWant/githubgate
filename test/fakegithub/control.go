package fakegithub

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// ControlHandler serves the test control API, meant for a separate port.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apps", s.controlCreateApp)
	mux.HandleFunc("POST /installations", s.controlCreateInstallation)
	mux.HandleFunc("POST /installations/{id}/suspend", s.controlSuspend)
	mux.HandleFunc("POST /repositories", s.controlCreateRepository)
	mux.HandleFunc("POST /clock/advance", s.controlAdvanceClock)
	mux.HandleFunc("POST /token-lifetime", s.controlSetTokenLifetime)
	mux.HandleFunc("GET /mint-log", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.MintLog()) })
	mux.HandleFunc("GET /tokens", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Tokens()) })
	mux.HandleFunc("GET /jwts", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.JWTs()) })

	return mux
}

func decode(writer http.ResponseWriter, request *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(into); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "bad request body")

		return false
	}

	return true
}

func parsePublicKey(pemText string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("no PEM block")
	}

	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
	}

	return x509.ParsePKCS1PublicKey(block.Bytes)
}

func (s *Server) controlCreateApp(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		PublicKeyPEM string `json:"public_key_pem"`
	}
	if !decode(writer, request, &body) {
		return
	}

	key, err := parsePublicKey(body.PublicKeyPEM)
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, "public_key_pem must be an RSA public key in PEM")

		return
	}

	s.CreateApp(App{ID: body.ID, Name: body.Name, PublicKey: key})
	writer.WriteHeader(http.StatusCreated)
}

func (s *Server) controlCreateInstallation(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ID           int64             `json:"id"`
		AppID        int64             `json:"app_id"`
		Owner        string            `json:"owner"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []string          `json:"repositories"`
	}
	if !decode(writer, request, &body) {
		return
	}

	err := s.CreateInstallation(Installation{ID: body.ID, AppID: body.AppID, Owner: body.Owner, Permissions: body.Permissions, Repositories: body.Repositories})
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, err.Error())

		return
	}

	writer.WriteHeader(http.StatusCreated)
}

func (s *Server) controlSuspend(writer http.ResponseWriter, request *http.Request) {
	id, _ := strconv.ParseInt(request.PathValue("id"), 10, 64)

	var body struct {
		Suspended bool `json:"suspended"`
	}
	if !decode(writer, request, &body) {
		return
	}

	if err := s.SuspendInstallation(id, body.Suspended); err != nil {
		writeJSONError(writer, http.StatusNotFound, err.Error())

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) controlCreateRepository(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Owner  string `json:"owner"`
		Name   string `json:"name"`
		Public bool   `json:"public"`
	}
	if !decode(writer, request, &body) {
		return
	}

	if err := s.CreateRepository(body.Owner, body.Name, body.Public); err != nil {
		writeJSONError(writer, http.StatusBadRequest, err.Error())

		return
	}

	writer.WriteHeader(http.StatusCreated)
}

func (s *Server) controlAdvanceClock(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Seconds int64 `json:"seconds"`
	}
	if !decode(writer, request, &body) {
		return
	}

	s.AdvanceClock(time.Duration(body.Seconds) * time.Second)
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) controlSetTokenLifetime(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Seconds int64 `json:"seconds"`
	}
	if !decode(writer, request, &body) {
		return
	}

	s.SetTokenLifetime(time.Duration(body.Seconds) * time.Second)
	writer.WriteHeader(http.StatusNoContent)
}
