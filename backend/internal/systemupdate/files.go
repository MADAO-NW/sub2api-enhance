package systemupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type BuildInfo struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	Date         string `json:"date"`
	BuildType    string `json:"build_type"`
	SchemaDigest string `json:"schema_digest"`
}

// acquireLock 与安装脚本共用同一文件锁，防止网页更新和命令行升级互相覆盖。
func acquireLock(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, ".update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("更新目录不可写，请检查脚本安装权限")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("已有安装、升级或恢复正在执行")
	}
	return file, nil
}
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// atomicWrite 将记录或备份先完整写入同目录，再替换目标，避免中途退出留下半个文件。
func atomicWrite(path string, mode os.FileMode, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".enhance-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func writeJSON(path string, value any) error {
	return atomicWrite(path, 0600, func(w io.Writer) error { return json.NewEncoder(w).Encode(value) })
}
func copyBinary(src, dst string) error {
	return atomicWrite(dst, 0755, func(w io.Writer) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
}

func verifyChecksum(path, name string, data []byte) error {
	actual, err := fileHash(path)
	if err != nil {
		return err
	}
	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) == 2 && strings.TrimPrefix(parts[1], "*") == name {
			if found || parts[0] != actual {
				return errors.New("发布包 SHA256 不匹配或校验项重复")
			}
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("checksums.txt 缺少 %s", name)
	}
	return nil
}

// extractRelease 只提取精确命名的常规二进制与版本信息，拒绝链接、路径穿越、重复文件和膨胀包。
func extractRelease(archive, dir string) (BuildInfo, error) {
	var info BuildInfo
	f, err := os.Open(archive)
	if err != nil {
		return info, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return info, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return info, err
		}
		if h.Size < 0 || h.Size > maxDownloadSize-total {
			return info, errors.New("发布包解压体积超过限制")
		}
		total += h.Size
		name := strings.TrimPrefix(h.Name, "./")
		if strings.Contains(name, "..") || filepath.IsAbs(name) || strings.Contains(name, "\\") {
			return info, errors.New("发布包含不安全路径")
		}
		if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
			return info, errors.New("发布包不允许符号链接或硬链接")
		}
		if name != "sub2api-enhance" && name != "release.json" {
			continue
		}
		if h.Typeflag != tar.TypeReg || seen[name] {
			return info, errors.New("发布包文件类型无效或重复")
		}
		seen[name] = true
		if name == "release.json" {
			if err := json.NewDecoder(tr).Decode(&info); err != nil {
				return info, err
			}
		} else {
			if err := atomicWrite(filepath.Join(dir, name), 0755, func(w io.Writer) error { _, err := io.Copy(w, tr); return err }); err != nil {
				return info, err
			}
		}
	}
	if !seen["release.json"] || !seen["sub2api-enhance"] || info.BuildType != "release" || !releaseTagPattern.MatchString("v"+info.Version) {
		return info, errors.New("发布包缺少有效二进制或版本信息")
	}
	digest, err := hex.DecodeString(info.SchemaDigest)
	if err != nil || len(digest) != sha256.Size {
		return info, errors.New("发布包迁移指纹无效")
	}
	return info, nil
}

// validateBinary 拒绝与当前 Linux 架构不匹配的发布文件，避免将错误平台包替换为服务程序。
func validateBinary(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var header [20]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return errors.New("发布二进制头不完整")
	}
	if string(header[:4]) != "\x7fELF" || header[4] != 2 || header[5] != 1 {
		return errors.New("发布文件不是受支持的 Linux ELF64")
	}
	expected := uint16(62)
	if runtime.GOARCH == "arm64" {
		expected = 183
	} else if runtime.GOARCH != "amd64" {
		return errors.New("当前架构不支持在线更新")
	}
	if binary.LittleEndian.Uint16(header[18:20]) != expected {
		return errors.New("发布二进制架构不匹配")
	}
	return nil
}
