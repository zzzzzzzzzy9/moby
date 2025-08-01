package dockerfile

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/sys/user"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestChownFlagParsing(t *testing.T) {
	testFiles := map[string]string{
		"passwd": `root:x:0:0::/bin:/bin/false
bin:x:1:1::/bin:/bin/false
wwwwww:x:21:33::/bin:/bin/false
unicorn:x:1001:1002::/bin:/bin/false
		`,
		"group": `root:x:0:
bin:x:1:
wwwwww:x:33:
unicorn:x:1002:
somegrp:x:5555:
othergrp:x:6666:
		`,
	}
	// test mappings for validating use of maps
	idMaps := []user.IDMap{
		{
			ID:       0,
			ParentID: 100000,
			Count:    65536,
		},
	}
	remapped := user.IdentityMapping{UIDMaps: idMaps, GIDMaps: idMaps}
	unmapped := user.IdentityMapping{}

	contextDir := t.TempDir()

	if err := os.Mkdir(filepath.Join(contextDir, "etc"), 0o755); err != nil {
		t.Fatalf("error creating test directory: %v", err)
	}

	for filename, content := range testFiles {
		createTestTempFile(t, filepath.Join(contextDir, "etc"), filename, content, 0o644)
	}

	// positive tests
	for _, testcase := range []struct {
		builder   *Builder
		name      string
		chownStr  string
		idMapping user.IdentityMapping
		state     *dispatchState
		expected  identity
	}{
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UIDNoMap",
			chownStr:  "1",
			idMapping: unmapped,
			state:     &dispatchState{},
			expected:  identity{UID: 1, GID: 1},
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UIDGIDNoMap",
			chownStr:  "0:1",
			idMapping: unmapped,
			state:     &dispatchState{},
			expected:  identity{UID: 0, GID: 1},
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UIDWithMap",
			chownStr:  "0",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  identity{UID: 100000, GID: 100000},
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UIDGIDWithMap",
			chownStr:  "1:33",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  identity{UID: 100001, GID: 100033},
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UserNoMap",
			chownStr:  "bin:5555",
			idMapping: unmapped,
			state:     &dispatchState{},
			expected:  identity{UID: 1, GID: 5555},
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "GroupWithMap",
			chownStr:  "0:unicorn",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  identity{UID: 100000, GID: 101002},
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UserOnlyWithMap",
			chownStr:  "unicorn",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  identity{UID: 101001, GID: 101002},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			idPair, err := parseChownFlag(context.TODO(), testcase.builder, testcase.state, testcase.chownStr, contextDir, testcase.idMapping)
			assert.NilError(t, err, "Failed to parse chown flag: %q", testcase.chownStr)
			assert.Check(t, is.DeepEqual(testcase.expected, idPair), "chown flag mapping failure")
		})
	}

	// error tests
	for _, testcase := range []struct {
		builder   *Builder
		name      string
		chownStr  string
		idMapping user.IdentityMapping
		state     *dispatchState
		descr     string
	}{
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "BadChownFlagFormat",
			chownStr:  "bob:1:555",
			idMapping: unmapped,
			state:     &dispatchState{},
			descr:     "invalid chown string format: bob:1:555",
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "UserNoExist",
			chownStr:  "bob",
			idMapping: unmapped,
			state:     &dispatchState{},
			descr:     "can't find uid for user bob: no such user: bob",
		},
		{
			builder:   &Builder{options: &build.ImageBuildOptions{Platform: "linux"}},
			name:      "GroupNoExist",
			chownStr:  "root:bob",
			idMapping: unmapped,
			state:     &dispatchState{},
			descr:     "can't find gid for group bob: no such group: bob",
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			_, err := parseChownFlag(context.TODO(), testcase.builder, testcase.state, testcase.chownStr, contextDir, testcase.idMapping)
			assert.Check(t, is.Error(err, testcase.descr), "Expected error string doesn't match")
		})
	}
}
