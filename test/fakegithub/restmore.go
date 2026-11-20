package fakegithub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
)

// Release is a release created through the API.
type Release struct {
	ID      int64
	TagName string
	Assets  map[string][]byte
}

func (s *Server) releaseKey(owner, name string, id int64) string {
	return owner + "/" + name + "#" + strconv.FormatInt(id, 10)
}

// handleCreateRelease creates a release; GitHub answers 422 when the tag has a
// release already ("already_exists"):
// https://docs.github.com/en/rest/releases/releases#create-a-release
func (s *Server) handleCreateRelease(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	var body struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
	}
	if !readBody(writer, request, &body) {
		return
	}

	if body.TagName == "" {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")

	s.mu.Lock()
	defer s.mu.Unlock()

	for key, existing := range s.releases {
		if strings.HasPrefix(key, owner+"/"+name+"#") && existing.TagName == body.TagName {
			writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed: tag_name already_exists")

			return
		}
	}

	s.nextID++
	release := &Release{ID: s.nextID, TagName: body.TagName, Assets: map[string][]byte{}}
	s.releases[s.releaseKey(owner, name, release.ID)] = release

	uploadURL := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets{?name,label}", hostURL(request), owner, name, release.ID)
	writeJSON(writer, http.StatusCreated, map[string]any{"id": release.ID, "tag_name": body.TagName, "name": body.Name, "upload_url": uploadURL})
}

// handleUploadReleaseAsset stores an asset. It is served on the uploads host
// in production; the fake accepts it on any host, so one listener serves all
// three names. The name query parameter is required:
// https://docs.github.com/en/rest/releases/assets#upload-a-release-asset
func (s *Server) handleUploadReleaseAsset(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	assetName := request.URL.Query().Get("name")

	s.mu.Lock()
	release := s.releases[s.releaseKey(request.PathValue("owner"), request.PathValue("repo"), id)]
	s.mu.Unlock()

	if err != nil || release == nil {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	if assetName == "" {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed: name is missing")

		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 64<<20))
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, "Problems reading body")

		return
	}

	s.mu.Lock()
	if _, exists := release.Assets[assetName]; exists {
		s.mu.Unlock()
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed: name already_exists")

		return
	}

	release.Assets[assetName] = data
	s.nextID++
	assetID := s.nextID
	s.mu.Unlock()

	writeJSON(writer, http.StatusCreated, map[string]any{"id": assetID, "name": assetName, "size": len(data), "state": "uploaded"})
}

// handleArchive redirects to the archive's storage URL, as GitHub redirects
// tarball and zipball requests to codeload.github.com with a 302:
// https://docs.github.com/en/rest/repos/contents#download-a-repository-archive-tar
func (s *Server) handleArchive(format string) repoHandler {
	return func(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
		owner, name, ref := request.PathValue("owner"), request.PathValue("repo"), request.PathValue("ref")
		if ref == "" {
			ref = "main"
		}

		if _, err := runGit(s.repoDir(owner, name), nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
			writeJSONError(writer, http.StatusNotFound, "Not Found")

			return
		}

		// The storage URL carries its own signature, never the caller's token.
		raw := make([]byte, 8)
		_, _ = rand.Read(raw)

		base := hostURL(request)
		if request.TLS != nil && request.Host == "api.github.com" {
			base = "https://" + StorageHost
		}

		target := fmt.Sprintf("%s/_storage/%s/%s/%s?format=%s&sig=%s", base, owner, name, ref, format, hex.EncodeToString(raw))
		http.Redirect(writer, request, target, http.StatusFound)
	}
}

// handleStorage serves the archive behind the redirect without any
// credential, like GitHub's signed storage URLs.
func (s *Server) handleStorage(writer http.ResponseWriter, request *http.Request) {
	owner, name, ref := request.PathValue("owner"), request.PathValue("repo"), request.PathValue("ref")
	format := request.URL.Query().Get("format")

	if !validName(owner) || !validName(name) || (format != "tar" && format != "zip") {
		http.NotFound(writer, request)

		return
	}

	cmd := exec.CommandContext(request.Context(), "git", "archive", "--format="+format, ref)
	cmd.Dir = s.repoDir(owner, name)

	out, err := cmd.Output()
	if err != nil {
		http.NotFound(writer, request)

		return
	}

	writer.Header().Set("Content-Type", "application/"+format)
	_, _ = writer.Write(out)
}

func hostURL(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}

	return scheme + "://" + request.Host
}
