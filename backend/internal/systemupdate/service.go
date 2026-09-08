package systemupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// updateCheckTTL 沿用原版二十分钟更新检查缓存，避免频繁消耗 GitHub API 配额。
const updateCheckTTL = 20 * time.Minute

type State struct {
	Phase         string     `json:"phase"`
	Action        string     `json:"action"`
	Actor         int64      `json:"actor_user_id,omitempty"`
	TargetVersion string     `json:"target_version"`
	TargetHash    string     `json:"target_hash"`
	Backup        *BuildInfo `json:"backup,omitempty"`
	BackupHash    string     `json:"backup_hash,omitempty"`
	Error         string     `json:"error"`
	UpdatedAt     time.Time  `json:"updated_at"`
}
type Status struct {
	StartedAt       time.Time `json:"started_at"`
	Current         BuildInfo `json:"current"`
	Repository      string    `json:"repository"`
	Supported       bool      `json:"supported"`
	Managed         bool      `json:"managed"`
	RestartRequired bool      `json:"restart_required"`
	CanRollback     bool      `json:"can_rollback"`
	State           State     `json:"state"`
}
type Check struct {
	CurrentVersion string  `json:"current_version"`
	LatestVersion  string  `json:"latest_version"`
	HasUpdate      bool    `json:"has_update"`
	Release        Release `json:"release"`
	Cached         bool    `json:"cached"`
}
type Service struct {
	startedAt  time.Time
	info       BuildInfo
	executable string
	managed    bool
	supported  bool
	restart    func()
	github     *githubClient
	mu         sync.Mutex
	operation  sync.Mutex
	state      State
	latest     *Check
	checkedAt  time.Time
	restarting bool
}

func New(info BuildInfo, executable, token string, managed bool, restart func()) (*Service, error) {
	path, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	s := &Service{startedAt: time.Now().UTC(), info: info, executable: path, github: newGitHubClient(token), managed: managed, supported: info.BuildType == "release" && runtime.GOOS == "linux" && filepath.Base(path) == "sub2api-enhance", restart: restart, state: State{Phase: "idle"}}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".update-state.json"))
	if err == nil {
		if err := json.Unmarshal(raw, &s.state); err != nil {
			s.supported = false
			s.state = State{Phase: "failed", Error: "更新记录损坏，请检查 .update-state.json；业务模块仍可运行"}
			return s, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		s.supported = false
		s.state = State{Phase: "failed", Error: "更新记录不可读取，请检查安装目录权限"}
		return s, nil
	}
	if s.state.TargetHash != "" {
		if hash, err := fileHash(path); err == nil && hash == s.state.TargetHash {
			if info.Version == s.state.TargetVersion {
				s.state.Phase = "completed"
			} else {
				s.state.Phase = "ready"
			}
		} else if s.state.Phase == "applying" || s.state.Phase == "ready" {
			s.state.Phase = "failed"
			s.state.Error = "上次替换未确认或安装文件已变化，请核对后重试"
		}
	}
	if s.state.Phase == "downloading" {
		s.state.Phase = "failed"
		s.state.Error = "上次下载被中断，可以重新检查更新"
	}
	return s, nil
}
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	supported := s.supported
	return Status{StartedAt: s.startedAt, Current: s.info, Repository: Repository, Supported: supported, Managed: s.managed, RestartRequired: s.state.Phase == "ready" && s.state.TargetVersion != s.info.Version, CanRollback: supported && s.state.Backup != nil && s.state.Backup.SchemaDigest == s.info.SchemaDigest, State: s.state}
}
func (s *Service) Check(ctx context.Context, force bool) (Check, error) {
	s.mu.Lock()
	if !force && s.latest != nil && time.Since(s.checkedAt) < updateCheckTTL {
		out := *s.latest
		out.Cached = true
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()
	release, err := s.github.latest(ctx)
	if err != nil {
		return Check{}, err
	}
	out := Check{CurrentVersion: s.info.Version, LatestVersion: strings.TrimPrefix(release.Tag, "v"), HasUpdate: newer(s.info.Version, release.Tag), Release: release}
	s.mu.Lock()
	s.latest = &out
	s.checkedAt = time.Now()
	s.mu.Unlock()
	return out, nil
}

// saveState 将操作进度同时保存在内存和安装目录；失败时不把已替换的文件误报为未执行。
func (s *Service) saveState(state State) error {
	state.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	return writeJSON(filepath.Join(filepath.Dir(s.executable), ".update-state.json"), state)
}

// Start 在管理员请求中领取唯一更新锁，后台执行不随浏览器断连而取消。
func (s *Service) Start(action string, actor int64, version string) error {
	if action != "update" && action != "rollback" {
		return errors.New("更新动作无效")
	}
	if !releaseTagPattern.MatchString("v" + version) {
		return errors.New("必须提供已确认的稳定版本号")
	}
	if !s.operation.TryLock() {
		return errors.New("已有更新操作正在执行")
	}
	status := s.Status()
	if !status.Supported {
		s.operation.Unlock()
		return errors.New("仅脚本部署的 Linux Release 支持在线更新")
	}
	lock, err := acquireLock(filepath.Dir(s.executable))
	if err != nil {
		s.operation.Unlock()
		return err
	}
	s.mu.Lock()
	restarting := s.restarting
	s.mu.Unlock()
	if restarting || (status.RestartRequired && action == "update") {
		lock.Close()
		s.operation.Unlock()
		return errors.New("已应用的版本等待重启，不能再次覆盖")
	}
	state := status.State
	state.Phase = "downloading"
	state.Action = action
	state.Actor = actor
	state.Error = ""
	state.TargetVersion = version
	state.TargetHash = ""
	if err := s.saveState(state); err != nil {
		lock.Close()
		s.operation.Unlock()
		return err
	}
	go func() {
		defer lock.Close()
		defer s.operation.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		log.Printf("开始增强服务版本操作 action=%s actor_user_id=%d current_version=%s", action, actor, s.info.Version)
		applied, err := s.apply(ctx, action, &state)
		state.Phase = "failed"
		if applied {
			state.Phase = "ready"
			if state.TargetVersion == s.info.Version {
				state.Phase = "completed"
			}
		}
		if err != nil {
			state.Error = err.Error()
		}
		if saveErr := s.saveState(state); saveErr != nil {
			log.Printf("增强版本状态保存失败：%v", saveErr)
		}
		log.Printf("增强服务版本操作结束 action=%s actor_user_id=%d from=%s to=%s 文件已替换=%t error=%s", action, actor, s.info.Version, state.TargetVersion, applied, state.Error)
	}()
	return nil
}

// apply 适配原版下载校验和同目录替换流程；先完整备份，保证替换时始终存在可启动文件。
func (s *Service) apply(ctx context.Context, action string, state *State) (bool, error) {
	dir := filepath.Dir(s.executable)
	stage, err := os.MkdirTemp(dir, ".sub2api-enhance-update-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	candidate := filepath.Join(stage, "sub2api-enhance")
	if action == "rollback" {
		if state.Backup == nil || state.Backup.SchemaDigest != s.info.SchemaDigest {
			return false, errors.New("备份迁移集合与当前版本不同，不能自动回退数据库；请采用修复版本或经确认的数据库恢复流程")
		}
		if state.TargetVersion != "" && state.TargetVersion != state.Backup.Version {
			return false, errors.New("备份版本已经变化，请重新确认")
		}
		backup := s.executable + ".backup"
		hash, err := fileHash(backup)
		if err != nil || hash != state.BackupHash {
			return false, errors.New("备份文件缺失或校验不一致")
		}
		if err := copyBinary(backup, candidate); err != nil {
			return false, err
		}
		state.TargetVersion = state.Backup.Version
		state.TargetHash = hash
	} else {
		check, err := s.Check(ctx, true)
		if err != nil {
			return false, err
		}
		if !check.HasUpdate {
			return false, errors.New("当前已是最新稳定版本")
		}
		if state.TargetVersion != "" && state.TargetVersion != check.LatestVersion {
			return false, errors.New("最新发布版本已经变化，请重新检查并确认")
		}
		name := fmt.Sprintf("sub2api-enhance_%s_linux_%s.tar.gz", check.LatestVersion, runtime.GOARCH)
		var archive, checksums *Asset
		for i := range check.Release.Assets {
			a := &check.Release.Assets[i]
			if a.Name == name {
				if archive != nil {
					return false, errors.New("发布包名称重复")
				}
				archive = a
			}
			if a.Name == "checksums.txt" {
				if checksums != nil {
					return false, errors.New("校验文件重复")
				}
				checksums = a
			}
		}
		if archive == nil || checksums == nil {
			return false, errors.New("发布缺少当前架构安装包或 checksums.txt，未替换程序")
		}
		path := filepath.Join(stage, name)
		out, err := os.Create(path)
		if err != nil {
			return false, err
		}
		err = s.github.download(ctx, *archive, out)
		closeErr := out.Close()
		if err != nil {
			return false, err
		}
		if closeErr != nil {
			return false, closeErr
		}
		var checksumData bytes.Buffer
		if err := s.github.download(ctx, *checksums, &checksumData); err != nil {
			return false, err
		}
		if err := verifyChecksum(path, name, checksumData.Bytes()); err != nil {
			return false, err
		}
		info, err := extractRelease(path, stage)
		if err != nil {
			return false, err
		}
		if info.Version != check.LatestVersion {
			return false, errors.New("包内版本与 GitHub 标签不同")
		}
		if err := validateBinary(candidate); err != nil {
			return false, err
		}
		if err := copyBinary(s.executable, s.executable+".backup"); err != nil {
			return false, err
		}
		backupHash, err := fileHash(s.executable + ".backup")
		if err != nil {
			return false, err
		}
		state.Backup = &s.info
		state.BackupHash = backupHash
		state.TargetVersion = info.Version
		state.TargetHash, err = fileHash(candidate)
		if err != nil {
			return false, err
		}
	}
	state.Phase = "applying"
	if err := s.saveState(*state); err != nil {
		return false, err
	}
	// 同文件系统 rename 直接替换当前路径，避免原版先移走当前文件留下的空档。
	if err := os.Rename(candidate, s.executable); err != nil {
		return false, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return true, err
	}
	defer d.Close()
	return true, d.Sync()
}

// Restart 只请求本进程按现有 shutdown 流程退出，由 systemd Restart=always 拉起，不能管理原版进程。
func (s *Service) Restart(actor int64) error {
	status := s.Status()
	if !status.Supported || !s.managed || s.restart == nil {
		return errors.New("当前不是 systemd 管理的 Release，请由部署人员重启增强服务")
	}
	if !s.operation.TryLock() {
		return errors.New("版本操作仍在执行，暂不能重启")
	}
	lock, err := acquireLock(filepath.Dir(s.executable))
	if err != nil {
		s.operation.Unlock()
		return err
	}
	s.mu.Lock()
	if s.restarting {
		s.mu.Unlock()
		lock.Close()
		s.operation.Unlock()
		return errors.New("增强服务正在重启")
	}
	s.restarting = true
	s.mu.Unlock()
	go func() {
		defer lock.Close()
		defer s.operation.Unlock()
		time.Sleep(500 * time.Millisecond)
		log.Printf("管理员请求增强服务重启 actor_user_id=%d", actor)
		s.restart()
	}()
	return nil
}
