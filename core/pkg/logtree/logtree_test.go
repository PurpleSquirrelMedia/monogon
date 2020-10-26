// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
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

package logtree

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBacklog(t *testing.T) {
	tree := New()
	tree.MustLeveledFor("main").Info("hello, main!")
	tree.MustLeveledFor("main.foo").Info("hello, main.foo!")
	tree.MustLeveledFor("main.bar").Info("hello, main.bar!")
	tree.MustLeveledFor("aux").Info("hello, aux!")

	expect := func(dn DN, entries ...string) string {
		res := tree.Read(dn, WithChildren(), WithBacklog(BacklogAllAvailable))
		if want, got := len(entries), len(res.Backlog); want != got {
			t.Fatalf("wanted %d backlog entries, got %d", want, got)
		}
		got := make(map[string]bool)
		for _, entry := range res.Backlog {
			got[entry.Message()] = true
		}
		for _, entry := range entries {
			if !got[entry] {
				return fmt.Sprintf("missing entry %q", entry)
			}
		}
		return ""
	}

	if res := expect("main", "hello, main!", "hello, main.foo!", "hello, main.bar!"); res != "" {
		t.Errorf("retrieval at main failed: %s", res)
	}
	if res := expect("", "hello, main!", "hello, main.foo!", "hello, main.bar!", "hello, aux!"); res != "" {
		t.Errorf("retrieval at root failed: %s", res)
	}
	if res := expect("aux", "hello, aux!"); res != "" {
		t.Errorf("retrieval at aux failed: %s", res)
	}
}

func TestStream(t *testing.T) {
	tree := New()
	tree.MustLeveledFor("main").Info("hello, backlog")

	res := tree.Read("", WithBacklog(BacklogAllAvailable), WithChildren(), WithStream())
	defer res.Close()
	if want, got := 1, len(res.Backlog); want != got {
		t.Errorf("wanted %d backlog item, got %d", want, got)
	}

	tree.MustLeveledFor("main").Info("hello, stream")

	select {
	case <-time.After(time.Second * 1):
		t.Fatalf("timeout elapsed")
	case p := <-res.Stream:
		if want, got := "hello, stream", p.Message(); want != got {
			t.Fatalf("stream returned %q, wanted %q", got, want)
		}
	}
}

func TestVerbose(t *testing.T) {
	tree := New()

	tree.MustLeveledFor("main").V(10).Info("this shouldn't get logged")

	reader := tree.Read("", WithBacklog(BacklogAllAvailable), WithChildren())
	if want, got := 0, len(reader.Backlog); want != got {
		t.Fatalf("expected nothing to be logged, got %+v", reader.Backlog)
	}

	tree.SetVerbosity("main", 10)
	tree.MustLeveledFor("main").V(10).Info("this should get logged")

	reader = tree.Read("", WithBacklog(BacklogAllAvailable), WithChildren())
	if want, got := 1, len(reader.Backlog); want != got {
		t.Fatalf("expected %d entries to get logged, got %d", want, got)
	}
}

func TestMetadata(t *testing.T) {
	tree := New()
	tree.MustLeveledFor("main").Error("i am an error")
	tree.MustLeveledFor("main").Warning("i am a warning")
	tree.MustLeveledFor("main").Info("i am informative")
	tree.MustLeveledFor("main").V(0).Info("i am a zero-level debug")

	reader := tree.Read("", WithChildren(), WithBacklog(BacklogAllAvailable))
	if want, got := 4, len(reader.Backlog); want != got {
		t.Fatalf("expected %d entries, got %d", want, got)
	}

	for _, te := range []struct {
		ix       int
		severity Severity
		message  string
	}{
		{0, ERROR, "i am an error"},
		{1, WARNING, "i am a warning"},
		{2, INFO, "i am informative"},
		{3, INFO, "i am a zero-level debug"},
	} {
		p := reader.Backlog[te.ix]
		if want, got := te.severity, p.Severity(); want != got {
			t.Errorf("wanted element %d to have severity %s, got %s", te.ix, want, got)
		}
		if want, got := te.message, p.Message(); want != got {
			t.Errorf("wanted element %d to have message %q, got %q", te.ix, want, got)
		}
		if want, got := "logtree_test.go", strings.Split(p.Location(), ":")[0]; want != got {
			t.Errorf("wanted element %d to have file %q, got %q", te.ix, want, got)
		}
	}
}

func TestSeverity(t *testing.T) {
	tree := New()
	tree.MustLeveledFor("main").Error("i am an error")
	tree.MustLeveledFor("main").Warning("i am a warning")
	tree.MustLeveledFor("main").Info("i am informative")
	tree.MustLeveledFor("main").V(0).Info("i am a zero-level debug")

	reader := tree.Read("main", WithBacklog(BacklogAllAvailable), WithMinimumSeverity(WARNING))
	if want, got := 2, len(reader.Backlog); want != got {
		t.Fatalf("wanted %d entries, got %d", want, got)
	}
	if want, got := "i am an error", reader.Backlog[0].Message(); want != got {
		t.Fatalf("wanted entry %q, got %q", want, got)
	}
	if want, got := "i am a warning", reader.Backlog[1].Message(); want != got {
		t.Fatalf("wanted entry %q, got %q", want, got)
	}
}