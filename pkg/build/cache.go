// Copyright 2021 ko Build Authors All Rights Reserved.
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

package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/logs"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
)

// layerCache caches the metadata (diffid and descriptor) of layers built from
// Go binaries in $KOCACHE, so that a rebuilt but unchanged binary doesn't need
// to be tarred and compressed again unless the registry is missing the layer.
//
// Entries live at $KOCACHE/layers/<key>/<buildid>.json, where key hashes the
// inputs to the layer other than the binary, and buildid hashes the binary's
// build ID. An entry's contents are determined by its path, so concurrent
// writers (goroutines or ko processes sharing $KOCACHE) write identical bytes
// and need no locking, and an entry that can't be read is just a cache miss.
// Only the maxEntriesPerKey most recently used entries are kept per key.
type layerCache struct{}

const maxEntriesPerKey = 4

// layerBuilder builds a layer from the binary at file.
type layerBuilder func(file string) (v1.Layer, error)

// cacheEntry is the on-disk form of a cached layer's metadata.
type cacheEntry struct {
	DiffID     v1.Hash       `json:"diffid"`
	Descriptor v1.Descriptor `json:"descriptor"`
}

// get returns the layer that build builds from the binary at file, using
// cached metadata to defer building it when possible. inputs must hold
// everything other than the binary that affects the layer build builds; it is
// encoded with encoding/json to form the cache key.
//
// On a cache hit, build is called lazily, only if the layer's contents are
// needed, so file must still exist when the returned layer is used.
func (c *layerCache) get(ctx context.Context, file string, inputs any, build layerBuilder) (v1.Layer, error) {
	kc := os.Getenv("KOCACHE")
	if kc == "" {
		return build(file)
	}
	buildid, err := getBuildID(ctx, file)
	if err != nil || buildid == "" {
		logs.Debug.Printf("no buildid for %q (%v), not caching", file, err)
		return build(file)
	}
	dir, err := cacheKeyDir(kc, inputs, build)
	if err != nil {
		logs.Debug.Printf("no cache key for %q (%v), not caching", file, err)
		return build(file)
	}
	path := cacheEntryPath(dir, buildid)

	// Cache hit.
	var entry cacheEntry
	/* #nosec G304 -- path is derived from the user-controlled KOCACHE. */
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &entry) == nil {
		// Mark the entry as recently used, for prune.
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return &lazyLayer{
			diffid:     entry.DiffID,
			desc:       entry.Descriptor,
			buildLayer: func() (v1.Layer, error) { return build(file) },
		}, nil
	}

	// Cache miss.
	layer, err := build(file)
	if err != nil {
		return nil, fmt.Errorf("build(%q): %w", file, err)
	}
	if err := writeCacheEntry(path, layer); err != nil {
		log.Printf("failed to cache metadata %s: %v", file, err)
	}
	prune(dir)
	return layer, nil
}

// cacheKeyDir returns the directory for the entries of layers that build
// builds with inputs. Its name hashes inputs and the digest of the layer that
// build builds from a fixed probe file, so that entries aren't used once
// build builds different layers for the same inputs, e.g. in a new ko version.
func cacheKeyDir(kocache string, inputs any, build layerBuilder) (string, error) {
	tmp, err := os.MkdirTemp("", "ko-layer-probe")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	probe := filepath.Join(tmp, "probe")
	if err := os.WriteFile(probe, []byte("ko layer cache probe\n"), 0o600); err != nil {
		return "", err
	}
	layer, err := build(probe)
	if err != nil {
		return "", fmt.Errorf("building probe layer: %w", err)
	}
	digest, err := layer.Digest()
	if err != nil {
		return "", fmt.Errorf("building probe layer: %w", err)
	}

	b, err := json.Marshal(struct {
		Inputs any
		Probe  v1.Hash
	}{inputs, digest})
	if err != nil {
		return "", fmt.Errorf("encoding layer cache key: %w", err)
	}
	return filepath.Join(kocache, "layers", hashString(b)), nil
}

// cacheEntryPath returns the path of the entry in dir for a binary with
// buildid, which may contain slashes.
func cacheEntryPath(dir, buildid string) string {
	return filepath.Join(dir, hashString([]byte(buildid))+".json")
}

func hashString(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func writeCacheEntry(path string, layer v1.Layer) error {
	desc, err := partial.Descriptor(layer)
	if err != nil {
		return err
	}
	diffid, err := layer.DiffID()
	if err != nil {
		return err
	}
	b, err := json.Marshal(cacheEntry{DiffID: diffid, Descriptor: *desc})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// prune removes all but the maxEntriesPerKey most recently used entries in
// dir. Concurrent prunes may race, but at worst remove an entry that would
// have been a cache hit.
func prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= maxEntriesPerKey {
		return
	}
	type file struct {
		path  string
		mtime time.Time
	}
	files := make([]file, 0, len(entries))
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	slices.SortFunc(files, func(a, b file) int { return b.mtime.Compare(a.mtime) })
	for _, f := range files[min(maxEntriesPerKey, len(files)):] {
		_ = os.Remove(f.path)
	}
}

func getBuildID(ctx context.Context, file string) (string, error) {
	gobin := getGoBinary()

	cmd := exec.CommandContext(ctx, gobin, "tool", "buildid", file)
	var output bytes.Buffer
	cmd.Stderr = &output
	cmd.Stdout = &output

	if err := cmd.Run(); err != nil {
		log.Printf("Unexpected error running \"go tool buildid %s\": %v\n%v", file, err, output.String())
		return "", fmt.Errorf("go tool buildid %s: %w", file, err)
	}
	return strings.TrimSpace(output.String()), nil
}
