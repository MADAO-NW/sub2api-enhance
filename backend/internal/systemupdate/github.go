package systemupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repository 是该增强项目唯一的发布来源，不继承原 sub2api 的更新仓库。
const Repository = "MADAO-NW/sub2api-enhance"

// maxDownloadSize 沿用原版更新器的单包和二进制 500 MiB 边界。
const maxDownloadSize int64 = 500 * 1024 * 1024

// releaseTagPattern 与发布流水线共同限定稳定版本标签。
var releaseTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Asset struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	BrowserURL string `json:"browser_download_url"`
}
type Release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	Body       string  `json:"body"`
	HTMLURL    string  `json:"html_url"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}
type githubClient struct {
	http  *http.Client
	token string
}

func newGitHubClient(token string) *githubClient {
	return &githubClient{token: token, http: &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("GitHub 下载跳转过多")
		}
		if err := trustedDownloadURL(req.URL); err != nil {
			return err
		}
		// 私有仓库凭据只发送到 GitHub API，绝不带给 Release CDN 或其他域名。
		req.Header.Del("Authorization")
		return nil
	}}}
}
func trustedDownloadURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return errors.New("更新下载必须使用可信 HTTPS 地址")
	}
	switch u.Host {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return nil
	}
	return errors.New("更新下载地址不在 GitHub 允许范围")
}
func (c *githubClient) request(ctx context.Context, address, accept string) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, errors.New("GitHub 地址无效")
	}
	if err := trustedDownloadURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "sub2api-enhance-updater")
	if u.Host == "api.github.com" && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("GitHub 请求失败，请检查网络或 UPDATE_GITHUB_TOKEN")
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("GitHub 返回 HTTP %d；请检查版本是否已发布及仓库权限", res.StatusCode)
	}
	return res, nil
}
func (c *githubClient) latest(ctx context.Context) (Release, error) {
	var release Release
	res, err := c.request(ctx, "https://api.github.com/repos/"+Repository+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return release, err
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&release); err != nil {
		return release, errors.New("GitHub 发布信息无法解析")
	}
	if release.Draft || release.Prerelease || !releaseTagPattern.MatchString(release.Tag) {
		return release, errors.New("发布版本不是有效稳定标签")
	}
	if release.HTMLURL != "https://github.com/"+Repository+"/releases/tag/"+release.Tag {
		return release, errors.New("发布页面不属于增强仓库")
	}
	return release, nil
}
func (c *githubClient) download(ctx context.Context, a Asset, dst io.Writer) error {
	u, err := url.Parse(a.URL)
	if err != nil {
		return errors.New("发布资源 URL 无效")
	}
	prefix := "/repos/" + Repository + "/releases/assets/"
	if u.Scheme != "https" || u.Host != "api.github.com" || u.User != nil || u.RawQuery != "" || !strings.HasPrefix(u.Path, prefix) {
		return errors.New("发布资源不属于增强仓库")
	}
	if id, err := strconv.ParseInt(strings.TrimPrefix(u.Path, prefix), 10, 64); err != nil || id <= 0 {
		return errors.New("发布资源 ID 无效")
	}
	res, err := c.request(ctx, a.URL, "application/octet-stream")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	n, err := io.Copy(dst, io.LimitReader(res.Body, maxDownloadSize+1))
	if err != nil {
		return errors.New("发布资源未完整下载")
	}
	if n > maxDownloadSize {
		return errors.New("发布资源超过原版更新器的 500 MiB 边界")
	}
	return nil
}
func newer(current, latest string) bool {
	if !releaseTagPattern.MatchString("v" + current) {
		return false
	}
	a := strings.Split(current, ".")
	b := strings.Split(strings.TrimPrefix(latest, "v"), ".")
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) < len(b[i])
		}
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
