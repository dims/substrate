// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memorypullcache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type MemoryPullCache struct {
	gcpAuthenticator authn.Authenticator

	localhostRegistryReplacement string

	// Map from hexadecimal sha256 hash of image to byte contents of composed
	// tarball
	cache *byteCache
}

func NewMemoryPullCache(ctx context.Context, gcpAuthenticator authn.Authenticator, localhostRegistryReplacement string, maxBytes int64) (*MemoryPullCache, error) {
	// TODO: share the cache across ateoms on a machine, e.g. an on-disk directory
	// keyed by sha256, keeping only the hottest images in memory.
	c := &MemoryPullCache{
		cache:                        newByteCache(maxBytes),
		localhostRegistryReplacement: localhostRegistryReplacement,
	}

	c.gcpAuthenticator = gcpAuthenticator

	slog.InfoContext(ctx, "Initialized image pull cache", slog.Int64("max_bytes", maxBytes))

	return c, nil
}

func (c *MemoryPullCache) Fetch(ctx context.Context, ref string) (io.ReadCloser, error) {
	// when running in kind we need to rewrite the registry endpoint similar to the
	// containerd mirror config used in https://kind.sigs.k8s.io/docs/user/local-registry/
	// for now we have simple opt-in support to rewrite local registries
	rewritten := false
	if c.localhostRegistryReplacement != "" {
		newRef := c.rewriteLocalRegistry(ref)
		if newRef != ref {
			ref = newRef
			rewritten = true
		}
	}
	var nameOpts []name.Option
	// match docker behavior, permit http image pulls for local registries
	// this avoids needing to distribute TLS certs all around for local development
	if rewritten || isLocalRegistry(ref) {
		nameOpts = append(nameOpts, name.Insecure)
	}

	parsedRef, err := name.ParseReference(ref, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("while parsing reference: %w", err)
	}

	// If the image ref included a digest, check for a hit in the pull cache.
	requestedDigest, digestWasIncluded := parsedRef.(name.Digest)
	if digestWasIncluded {
		slog.InfoContext(
			ctx,
			"Ref includes digest, checking for cache hit",
			slog.String("ref", ref),
			slog.String("digest", requestedDigest.DigestStr()),
		)

		if data, ok := c.cache.get(requestedDigest.DigestStr()); ok {
			slog.InfoContext(
				ctx,
				"Cache hit",
				slog.String("ref", ref),
				slog.String("digest", requestedDigest.DigestStr()),
			)
			return io.NopCloser(bytes.NewReader(data)), nil
		}
	}

	slog.InfoContext(
		ctx,
		"Cache miss",
		slog.String("ref", ref),
	)

	// If we didn't have a cache hit, we are on the slow path of pulling the
	// image from the registry.  This is a chatty process, with multiple round
	// trips to the registry.

	var remoteOptions []remote.Option
	remoteOptions = append(remoteOptions, remote.WithPlatform(v1.Platform{
		Architecture: runtime.GOARCH,
		OS:           "linux",
	}))

	registry := parsedRef.Context().Registry.RegistryStr()
	if registry == "gcr.io" || strings.HasSuffix(registry, ".gcr.io") || registry == "pkg.dev" || strings.HasSuffix(registry, ".pkg.dev") {
		if c.gcpAuthenticator != nil {
			remoteOptions = append(remoteOptions, remote.WithAuth(c.gcpAuthenticator))
		}
	}

	img, err := remote.Image(parsedRef, remoteOptions...)
	if err != nil {
		return nil, fmt.Errorf("in remote.Image: %w", err)
	}

	tarData := mutate.Extract(img)

	// Only digest-pinned refs are cached (the digest is the cache key); others stream.
	if !digestWasIncluded {
		return tarData, nil
	}

	// img.Size() is the manifest size, not the rootfs, so it can't gate caching.
	// Read up to the budget: smaller images are cached, larger ones streamed.
	cacheData, body, err := readBoundedForCache(tarData, c.cache.maxBytes)
	if err != nil {
		return nil, fmt.Errorf("while reading image: %w", err)
	}

	if cacheData == nil {
		slog.InfoContext(ctx,
			"Image is too large to cache, streaming",
			slog.String("ref", ref),
			slog.Int64("max_bytes", c.cache.maxBytes),
		)
		return body, nil
	}

	if c.cache.add(requestedDigest.DigestStr(), cacheData) {
		slog.InfoContext(
			ctx,
			"Populated image cache",
			slog.String("ref", ref),
			slog.String("digest", requestedDigest.DigestStr()),
			slog.Int("bytes", len(cacheData)),
		)
	}

	return body, nil
}

// readBoundedForCache reads up to limit+1 bytes from r. If the stream ends within
// limit, the whole content is returned as cacheData and r is closed; otherwise
// cacheData is nil and body streams the buffered prefix followed by the rest of
// r, closing r on Close. body always yields the full content once.
func readBoundedForCache(r io.ReadCloser, limit int64) (cacheData []byte, body io.ReadCloser, err error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}

	if int64(len(buf)) > limit {
		// Too large to cache; r is closed by the caller via body.Close.
		return nil, newReadCloser(io.MultiReader(bytes.NewReader(buf), r), r), nil
	}

	// The whole content is in buf; r is fully consumed and can be closed now.
	if cerr := r.Close(); cerr != nil {
		return nil, nil, cerr
	}
	return buf, io.NopCloser(bytes.NewReader(buf)), nil
}

// newReadCloser pairs a Reader with an independent Closer.
func newReadCloser(r io.Reader, c io.Closer) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{r, c}
}

func registryHost(ref string) string {
	parts := strings.SplitN(ref, "/", 2)
	reg, err := name.NewRegistry(parts[0], name.Insecure)
	if err != nil {
		return ""
	}
	hostPart := reg.Name()
	if h, _, err := net.SplitHostPort(hostPart); err == nil {
		return h
	}
	return hostPart
}

func isLocalhostOrLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func isLocalRegistry(ref string) bool {
	// by default docker permits localhost and 127.0.0.0/8
	// we also permit IPv6 loopback here
	return isLocalhostOrLoopback(registryHost(ref))
}

func (c *MemoryPullCache) rewriteLocalRegistry(ref string) string {
	if isLocalRegistry(ref) {
		parts := strings.SplitN(ref, "/", 2)
		return c.localhostRegistryReplacement + "/" + parts[1]
	}
	return ref
}
