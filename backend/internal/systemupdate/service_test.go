package systemupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func archiveFixture(t *testing.T, info BuildInfo, name string, kind byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	data := make([]byte, 64)
	copy(data, "\x7fELF")
	data[4] = 2
	data[5] = 1
	machine := uint16(62)
	if runtime.GOARCH == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:20], machine)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Typeflag: kind, Size: int64(len(data))}))
	require.NoError(t, writeTarData(tw, data))
	meta, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "release.json", Mode: 0644, Size: int64(len(meta))}))
	require.NoError(t, writeTarData(tw, meta))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return out.Bytes()
}
func writeTarData(w io.Writer, data []byte) error { _, err := w.Write(data); return err }

func TestUpdateVerifiesArchiveBeforeReplacementAndCanRestoreBackup(t *testing.T) {
	for _, badChecksum := range []bool{false, true} {
		t.Run(fmt.Sprint(badChecksum), func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "sub2api-enhance")
			require.NoError(t, os.WriteFile(exe, []byte("original executable"), 0755))
			current := BuildInfo{Version: "1.0.0", BuildType: "release", SchemaDigest: strings.Repeat("a", 64)}
			next := current
			next.Version = "1.1.0"
			archive := archiveFixture(t, next, "sub2api-enhance", tar.TypeReg)
			name := fmt.Sprintf("sub2api-enhance_1.1.0_linux_%s.tar.gz", runtime.GOARCH)
			sum := fmt.Sprintf("%x", sha256.Sum256(archive))
			if badChecksum {
				sum = strings.Repeat("0", 64)
			}
			release := Release{Tag: "v1.1.0", HTMLURL: "https://github.com/" + Repository + "/releases/tag/v1.1.0", Assets: []Asset{{Name: name, URL: "https://api.github.com/repos/" + Repository + "/releases/assets/1"}, {Name: "checksums.txt", URL: "https://api.github.com/repos/" + Repository + "/releases/assets/2"}}}
			releaseRaw, _ := json.Marshal(release)
			s, err := New(current, exe, "unit-github-token", false, nil)
			require.NoError(t, err)
			s.github.http.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, "api.github.com", r.URL.Host)
				require.Equal(t, "Bearer unit-github-token", r.Header.Get("Authorization"))
				var body []byte
				switch {
				case strings.HasSuffix(r.URL.Path, "/latest"):
					body = releaseRaw
				case strings.HasSuffix(r.URL.Path, "/1"):
					body = archive
				case strings.HasSuffix(r.URL.Path, "/2"):
					body = []byte(sum + "  " + name + "\n")
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})
			state := State{}
			applied, err := s.apply(context.Background(), "update", &state)
			actual, readErr := os.ReadFile(exe)
			require.NoError(t, readErr)
			if badChecksum {
				require.Error(t, err)
				require.False(t, applied)
				require.Equal(t, "original executable", string(actual))
				return
			}
			require.NoError(t, err)
			require.True(t, applied)
			require.Equal(t, "\x7fELF", string(actual[:4]))
			require.Equal(t, "1.0.0", state.Backup.Version)
			state.Phase = "ready"
			require.NoError(t, s.saveState(state))
			restarted, err := New(next, exe, "", false, nil)
			require.NoError(t, err)
			require.Equal(t, "completed", restarted.Status().State.Phase)
			rollbackState := state
			rollbackState.TargetVersion = "1.0.0"
			applied, err = restarted.apply(context.Background(), "rollback", &rollbackState)
			require.NoError(t, err)
			require.True(t, applied)
			restored, err := os.ReadFile(exe)
			require.NoError(t, err)
			require.Equal(t, "original executable", string(restored))
		})
	}
}
func TestRollbackRejectsDifferentMigrationsWithoutChangingBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sub2api-enhance")
	require.NoError(t, os.WriteFile(exe, []byte("current"), 0755))
	s, err := New(BuildInfo{SchemaDigest: "new"}, exe, "", false, nil)
	require.NoError(t, err)
	applied, err := s.apply(context.Background(), "rollback", &State{Backup: &BuildInfo{SchemaDigest: "old"}})
	require.ErrorContains(t, err, "迁移集合")
	require.False(t, applied)
	raw, _ := os.ReadFile(exe)
	require.Equal(t, "current", string(raw))
}
func TestArchiveTraversalAndForeignRepositoryAssetsAreRejected(t *testing.T) {
	dir := t.TempDir()
	info := BuildInfo{Version: "1.0.0", BuildType: "release", SchemaDigest: strings.Repeat("a", 64)}
	archive := filepath.Join(dir, "bad.tar.gz")
	require.NoError(t, os.WriteFile(archive, archiveFixture(t, info, "../sub2api-enhance", tar.TypeReg), 0600))
	_, err := extractRelease(archive, dir)
	require.ErrorContains(t, err, "不安全路径")
	c := newGitHubClient("token")
	err = c.download(context.Background(), Asset{URL: "https://api.github.com/repos/other/repo/releases/assets/1"}, io.Discard)
	require.ErrorContains(t, err, "不属于")
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "release-assets.githubusercontent.com"}, Header: http.Header{"Authorization": []string{"Bearer unit-token"}}}
	require.NoError(t, c.http.CheckRedirect(req, nil))
	require.Empty(t, req.Header.Get("Authorization"))
	req.URL = &url.URL{Scheme: "https", Host: "github.com.evil.example"}
	require.Error(t, c.http.CheckRedirect(req, nil))
}
func TestSharedLockPreventsWebAndScriptUpdatesFromOverlapping(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireLock(dir)
	require.NoError(t, err)
	_, err = acquireLock(dir)
	require.ErrorContains(t, err, "已有")
	require.NoError(t, first.Close())
	second, err := acquireLock(dir)
	require.NoError(t, err)
	second.Close()
}
func TestSourceBuildCannotUpdateAndRestartUsesCallbackOnly(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sub2api-enhance")
	require.NoError(t, os.WriteFile(exe, []byte("source"), 0755))
	restarted := make(chan struct{}, 1)
	s, err := New(BuildInfo{Version: "dev", BuildType: "source"}, exe, "", false, func() { restarted <- struct{}{} })
	require.NoError(t, err)
	require.Error(t, s.Start("update", 7, "1.1.0"))
	require.Error(t, s.Restart(7))
	s.supported = true
	s.managed = true
	require.NoError(t, s.Restart(7))
	require.Error(t, s.Start("update", 7, "1.1.0"))
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback was not invoked")
	}
}
func TestStableVersionOrdering(t *testing.T) {
	require.True(t, newer("1.9.0", "v1.10.0"))
	require.False(t, newer("dev", "v1.10.0"))
	require.False(t, newer("1.10.0", "v1.9.0"))
	require.False(t, releaseTagPattern.MatchString("v01.2.3"))
}

func TestBackgroundUpdateIsExclusiveAndPersistsFailureWithoutReplacingProgram(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sub2api-enhance")
	require.NoError(t, os.WriteFile(exe, []byte("unchanged"), 0755))
	s, err := New(BuildInfo{Version: "1.0.0", BuildType: "release"}, exe, "", false, nil)
	require.NoError(t, err)
	s.supported = true
	entered := make(chan struct{})
	release := make(chan struct{})
	s.github.http.Transport = fakeTransport(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("not published")), Header: make(http.Header)}, nil
	})
	require.NoError(t, s.Start("update", 7, "1.1.0"))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("update did not start")
	}
	require.ErrorContains(t, s.Start("update", 7, "1.1.0"), "已有")
	close(release)
	require.Eventually(t, func() bool { return s.Status().State.Phase == "failed" }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		if s.operation.TryLock() {
			s.operation.Unlock()
			return true
		}
		return false
	}, time.Second, time.Millisecond)
	raw, err := os.ReadFile(filepath.Join(dir, ".update-state.json"))
	require.NoError(t, err)
	var state State
	require.NoError(t, json.Unmarshal(raw, &state))
	require.Equal(t, int64(7), state.Actor)
	require.Contains(t, state.Error, "404")
	contents, _ := os.ReadFile(exe)
	require.Equal(t, "unchanged", string(contents))
}

func TestReleaseChangeRequiresFreshAdministratorConfirmation(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sub2api-enhance")
	require.NoError(t, os.WriteFile(exe, []byte("current"), 0755))
	s, err := New(BuildInfo{Version: "1.0.0", BuildType: "release"}, exe, "", false, nil)
	require.NoError(t, err)
	s.github.http.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/latest"))
		raw, _ := json.Marshal(Release{Tag: "v1.2.0", HTMLURL: "https://github.com/" + Repository + "/releases/tag/v1.2.0"})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})
	applied, err := s.apply(context.Background(), "update", &State{TargetVersion: "1.1.0"})
	require.False(t, applied)
	require.ErrorContains(t, err, "已经变化")
	contents, _ := os.ReadFile(exe)
	require.Equal(t, "current", string(contents))
}
