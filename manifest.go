// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type manifest struct {
	contentType string
	blob        []byte
}

type manifests struct {
	// maps repo -> manifest tag/digest -> manifest
	manifests map[string]map[string]manifest
	lock      sync.Mutex
}

func isManifest(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (m *manifests) handle(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	target := elem[len(elem)-1]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	if req.Method == "GET" {
		m.lock.Lock()
		defer m.lock.Unlock()
		c, ok := m.manifests[repo]
		if !ok {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "NAME_UNKNOWN",
				Message: "Unknown name",
			}
		}
		m, ok := c[target]
		if !ok {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "MANIFEST_UNKNOWN",
				Message: "Unknown manifest",
			}
		}
		rd := sha256.Sum256(m.blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", m.contentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(m.blob)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader(m.blob))
		return nil
	}

	if req.Method == "HEAD" {
		m.lock.Lock()
		defer m.lock.Unlock()
		if _, ok := m.manifests[repo]; !ok {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "NAME_UNKNOWN",
				Message: "Unknown name",
			}
		}
		m, ok := m.manifests[repo][target]
		if !ok {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "MANIFEST_UNKNOWN",
				Message: "Unknown manifest",
			}
		}
		rd := sha256.Sum256(m.blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", m.contentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(m.blob)))
		resp.WriteHeader(http.StatusOK)
		return nil
	}

	if req.Method == "PUT" {
		m.lock.Lock()
		defer m.lock.Unlock()
		if _, ok := m.manifests[repo]; !ok {
			m.manifests[repo] = map[string]manifest{}
		}
		b := &bytes.Buffer{}
		io.Copy(b, req.Body)
		rd := sha256.Sum256(b.Bytes())
		digest := "sha256:" + hex.EncodeToString(rd[:])
		mf := manifest{
			blob:        b.Bytes(),
			contentType: req.Header.Get("Content-Type"),
		}

		// If the manifest is a manifest list, check that the manifest
		// list's constituent manifests are already uploaded.
		// This isn't strictly required by the registry API, but some
		// registries require this.
		if types.MediaType(mf.contentType).IsIndex() {
			im, err := v1.ParseIndexManifest(b)
			if err != nil {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "MANIFEST_INVALID",
					Message: err.Error(),
				}
			}
			for _, desc := range im.Manifests {
				if !desc.MediaType.IsDistributable() {
					continue
				}
				if desc.MediaType.IsIndex() || desc.MediaType.IsImage() {
					if _, found := m.manifests[repo][desc.Digest.String()]; !found {
						return &regError{
							Status:  http.StatusNotFound,
							Code:    "MANIFEST_UNKNOWN",
							Message: fmt.Sprintf("Sub-manifest %q not found", desc.Digest),
						}
					}
				} else {
					// TODO: Probably want to do an existence check for blobs.
				}
			}
		}

		// Allow future references by target (tag) and immutable digest.
		// See https://docs.docker.com/engine/reference/commandline/pull/#pull-an-image-by-digest-immutable-identifier.
		m.manifests[repo][target] = mf
		m.manifests[repo][digest] = mf
		resp.Header().Set("Docker-Content-Digest", digest)
		resp.WriteHeader(http.StatusCreated)
		return nil
	}

	if req.Method == "DELETE" {
		m.lock.Lock()
		defer m.lock.Unlock()
		if _, ok := m.manifests[repo]; !ok {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "NAME_UNKNOWN",
				Message: "Unknown name",
			}
		}

		_, ok := m.manifests[repo][target]
		if !ok {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "MANIFEST_UNKNOWN",
				Message: "Unknown manifest",
			}
		}

		delete(m.manifests[repo], target)
		resp.WriteHeader(http.StatusAccepted)
		return nil
	}

	return &regError{
		Status:  http.StatusBadRequest,
		Code:    "METHOD_UNKNOWN",
		Message: "We don't understand your method + url",
	}
}
