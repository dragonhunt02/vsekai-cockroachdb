// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessio/shellescape"
	"github.com/cockroachdb/cockroach/pkg/release"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/stretchr/testify/require"
)

type mockStorage struct {
	bucket string
	gets   []string
	puts   []string
}

var _ release.ObjectPutGetter = (*mockStorage)(nil)

func (s *mockStorage) Bucket() string {
	return s.bucket
}

func (s mockStorage) URL(key string) string {
	return "storage://bucket/" + key
}

func (s *mockStorage) GetObject(i *release.GetObjectInput) (*release.GetObjectOutput, error) {
	url := fmt.Sprintf(`gs://%s/%s`, s.Bucket(), *i.Key)
	s.gets = append(s.gets, url)
	o := &release.GetObjectOutput{
		Body: io.NopCloser(bytes.NewBufferString(url)),
	}
	return o, nil
}

func (s *mockStorage) PutObject(i *release.PutObjectInput) error {
	url := fmt.Sprintf(`gs://%s/%s`, s.Bucket(), *i.Key)
	if i.CacheControl != nil {
		url += `/` + *i.CacheControl
	}
	if i.Body != nil {
		binary, err := io.ReadAll(i.Body)
		if err != nil {
			return err
		}
		if strings.HasSuffix(*i.Key, release.ChecksumSuffix) {
			// Unfortunately the archive tarball checksum changes every time,
			// because we generate tarballs and the copy file modification time from the generated files.
			// This makes the checksum not reproducible.
			s.puts = append(s.puts, fmt.Sprintf("%s CONTENTS <sha256sum>", url))
		} else if utf8.Valid(binary) {
			s.puts = append(s.puts, fmt.Sprintf("%s CONTENTS %s", url, binary))
		} else {
			s.puts = append(s.puts, fmt.Sprintf("%s CONTENTS <binary stuff>", url))
		}
	} else if i.WebsiteRedirectLocation != nil {
		s.puts = append(s.puts, fmt.Sprintf("%s REDIRECT %s", url, *i.WebsiteRedirectLocation))
	}
	return nil
}

type mockExecRunner struct {
	fakeBazelBin string
	cmds         []string
}

func (r *mockExecRunner) run(c *exec.Cmd) ([]byte, error) {
	if r.fakeBazelBin == "" {
		panic("r.fakeBazelBin not set")
	}
	if c.Dir == "" {
		return nil, fmt.Errorf("`Dir` must be specified")
	}
	cmd := fmt.Sprintf("env=%s args=%s", c.Env, shellescape.QuoteCommand(c.Args))
	r.cmds = append(r.cmds, cmd)

	var paths []string
	if c.Args[0] == "bazel" && c.Args[1] == "info" && c.Args[2] == "bazel-bin" {
		return []byte(r.fakeBazelBin), nil
	}
	if c.Args[0] == "bazel" && c.Args[1] == "build" {
		path := filepath.Join(r.fakeBazelBin, "pkg", "cmd", "cockroach", "cockroach_", "cockroach")
		pathSQL := filepath.Join(r.fakeBazelBin, "pkg", "cmd", "cockroach-sql", "cockroach-sql_", "cockroach-sql")
		var platform release.Platform
		for _, arg := range c.Args {
			if strings.HasPrefix(arg, `--config=`) {
				switch strings.TrimPrefix(arg, `--config=`) {
				case "crosslinuxbase":
					platform = release.PlatformLinux
				case "crossmacosbase":
					platform = release.PlatformMacOS
				case "crosswindowsbase":
					platform = release.PlatformWindows
					path += ".exe"
					pathSQL += ".exe"
				case "ci", "with_ui":
				default:
					panic(fmt.Sprintf("Unexpected configuration %s", arg))
				}
			}
		}
		paths = append(paths, path, pathSQL)
		ext := release.SharedLibraryExtensionFromPlatform(platform)
		for _, lib := range release.CRDBSharedLibraries {
			libDir := "lib"
			if platform == release.PlatformWindows {
				libDir = "bin"
			}
			paths = append(paths, filepath.Join(r.fakeBazelBin, "c-deps", "libgeos", libDir, lib+ext))
		}
	}

	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(cmd), 0666); err != nil {
			return nil, err
		}
	}

	var output []byte
	return output, nil
}

func TestProvisional(t *testing.T) {
	tests := []struct {
		name         string
		flags        runFlags
		expectedCmds []string
		expectedGets []string
		expectedPuts []string
	}{
		{
			name: `release`,
			flags: runFlags{
				doProvisional: true,
				isRelease:     true,
				branch:        `provisional_201901010101_v0.0.1-alpha`,
			},
			expectedCmds: []string{
				"env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-pc-linux-gnu official-binary v0.0.1-alpha release' -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"env=[] args=bazel info bazel-bin -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"env=[MALLOC_CONF=prof:true] args=./cockroach.linux-2.6.32-gnu-amd64 version",
				"env=[] args=ldd ./cockroach.linux-2.6.32-gnu-amd64",
				"env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-apple-darwin19 official-binary v0.0.1-alpha release' -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"env=[] args=bazel info bazel-bin -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=." +
					"/build/bazelutil/stamp.sh x86_64-w64-mingw32 official-binary v0.0.1-alpha release' -c opt --config=ci --config=with_ui --config=crosswindowsbase",
				"env=[] args=bazel info bazel-bin -c opt --config=ci --config=with_ui --config=crosswindowsbase",
			},
			expectedGets: nil,
			expectedPuts: []string{
				"gs://release-binaries-bucket/cockroach-v0.0.1-alpha.linux-amd64.tgz CONTENTS <binary stuff>",
				"gs://release-binaries-bucket/cockroach-v0.0.1-alpha.linux-amd64.tgz.sha256sum CONTENTS <sha256sum>",
				"gs://release-binaries-bucket/cockroach-sql-v0.0.1-alpha.linux-amd64.tgz CONTENTS <binary stuff>",
				"gs://release-binaries-bucket/cockroach-sql-v0.0.1-alpha.linux-amd64.tgz.sha256sum CONTENTS <sha256sum>",
				"gs://release-binaries-bucket/cockroach-v0.0.1-alpha.darwin-10.9-amd64.tgz CONTENTS <binary stuff>",
				"gs://release-binaries-bucket/cockroach-v0.0.1-alpha.darwin-10.9-amd64.tgz.sha256sum CONTENTS <sha256sum>",
				"gs://release-binaries-bucket/cockroach-sql-v0.0.1-alpha.darwin-10.9-amd64.tgz CONTENTS <binary stuff>",
				"gs://release-binaries-bucket/cockroach-sql-v0.0.1-alpha.darwin-10.9-amd64.tgz.sha256sum CONTENTS <sha256sum>",
				"gs://release-binaries-bucket/cockroach-v0.0.1-alpha.windows-6.2-amd64.zip CONTENTS <binary stuff>",
				"gs://release-binaries-bucket/cockroach-v0.0.1-alpha.windows-6.2-amd64.zip.sha256sum CONTENTS <sha256sum>",
				"gs://release-binaries-bucket/cockroach-sql-v0.0.1-alpha.windows-6.2-amd64.zip CONTENTS <binary stuff>",
				"gs://release-binaries-bucket/cockroach-sql-v0.0.1-alpha.windows-6.2-amd64.zip.sha256sum CONTENTS <sha256sum>",
			},
		},
		{
			name: `edge`,
			flags: runFlags{
				doProvisional: true,
				isRelease:     false,
				branch:        `master`,
				sha:           `00SHA00`,
			},
			expectedCmds: []string{
				"env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=." +
					"/build/bazelutil/stamp.sh x86_64-pc-linux-gnu official-binary' -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"env=[] args=bazel info bazel-bin -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"env=[MALLOC_CONF=prof:true] args=./cockroach.linux-2.6.32-gnu-amd64 version",
				"env=[] args=ldd ./cockroach.linux-2.6.32-gnu-amd64",
				"env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-apple-darwin19 official-binary' -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"env=[] args=bazel info bazel-bin -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-w64-mingw32 official-binary' -c opt --config=ci --config=with_ui --config=crosswindowsbase",
				"env=[] args=bazel info bazel-bin -c opt --config=ci --config=with_ui --config=crosswindowsbase",
			},
			expectedGets: nil,
			expectedPuts: []string{
				"gs://edge-binaries-bucket/cockroach/cockroach.linux-gnu-amd64.00SHA00 " +
					"CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp." +
					"sh x86_64-pc-linux-gnu official-binary' -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"gs://edge-binaries-bucket/cockroach/cockroach.linux-gnu-amd64.LATEST/no-cache " +
					"REDIRECT /cockroach/cockroach.linux-gnu-amd64.00SHA00",
				"gs://edge-binaries-bucket/cockroach/cockroach-sql.linux-gnu-amd64.00SHA00 CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql '--workspace_status_command=./build/bazelutil/stamp.sh x86_64-pc-linux-gnu official-binary' -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"gs://edge-binaries-bucket/cockroach/cockroach-sql.linux-gnu-amd64.LATEST/no-cache REDIRECT /cockroach/cockroach-sql.linux-gnu-amd64.00SHA00",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos.linux-gnu-amd64.00SHA00." +
					"so CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-pc-linux-gnu official-binary' -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos.linux-gnu-amd64.so.LATEST/no-cache REDIRECT /cockroach/lib/libgeos.linux-gnu-amd64.00SHA00.so",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos_c.linux-gnu-amd64.00SHA00." +
					"so CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-pc-linux-gnu official-binary' -c opt --config=ci --config=with_ui --config=crosslinuxbase",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos_c.linux-gnu-amd64.so.LATEST/no-cache REDIRECT /cockroach/lib/libgeos_c.linux-gnu-amd64.00SHA00.so",
				"gs://edge-binaries-bucket/cockroach/cockroach.darwin-amd64.00SHA00 " +
					"CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-apple-darwin19 official-binary' -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"gs://edge-binaries-bucket/cockroach/cockroach.darwin-amd64.LATEST/no-cache " +
					"REDIRECT /cockroach/cockroach.darwin-amd64.00SHA00",
				"gs://edge-binaries-bucket/cockroach/cockroach-sql.darwin-amd64.00SHA00 CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql '--workspace_status_command=./build/bazelutil/stamp.sh x86_64-apple-darwin19 official-binary' -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"gs://edge-binaries-bucket/cockroach/cockroach-sql.darwin-amd64.LATEST/no-cache REDIRECT /cockroach/cockroach-sql." +
					"darwin-amd64.00SHA00",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos.darwin-amd64.00SHA00." +
					"dylib CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-apple-darwin19 official-binary' -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos.darwin-amd64.dylib.LATEST/no-cache REDIRECT /cockroach/lib/libgeos.darwin-amd64.00SHA00.dylib",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos_c.darwin-amd64.00SHA00." +
					"dylib CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp." +
					"sh x86_64-apple-darwin19 official-binary' -c opt --config=ci --config=with_ui --config=crossmacosbase",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos_c.darwin-amd64.dylib.LATEST/no-cache REDIRECT /cockroach/lib/libgeos_c.darwin-amd64.00SHA00.dylib",
				"gs://edge-binaries-bucket/cockroach/cockroach.windows-amd64.00SHA00.exe " +
					"CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp." +
					"sh x86_64-w64-mingw32 official-binary' -c opt --config=ci --config=with_ui --config=crosswindowsbase",
				"gs://edge-binaries-bucket/cockroach/cockroach.windows-amd64.LATEST/no-cache " +
					"REDIRECT /cockroach/cockroach.windows-amd64.00SHA00.exe",
				"gs://edge-binaries-bucket/cockroach/cockroach-sql.windows-amd64.00SHA00.exe CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql '--workspace_status_command=./build/bazelutil/stamp.sh x86_64-w64-mingw32 official-binary' -c opt --config=ci --config=with_ui --config=crosswindowsbase",
				"gs://edge-binaries-bucket/cockroach/cockroach-sql.windows-amd64.LATEST/no-cache REDIRECT /cockroach/cockroach-sql.windows-amd64.00SHA00.exe",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos.windows-amd64.00SHA00." +
					"dll CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-w64-mingw32 official-binary' -c opt --config=ci --config=with_ui --config=crosswindowsbase",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos.windows-amd64.dll.LATEST/no-cache REDIRECT /cockroach/lib/libgeos.windows-amd64.00SHA00.dll",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos_c.windows-amd64.00SHA00." +
					"dll CONTENTS env=[] args=bazel build //pkg/cmd/cockroach //c-deps:libgeos //pkg/cmd/cockroach-sql " +
					"'--workspace_status_command=./build/bazelutil/stamp.sh x86_64-w64-mingw32 official-binary' -c opt --config=ci --config=with_ui --config=crosswindowsbase",
				"gs://edge-binaries-bucket/cockroach/lib/libgeos_c.windows-amd64.dll.LATEST/no-cache REDIRECT /cockroach/lib/libgeos_c.windows-amd64.00SHA00.dll",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, cleanup := testutils.TempDir(t)
			defer cleanup()

			var gcs mockStorage
			gcs.bucket = "edge-binaries-bucket"
			if test.flags.isRelease {
				gcs.bucket = "release-binaries-bucket"
			}
			var runner mockExecRunner
			fakeBazelBin, cleanup := testutils.TempDir(t)
			defer cleanup()
			runner.fakeBazelBin = fakeBazelBin
			flags := test.flags
			flags.pkgDir = dir
			execFn := release.ExecFn{MockExecFn: runner.run}
			run([]release.ObjectPutGetter{&gcs}, flags, execFn)
			require.Equal(t, test.expectedCmds, runner.cmds)
			require.Equal(t, test.expectedGets, gcs.gets)
			require.Equal(t, test.expectedPuts, gcs.puts)
		})
	}
}

func TestBless(t *testing.T) {
	tests := []struct {
		name         string
		flags        runFlags
		expectedGets []string
		expectedPuts []string
	}{
		{
			name: "testing",
			flags: runFlags{
				doBless:   true,
				isRelease: true,
				branch:    `provisional_201901010101_v0.0.1-alpha`,
			},
			expectedGets: nil,
			expectedPuts: nil,
		},
		{
			name: "stable",
			flags: runFlags{
				doBless:   true,
				isRelease: true,
				branch:    `provisional_201901010101_v0.0.1`,
			},
			expectedGets: nil,
			expectedPuts: []string{
				"gs://release-binaries-bucket/cockroach-latest.linux-amd64.tgz/no-cache " +
					"REDIRECT /cockroach-v0.0.1.linux-amd64.tgz",
				"gs://release-binaries-bucket/cockroach-latest.linux-amd64.tgz.sha256sum/no-cache " +
					"REDIRECT /cockroach-v0.0.1.linux-amd64.tgz.sha256sum",
				"gs://release-binaries-bucket/cockroach-latest.darwin-10.9-amd64.tgz/no-cache " +
					"REDIRECT /cockroach-v0.0.1.darwin-10.9-amd64.tgz",
				"gs://release-binaries-bucket/cockroach-latest.darwin-10.9-amd64.tgz.sha256sum/no-cache " +
					"REDIRECT /cockroach-v0.0.1.darwin-10.9-amd64.tgz.sha256sum",
				"gs://release-binaries-bucket/cockroach-latest.windows-6.2-amd64.zip/no-cache " +
					"REDIRECT /cockroach-v0.0.1.windows-6.2-amd64.zip",
				"gs://release-binaries-bucket/cockroach-latest.windows-6.2-amd64.zip.sha256sum/no-cache " +
					"REDIRECT /cockroach-v0.0.1.windows-6.2-amd64.zip.sha256sum",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gs mockStorage
			gs.bucket = "release-binaries-bucket"
			var execFn release.ExecFn // bless shouldn't exec anything
			run([]release.ObjectPutGetter{&gs}, test.flags, execFn)
			require.Equal(t, test.expectedGets, gs.gets)
			require.Equal(t, test.expectedPuts, gs.puts)
		})
	}
}
