// Copyright 2026 ko Build Authors All Rights Reserved.
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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func mustLayer(t *testing.T) v1.Layer {
	t.Helper()
	layer, err := random.Layer(64, "application/vnd.oci.image.layer.v1.tar+gzip")
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

func checkSameLayer(t *testing.T, got, want v1.Layer) {
	t.Helper()
	for _, f := range []func(v1.Layer) (v1.Hash, error){v1.Layer.Digest, v1.Layer.DiffID} {
		g, err := f(got)
		if err != nil {
			t.Fatal(err)
		}
		w, err := f(want)
		if err != nil {
			t.Fatal(err)
		}
		if g != w {
			t.Errorf("got %v, wanted %v", g, w)
		}
	}
}

// testBuilder returns a layerBuilder that always returns layer, and counts
// how often it builds the layer of file (rather than the cache's probe).
func testBuilder(file string, layer v1.Layer, calls *atomic.Int32) layerBuilder {
	return func(f string) (v1.Layer, error) {
		if f == file {
			calls.Add(1)
		}
		return layer, nil
	}
}

func writeEntry(t *testing.T, dir, buildid, content string, mtime time.Time) {
	t.Helper()
	path := cacheEntryPath(dir, buildid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestLayerCache(t *testing.T) {
	// The test binary has a Go build ID, so it can serve as the app binary.
	file, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	buildid, err := getBuildID(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)

	tests := []struct {
		name      string
		noKOCACHE bool
		// setup, if set, prepares the entries in the key directory of
		// the first get beforehand.
		setup       func(t *testing.T, dir string)
		inputs      [2]string
		wantMisses  int32
		wantHit     bool // whether the second get is a cache hit
		wantEntries int  // if set, the number of entries left for the second get's key
	}{
		{name: "hit", inputs: [2]string{"a", "a"}, wantMisses: 1, wantHit: true, wantEntries: 1},
		{name: "without KOCACHE", noKOCACHE: true, inputs: [2]string{"a", "a"}, wantMisses: 2},
		{name: "key includes inputs", inputs: [2]string{"/ko-app/a", "/ko-app/b"}, wantMisses: 2, wantEntries: 1},
		{
			name: "corrupt entry is replaced",
			setup: func(t *testing.T, dir string) {
				writeEntry(t, dir, buildid, "not json", old)
			},
			inputs:      [2]string{"a", "a"},
			wantMisses:  1,
			wantHit:     true,
			wantEntries: 1,
		},
		{
			name: "entries for other builds are kept",
			setup: func(t *testing.T, dir string) {
				writeEntry(t, dir, "other", "{}", old)
			},
			inputs:      [2]string{"a", "a"},
			wantMisses:  1,
			wantHit:     true,
			wantEntries: 2,
		},
		{
			name: "least recently used entries are pruned",
			setup: func(t *testing.T, dir string) {
				for i := range maxEntriesPerKey {
					writeEntry(t, dir, fmt.Sprint("other", i), "{}", old.Add(time.Duration(i)*time.Minute))
				}
			},
			inputs:      [2]string{"a", "a"},
			wantMisses:  1,
			wantHit:     true,
			wantEntries: maxEntriesPerKey,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := t.TempDir()
			if tt.noKOCACHE {
				kc = ""
			}
			t.Setenv("KOCACHE", kc)

			layers := map[string]v1.Layer{}
			for _, inputs := range tt.inputs {
				layers[inputs] = mustLayer(t)
			}
			var calls atomic.Int32
			keyDir := func(inputs string) string {
				dir, err := cacheKeyDir(kc, inputs, testBuilder(file, layers[inputs], &calls))
				if err != nil {
					t.Fatal(err)
				}
				return dir
			}
			if tt.setup != nil {
				tt.setup(t, keyDir(tt.inputs[0]))
			}

			var got v1.Layer
			for _, inputs := range tt.inputs {
				// Use a new cache each time: entries live on disk.
				build := testBuilder(file, layers[inputs], &calls)
				if got, err = (&layerCache{}).get(ctx, file, inputs, build); err != nil {
					t.Fatalf("get() = %v", err)
				}
				checkSameLayer(t, got, layers[inputs])
			}
			if n := calls.Load(); n != tt.wantMisses {
				t.Errorf("misses = %d, wanted %d", n, tt.wantMisses)
			}
			if _, hit := got.(*lazyLayer); hit != tt.wantHit {
				t.Fatalf("second get hit = %v, wanted %v", hit, tt.wantHit)
			}
			if tt.wantEntries != 0 {
				entries, err := os.ReadDir(keyDir(tt.inputs[1]))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != tt.wantEntries {
					t.Errorf("got %d entries, wanted %d", len(entries), tt.wantEntries)
				}
			}

			// A hit builds the layer only once its contents are needed.
			if tt.wantHit {
				rc, err := got.Compressed()
				if err != nil {
					t.Fatalf("Compressed() = %v", err)
				}
				rc.Close()
				if n := calls.Load(); n != tt.wantMisses+1 {
					t.Errorf("misses after Compressed() = %d, wanted %d", n, tt.wantMisses+1)
				}
			}
		})
	}
}

func TestLayerCacheConcurrent(t *testing.T) {
	kc := t.TempDir()
	t.Setenv("KOCACHE", kc)
	file, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Layers for several apps and platforms, fetched by concurrent cold
	// builds and then concurrent warm builds.
	layers := map[string]v1.Layer{}
	for i := range 12 {
		layers[fmt.Sprintf("/ko-app/app%d\nplatform-%d", i/4, i%4)] = mustLayer(t)
	}

	for _, round := range []string{"cold", "warm"} {
		t.Run(round, func(t *testing.T) {
			c := &layerCache{}
			var wg sync.WaitGroup
			for inputs, layer := range layers {
				for range 4 {
					wg.Go(func() {
						build := func(string) (v1.Layer, error) { return layer, nil }
						got, err := c.get(ctx, file, inputs, build)
						if err != nil {
							t.Errorf("get() = %v", err)
							return
						}
						checkSameLayer(t, got, layer)
						if _, hit := got.(*lazyLayer); round == "warm" && !hit {
							t.Errorf("get(%q) missed on a warm cache", inputs)
						}
					})
				}
			}
			wg.Wait()

			// Exactly one entry per key.
			dirs, err := os.ReadDir(filepath.Join(kc, "layers"))
			if err != nil {
				t.Fatal(err)
			}
			if len(dirs) != len(layers) {
				t.Errorf("got %d keys in the cache, wanted %d", len(dirs), len(layers))
			}
			for _, d := range dirs {
				entries, err := os.ReadDir(filepath.Join(kc, "layers", d.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Errorf("got %d entries for key %s, wanted 1", len(entries), d.Name())
				}
			}
		})
	}
}

func TestCacheKeyIncludesProbe(t *testing.T) {
	// The same inputs get a different key once the layers built for them
	// change, e.g. with a new version of ko.
	layerA, layerB := mustLayer(t), mustLayer(t)
	key := func(layer v1.Layer) string {
		dir, err := cacheKeyDir("/kocache", "inputs", func(string) (v1.Layer, error) { return layer, nil })
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if first := key(layerA); key(layerA) != first {
		t.Error("the same inputs and layers have different keys")
	}
	if key(layerA) == key(layerB) {
		t.Error("different layers for the same inputs have the same key")
	}
}

func TestBinaryLayerKey(t *testing.T) {
	// encoding/json skips unexported fields, which would leave them out of
	// the cache key.
	typ := reflect.TypeFor[binaryLayerKey]()
	for f := range typ.Fields() {
		if !f.IsExported() {
			t.Errorf("binaryLayerKey.%s is unexported, so it isn't part of the cache key", f.Name)
		}
	}

	// Changing any input changes the key.
	base := func() binaryLayerKey {
		return binaryLayerKey{
			ImportPath: "example.com/cmd/app",
			AppPath:    "/ko-app/app",
			Platform:   &v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"},
			MediaType:  types.OCILayer,
		}
	}
	variants := map[string]func(*binaryLayerKey){
		"base":         func(*binaryLayerKey) {},
		"import path":  func(k *binaryLayerKey) { k.ImportPath = "example.org/cmd/app" },
		"app path":     func(k *binaryLayerKey) { k.AppPath = "/ko-app/other" },
		"os":           func(k *binaryLayerKey) { k.Platform.OS = "windows" },
		"architecture": func(k *binaryLayerKey) { k.Platform.Architecture = "arm64" },
		"variant":      func(k *binaryLayerKey) { k.Platform.Variant = "v6" },
		"os version":   func(k *binaryLayerKey) { k.Platform.OSVersion = "10.0.17763" },
		"media type":   func(k *binaryLayerKey) { k.MediaType = types.DockerLayer },
	}
	layer := mustLayer(t)
	build := func(string) (v1.Layer, error) { return layer, nil }
	seen := map[string]string{}
	for name, mutate := range variants {
		k := base()
		mutate(&k)
		dir, err := cacheKeyDir("/kocache", k, build)
		if err != nil {
			t.Fatalf("cacheKeyDir(%s) = %v", name, err)
		}
		if other, ok := seen[dir]; ok {
			t.Errorf("%s and %s have the same cache key", name, other)
		}
		seen[dir] = name
	}
}
